package clone

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klauspost/compress/zstd"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/settings"
	"github.com/servicelater/servicelater/backend/internal/slatimer"
	"github.com/servicelater/servicelater/backend/internal/snapshot"
)

// Loading a package into this instance (the target), docs/backup-and-clone.md §2 "Applying a clone": the target's
// records are replaced by the package's; what the target keeps of its own (settings, secrets, single sign-on, the
// people it keeps on a clone, its studio layer) is put aside, matched to the package's people and groups by natural
// key with the package's id winning, and put back. All in one transaction: a load that fails changes nothing.

// emptied are tables a load empties though no package holds them: sign-in state, queued work and counters, and the
// schedules' marks (schedules start counting from the clone). api_keys, user_identities, sso_group_maps and
// clone_keep_people are emptied and put back, moved to the package's ids.
var emptied = []string{"sessions", "jobs", "rate_limits", "rate_counts", "workflow_schedules",
	"api_keys", "user_identities", "sso_group_maps", "clone_keep_people"}

// KeptPerson is one of the target's own people and what a load does with them.
type KeptPerson struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	// Outcome is matched (production has them: they take production's id, with this instance's password, roles,
	// groups, phone and keys), added (production does not: they are added as they are), or not_kept (Detail says why).
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// SetAside is a studio edit the target had that could not be kept: its content is here, so it is never lost.
type SetAside struct {
	App     string `json:"app"`
	Path    string `json:"path"`
	Reason  string `json:"reason"`
	Content string `json:"content"`
}

// LoadReport is what a load brought and what the target kept.
type LoadReport struct {
	Source Header `json:"source"`
	// Package is the report made with the package on the source.
	Package Report           `json:"package"`
	Rows    map[string]int64 `json:"rows"`
	People  []KeptPerson     `json:"people"`
	// Dropped are what kept people had that could not follow: groups and roles production does not have.
	Dropped []string `json:"dropped"`
	// Identities are single sign-on links moved to production's people; IdentitiesDropped those of people it does
	// not have.
	Identities        int64      `json:"identities"`
	IdentitiesDropped int64      `json:"identities_dropped"`
	APIKeys           int64      `json:"api_keys"`
	StudioKept        []string   `json:"studio_kept"`
	SetAside          []SetAside `json:"set_aside"`
	RunsCancelled     int64      `json:"runs_cancelled"`
	ApprovalsClosed   int64      `json:"approvals_closed"`
	ClocksSettled     int        `json:"clocks_settled"`
	// SwitchedOff are the settings the safety switch turned off (they were on); the holds are always set.
	SwitchedOff []string `json:"switched_off"`
	Files       int      `json:"files"`
	// MissingFiles are attachments that came without their file.
	MissingFiles int64 `json:"missing_files"`
}

// LoadOptions are what Load and Inspect take.
type LoadOptions struct {
	Passphrase string
	// KeepPeople keeps the people on the keep list (clone_keep_people); Applier, the person loading it, is kept
	// either way, so they can still sign in.
	KeepPeople bool
	Applier    uuid.UUID
	// KeepStudio keeps this instance's studio layer (workflow, form and catalog edits) over production's.
	KeepStudio bool
	// PutFile stores an attachment's file in this instance's storage and says in which backend (Load only). It runs
	// while the load's transaction holds its locks, so it must not touch the database (choose the store beforehand).
	PutFile func(ctx context.Context, key string, size int64, r io.Reader) (backend string, err error)
}

// LoadPreview is what loading a package would do, worked out without changing anything.
type LoadPreview struct {
	Contents
	Report     Report           `json:"report"`
	Rows       map[string]int64 `json:"rows"`
	SchemaHere int64            `json:"schema_here"`
	People     []KeptPerson     `json:"people"`
	Dropped    []string         `json:"dropped"`
	StudioKept []string         `json:"studio_kept"`
	SetAside   []SetAside       `json:"set_aside"`
	// Problems stop the load (a package from a newer version, tables this instance does not have).
	Problems []string `json:"problems"`
}

// open reads a package's header and starts decrypting it.
func open(r io.Reader, passphrase string) (Header, *tar.Reader, func(), error) {
	br := bufio.NewReader(r)
	head, err := ReadHeader(br)
	if err != nil {
		return head, nil, nil, err
	}
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return head, nil, nil, err
	}
	dec, err := age.Decrypt(br, id)
	if err != nil {
		var wrong *age.NoIdentityMatchError
		if errors.As(err, &wrong) {
			return head, nil, nil, ErrPassphrase
		}
		return head, nil, nil, fmt.Errorf("the package cannot be decrypted: %w", err)
	}
	zr, err := zstd.NewReader(dec)
	if err != nil {
		return head, nil, nil, err
	}
	return head, tar.NewReader(zr), zr.Close, nil
}

// fits says what stops a package's tables loading here: each must be one a clone may fill, and have no column this
// database lacks (an older package has fewer columns; those it lacks take their defaults).
func fits(pkg []snapshot.Table, here []snapshot.Table) []string {
	byName := map[string]snapshot.Table{}
	for _, t := range here {
		byName[t.Name] = t
	}
	var bad []string
	for _, t := range pkg {
		have, ok := byName[t.Name]
		switch {
		case slices.Contains(never, t.Name) || slices.Contains(snapshot.Kept(), t.Name):
			bad = append(bad, fmt.Sprintf("the package holds %s, which a clone never copies", t.Name))
		case !ok:
			bad = append(bad, fmt.Sprintf("the package has a table %s this instance does not: update this instance first", t.Name))
		default:
			for _, c := range t.Columns {
				if !slices.Contains(have.Columns, c) {
					bad = append(bad, fmt.Sprintf("the package's %s has a column %s this instance does not: update this instance first", t.Name, c))
				}
			}
		}
	}
	return bad
}

// ---- people ----

type person struct {
	id             uuid.UUID
	email, name    string
	app            string
	serviceAccount bool
}

func readPeople(ctx context.Context, tx pgx.Tx, users, serviceAccounts string) ([]person, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT u.id, coalesce(lower(trim(u.email)), ''), u.name, coalesce(u.app_name, ''),
		EXISTS (SELECT 1 FROM %s s WHERE s.user_id = u.id) FROM %s u`, serviceAccounts, users))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (person, error) {
		var p person
		return p, r.Scan(&p.id, &p.email, &p.name, &p.app, &p.serviceAccount)
	})
}

// matcher finds the package's person who is one of this instance's: people by address (by its salted hash when the
// package masked addresses), service accounts by name. Apps' principals come with the package and are not matched.
type matcher struct {
	salt                    string
	byEmail, byHash, byName map[string]uuid.UUID
}

func newMatcher(source []person, ids []Identity, salt string) matcher {
	m := matcher{salt: salt, byEmail: map[string]uuid.UUID{}, byHash: map[string]uuid.UUID{}, byName: map[string]uuid.UUID{}}
	for _, p := range source {
		switch {
		case p.serviceAccount:
			m.byName[strings.ToLower(p.name)] = p.id
		case p.app == "" && p.email != "" && salt == "":
			m.byEmail[p.email] = p.id
		}
	}
	for _, i := range ids {
		m.byHash[i.EmailHash] = i.ID
	}
	return m
}

func (m matcher) match(p person) (uuid.UUID, bool) {
	switch {
	case p.serviceAccount:
		id, ok := m.byName[strings.ToLower(p.name)]
		return id, ok
	case p.app != "" || p.email == "":
		return uuid.Nil, false
	case m.salt != "":
		id, ok := m.byHash[HashEmail(m.salt, p.email)]
		return id, ok
	}
	id, ok := m.byEmail[p.email]
	return id, ok
}

// keptIDs are the target's people a load keeps.
func keptIDs(ctx context.Context, tx pgx.Tx, o LoadOptions) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	if o.KeepPeople {
		rows, err := tx.Query(ctx, `SELECT user_id FROM clone_keep_people ORDER BY user_id`)
		if err != nil {
			return nil, err
		}
		if ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
			return nil, err
		}
	}
	if o.Applier != uuid.Nil && !slices.Contains(ids, o.Applier) {
		ids = append(ids, o.Applier)
	}
	return ids, nil
}

// ---- preview ----

// Inspect reads a whole package and works out what loading it would do, against this instance as it is now. It
// changes nothing: the package's people, groups and roles are read into temporary tables of a rolled-back transaction.
func Inspect(ctx context.Context, pool *pgxpool.Pool, r io.Reader, o LoadOptions) (*LoadPreview, error) {
	head, tr, done, err := open(r, o.Passphrase)
	if err != nil {
		return nil, err
	}
	defer done()
	pv := &LoadPreview{Rows: map[string]int64{}, People: []KeptPerson{}, Dropped: []string{}, StudioKept: []string{},
		SetAside: []SetAside{}, Problems: []string{}}
	if pv.SchemaHere, err = db.LatestVersion(); err != nil {
		return nil, err
	}
	if head.Schema > pv.SchemaHere {
		pv.Problems = append(pv.Problems, fmt.Sprintf("the package is from a newer version of ServiceLater (schema %d; this one is at %d): update this instance first", head.Schema, pv.SchemaHere))
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	here, err := snapshot.AllTables(ctx, tx, nil)
	if err != nil {
		return nil, err
	}

	wanted := []string{"users", "service_accounts", "groups", "roles"}
	for _, t := range wanted {
		if _, err := tx.Exec(ctx, fmt.Sprintf("CREATE TEMP TABLE clone_pkg_%s (LIKE %s) ON COMMIT DROP", t, t)); err != nil {
			return nil, err
		}
	}
	var ids []Identity
	var totals snapshot.Totals
	hashes := map[string]string{}
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "clone.json" {
		return nil, errors.New("the package does not start with its contents")
	}
	if err := json.NewDecoder(tr).Decode(&pv.Contents); err != nil {
		return nil, fmt.Errorf("the package's contents cannot be read: %w", err)
	}
	pv.Problems = append(pv.Problems, fits(pv.Tables, here)...)
	columns := map[string][]string{}
	for _, t := range pv.Tables {
		columns[t.Name] = t.Columns
	}
	hdr, err = tr.Next()
	for err == nil {
		switch {
		case strings.HasPrefix(hdr.Name, "tables/"):
			table := path.Dir(strings.TrimPrefix(hdr.Name, "tables/"))
			if slices.Contains(wanted, table) && columns[table] != nil {
				var n int64
				var sum string
				n, sum, hdr, err = snapshot.CopyIn(ctx, tx, tr, table, "clone_pkg_"+table, columns[table])
				pv.Rows[table], hashes[table] = n, sum
				continue
			}
			h := sha256.New()
			for err == nil && strings.HasPrefix(hdr.Name, "tables/"+table+"/") {
				if _, err = io.Copy(h, tr); err != nil { //nolint:gosec // age authenticates the stream
					return nil, err
				}
				hdr, err = tr.Next()
			}
			hashes[table] = hex.EncodeToString(h.Sum(nil))
			continue
		case hdr.Name == "identities.json":
			if err := json.NewDecoder(tr).Decode(&ids); err != nil {
				return nil, err
			}
		case hdr.Name == "report.json":
			if err := json.NewDecoder(tr).Decode(&pv.Report); err != nil {
				return nil, err
			}
		case hdr.Name == "totals.json":
			if err := json.NewDecoder(tr).Decode(&totals); err != nil {
				return nil, err
			}
		}
		hdr, err = tr.Next()
	}
	if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("reading the package: %w", err)
	}
	if totals.Rows == nil {
		return nil, errors.New("the package ends before its totals: it is incomplete")
	}
	for table, want := range totals.SHA256 {
		got := hashes[table]
		if got == "" {
			got = hex.EncodeToString(sha256.New().Sum(nil))
		}
		if got != want {
			return nil, fmt.Errorf("the table %s does not match the package's totals: the file is damaged", table)
		}
	}
	pv.Rows = totals.Rows

	source, err := readPeople(ctx, tx, "clone_pkg_users", "clone_pkg_service_accounts")
	if err != nil {
		return nil, err
	}
	target, err := readPeople(ctx, tx, "users", "service_accounts")
	if err != nil {
		return nil, err
	}
	m := newMatcher(source, ids, pv.IdentitySalt)
	keep, err := keptIDs(ctx, tx, o)
	if err != nil {
		return nil, err
	}
	for _, p := range target {
		if !slices.Contains(keep, p.id) {
			continue
		}
		k := KeptPerson{Name: p.name, Email: p.email, Outcome: "added"}
		if _, ok := m.match(p); ok {
			k.Outcome = "matched"
		}
		pv.People = append(pv.People, k)
	}
	rows, err := tx.Query(ctx, `
		SELECT u.name, 'group ' || g.name FROM group_members gm JOIN groups g ON g.id = gm.group_id JOIN users u ON u.id = gm.user_id
		WHERE gm.user_id = ANY ($1) AND NOT EXISTS (SELECT 1 FROM clone_pkg_groups p WHERE lower(p.name) = lower(g.name))
		UNION ALL
		SELECT u.name, 'role ' || r.role FROM role_grants r JOIN users u ON u.id = r.user_id
		WHERE r.user_id = ANY ($1) AND NOT EXISTS (SELECT 1 FROM clone_pkg_roles p WHERE p.name = r.role)
		ORDER BY 1, 2`, keep)
	if err != nil {
		return nil, err
	}
	if err := forEach(rows, func(r pgx.Rows) error {
		var who, what string
		if err := r.Scan(&who, &what); err != nil {
			return err
		}
		pv.Dropped = append(pv.Dropped, who+": "+what+" (production does not have it)")
		return nil
	}); err != nil {
		return nil, err
	}
	if o.KeepStudio {
		installed := map[string]bool{}
		for _, a := range pv.Apps {
			installed[a.Name] = true
		}
		rows, err := tx.Query(ctx, `SELECT app_name, path, content FROM app_overrides ORDER BY 1, 2`)
		if err != nil {
			return nil, err
		}
		if err := forEach(rows, func(r pgx.Rows) error {
			var s SetAside
			if err := r.Scan(&s.App, &s.Path, &s.Content); err != nil {
				return err
			}
			if installed[s.App] {
				pv.StudioKept = append(pv.StudioKept, s.App+"/"+s.Path)
			} else {
				s.Reason = "production does not have the app"
				pv.SetAside = append(pv.SetAside, s)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return pv, nil
}

// ---- loading ----

// Load replaces this instance's records with a package's, in one transaction, keeping what the target keeps (see the
// top of this file). It does not reinstall apps over the kept studio layer: the caller does that once it is committed
// (ReinstallStudio).
func Load(ctx context.Context, pool *pgxpool.Pool, r io.Reader, o LoadOptions) (*LoadReport, error) {
	head, tr, done, err := open(r, o.Passphrase)
	if err != nil {
		return nil, err
	}
	defer done()
	latest, err := db.LatestVersion()
	if err != nil {
		return nil, err
	}
	if head.Schema > latest {
		return nil, fmt.Errorf("the package is from a newer version of ServiceLater (schema %d; this one is at %d): update this instance first", head.Schema, latest)
	}
	rep := &LoadReport{Source: head, Rows: map[string]int64{}, People: []KeptPerson{}, Dropped: []string{},
		StudioKept: []string{}, SetAside: []SetAside{}, SwitchedOff: []string{}}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return load(ctx, tx, tr, o, rep) })
	if err != nil {
		return nil, err
	}
	return rep, nil
}

func load(ctx context.Context, tx pgx.Tx, tr *tar.Reader, o LoadOptions, rep *LoadReport) error {
	// One restore or clone at a time.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('servicelater:snapshot-restore'))"); err != nil {
		return err
	}
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "clone.json" {
		return errors.New("the package does not start with its contents")
	}
	var contents Contents
	if err := json.NewDecoder(tr).Decode(&contents); err != nil {
		return fmt.Errorf("the package's contents cannot be read: %w", err)
	}
	here, err := snapshot.AllTables(ctx, tx, nil)
	if err != nil {
		return err
	}
	if bad := fits(contents.Tables, here); len(bad) > 0 {
		return errors.New(strings.Join(bad, "; "))
	}
	columns := map[string][]string{}
	for _, t := range contents.Tables {
		columns[t.Name] = t.Columns
	}
	var userColumns []string
	for _, t := range here {
		if t.Name == "users" {
			userColumns = t.Columns
		}
	}

	// Put aside what the target keeps.
	keep, err := keptIDs(ctx, tx, o)
	if err != nil {
		return err
	}
	for _, stmt := range []string{
		"CREATE TEMP TABLE clone_t_users ON COMMIT DROP AS SELECT * FROM users",
		"CREATE TEMP TABLE clone_t_sa ON COMMIT DROP AS SELECT * FROM service_accounts",
		"CREATE TEMP TABLE clone_t_credentials ON COMMIT DROP AS SELECT * FROM user_credentials WHERE user_id = ANY ($1)",
		"CREATE TEMP TABLE clone_t_phones ON COMMIT DROP AS SELECT * FROM user_phones WHERE user_id = ANY ($1)",
		"CREATE TEMP TABLE clone_t_grants ON COMMIT DROP AS SELECT * FROM role_grants WHERE user_id = ANY ($1)",
		`CREATE TEMP TABLE clone_t_members ON COMMIT DROP AS SELECT gm.user_id, g.name AS group_name, gm.added_at
			FROM group_members gm JOIN groups g ON g.id = gm.group_id WHERE gm.user_id = ANY ($1)`,
		"CREATE TEMP TABLE clone_t_keys ON COMMIT DROP AS SELECT * FROM api_keys WHERE user_id = ANY ($1)",
		"CREATE TEMP TABLE clone_t_identities ON COMMIT DROP AS SELECT * FROM user_identities",
		`CREATE TEMP TABLE clone_t_maps ON COMMIT DROP AS SELECT m.provider_id, m.claim_value, g.name AS group_name
			FROM sso_group_maps m JOIN groups g ON g.id = m.group_id`,
		"CREATE TEMP TABLE clone_t_overrides ON COMMIT DROP AS SELECT * FROM app_overrides",
		"CREATE TEMP TABLE clone_t_keeplist ON COMMIT DROP AS SELECT * FROM clone_keep_people",
		"CREATE TEMP TABLE clone_map (target uuid PRIMARY KEY, final uuid NOT NULL) ON COMMIT DROP",
	} {
		var args []any
		if strings.Contains(stmt, "$1") {
			args = append(args, keep)
		}
		if _, err := tx.Exec(ctx, stmt, args...); err != nil {
			return fmt.Errorf("putting aside what this instance keeps: %w", err)
		}
	}

	// Empty everything a clone replaces, with triggers off (loading writes no history, notifications or live
	// events) and the indexes and foreign keys dropped, as a restore does.
	var replaced []string
	for _, t := range here {
		if !slices.Contains(never, t.Name) && !slices.Contains(snapshot.Kept(), t.Name) {
			replaced = append(replaced, t.Name)
		}
	}
	replaced = append(replaced, emptied...)
	var touched []string
	for _, t := range here {
		if !slices.Contains(snapshot.Kept(), t.Name) {
			touched = append(touched, snapshot.Ident(t.Name))
		}
	}
	for _, t := range touched {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+t+" DISABLE TRIGGER USER"); err != nil {
			return err
		}
	}
	rebuild, err := snapshot.Unindex(ctx, tx)
	if err != nil {
		return err
	}
	quoted := make([]string, len(replaced))
	for i, t := range replaced {
		quoted[i] = snapshot.Ident(t)
	}
	if _, err := tx.Exec(ctx, "TRUNCATE "+strings.Join(quoted, ", ")); err != nil {
		return fmt.Errorf("emptying the tables: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL maintenance_work_mem = '256MB'"); err != nil {
		return err
	}

	// Load.
	var ids []Identity
	var totals snapshot.Totals
	hashes := map[string]string{}
	written := map[string]string{} // attachment key → backend
	hdr, err = tr.Next()
	for err == nil {
		switch {
		case strings.HasPrefix(hdr.Name, "tables/"):
			table := path.Dir(strings.TrimPrefix(hdr.Name, "tables/"))
			if columns[table] == nil {
				return fmt.Errorf("the package holds data for %s, which its contents do not list", table)
			}
			var n int64
			var sum string
			n, sum, hdr, err = snapshot.CopyIn(ctx, tx, tr, table, table, columns[table])
			rep.Rows[table], hashes[table] = n, sum
			continue
		case hdr.Name == "identities.json":
			if err := json.NewDecoder(tr).Decode(&ids); err != nil {
				return err
			}
		case hdr.Name == "report.json":
			if err := json.NewDecoder(tr).Decode(&rep.Package); err != nil {
				return err
			}
		case strings.HasPrefix(hdr.Name, "attachments/"):
			key := strings.TrimPrefix(hdr.Name, "attachments/")
			if o.PutFile != nil && safeKey(key) {
				backend, err := o.PutFile(ctx, key, hdr.Size, tr)
				if err != nil {
					return fmt.Errorf("storing the file %s: %w", key, err)
				}
				written[key] = backend
				rep.Files++
			}
		case hdr.Name == "totals.json":
			if err := json.NewDecoder(tr).Decode(&totals); err != nil {
				return fmt.Errorf("the package's totals cannot be read: %w", err)
			}
			for table, want := range totals.SHA256 {
				if hashes[table] == "" && totals.Rows[table] == 0 {
					continue
				}
				if hashes[table] != want || rep.Rows[table] != totals.Rows[table] {
					return fmt.Errorf("the table %s does not match the package's totals: the file is damaged", table)
				}
			}
			if err := finish(ctx, tx, o, rep, contents, ids, keep, userColumns, written); err != nil {
				return err
			}
			if err := rebuild(ctx); err != nil {
				return err
			}
			for _, t := range touched {
				if _, err := tx.Exec(ctx, "ALTER TABLE "+t+" ENABLE TRIGGER USER"); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, "SELECT pg_notify($1, '*')", apps.NotifyChannel)
			return err
		}
		hdr, err = tr.Next()
	}
	if errors.Is(err, io.EOF) {
		return errors.New("the package ends before its totals: it is incomplete")
	}
	return fmt.Errorf("reading the package: %w", err)
}

// finish puts back what the target keeps, moved to the package's ids, and makes the instance safe to start.
func finish(ctx context.Context, tx pgx.Tx, o LoadOptions, rep *LoadReport, contents Contents, ids []Identity,
	keep []uuid.UUID, userColumns []string, written map[string]string) error {
	// Who is who.
	source, err := readPeople(ctx, tx, "users", "service_accounts")
	if err != nil {
		return err
	}
	target, err := readPeople(ctx, tx, "clone_t_users", "clone_t_sa")
	if err != nil {
		return err
	}
	m := newMatcher(source, ids, contents.IdentitySalt)
	for _, p := range target {
		if final, ok := m.match(p); ok {
			if _, err := tx.Exec(ctx, "INSERT INTO clone_map VALUES ($1, $2) ON CONFLICT DO NOTHING", p.id, final); err != nil {
				return err
			}
		}
	}
	mapped := func(col string) string {
		return fmt.Sprintf(`coalesce((SELECT m.final FROM clone_map m WHERE m.target = %[1]s),
			CASE WHEN EXISTS (SELECT 1 FROM users u WHERE u.id = %[1]s) THEN %[1]s END)`, col)
	}

	// The people kept, each in a savepoint: one that cannot be kept is reported, never a failed clone.
	for _, p := range target {
		if !slices.Contains(keep, p.id) {
			continue
		}
		k := KeptPerson{Name: p.name, Email: p.email}
		err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			var e error
			k.Outcome, e = keepPerson(ctx, sp, p, userColumns, mapped, rep)
			return e
		})
		if err != nil {
			k.Outcome, k.Detail = "not_kept", err.Error()
		}
		rep.People = append(rep.People, k)
	}
	_, err = tx.Exec(ctx, `INSERT INTO clone_keep_people (user_id, added_at)
		SELECT DISTINCT ON (m.final) m.final, k.added_at FROM clone_t_keeplist k JOIN clone_map m ON m.target = k.user_id
		WHERE EXISTS (SELECT 1 FROM users u WHERE u.id = m.final) ORDER BY m.final ON CONFLICT DO NOTHING`)
	if err != nil {
		return err
	}

	// Single sign-on links and group mappings, API keys of kept people, and who last changed a setting or secret.
	tag, err := tx.Exec(ctx, `INSERT INTO user_identities (provider_id, subject, user_id, email, created_at, last_sign_in_at)
		SELECT i.provider_id, i.subject, m.final, i.email, i.created_at, i.last_sign_in_at
		FROM clone_t_identities i JOIN clone_map m ON m.target = i.user_id ON CONFLICT DO NOTHING`)
	if err != nil {
		return err
	}
	rep.Identities = tag.RowsAffected()
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM clone_t_identities").Scan(&rep.IdentitiesDropped); err != nil {
		return err
	}
	rep.IdentitiesDropped -= rep.Identities
	if _, err := tx.Exec(ctx, `INSERT INTO sso_group_maps (provider_id, claim_value, group_id)
		SELECT t.provider_id, t.claim_value, g.id FROM clone_t_maps t JOIN groups g ON lower(g.name) = lower(t.group_name)
		ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	for _, t := range []string{"settings", "secrets"} {
		if _, err := tx.Exec(ctx, fmt.Sprintf("UPDATE %s SET updated_by = %s WHERE updated_by IS NOT NULL", t, mapped("updated_by"))); err != nil {
			return err
		}
	}

	// The studio layer.
	if o.KeepStudio {
		if _, err := tx.Exec(ctx, "DELETE FROM app_overrides"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO app_overrides (app_name, path, content, updated_by, updated_at)
			SELECT t.app_name, t.path, t.content, `+mapped("t.updated_by")+`, t.updated_at FROM clone_t_overrides t
			WHERE EXISTS (SELECT 1 FROM apps a WHERE a.name = t.app_name)`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT t.app_name, t.path, t.content, EXISTS (SELECT 1 FROM apps a WHERE a.name = t.app_name)
			FROM clone_t_overrides t ORDER BY 1, 2`)
		if err != nil {
			return err
		}
		if err := forEach(rows, func(r pgx.Rows) error {
			var s SetAside
			var kept bool
			if err := r.Scan(&s.App, &s.Path, &s.Content, &kept); err != nil {
				return err
			}
			if kept {
				rep.StudioKept = append(rep.StudioKept, s.App+"/"+s.Path)
			} else {
				s.Reason = "production does not have the app"
				rep.SetAside = append(rep.SetAside, s)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	// Files: the rows whose file came are in this instance's storage now.
	for key, backend := range written {
		if _, err := tx.Exec(ctx, "UPDATE attachments SET backend = $2 WHERE key = $1", key, backend); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(written))
	for k := range written {
		keys = append(keys, k)
	}
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM attachments WHERE status = 'ready' AND NOT key = ANY ($1)", keys).Scan(&rep.MissingFiles); err != nil {
		return err
	}

	// Running work: runs still going are cancelled, their open approvals closed; clocks catch up without alerting.
	tag, err = tx.Exec(ctx, `UPDATE workflow_executions SET status = 'cancelled', error = 'cancelled by a clone', wake_at = NULL,
		finished_at = now(), updated_at = now() WHERE status IN ('queued', 'running', 'waiting')`)
	if err != nil {
		return err
	}
	rep.RunsCancelled = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `UPDATE workflow_approvals SET closed_at = now() WHERE closed_at IS NULL AND decided_at IS NULL`)
	if err != nil {
		return err
	}
	rep.ApprovalsClosed = tag.RowsAffected()
	if rep.ClocksSettled, err = slatimer.Settle(ctx, dbgen.New(tx), time.Now()); err != nil {
		return fmt.Errorf("settling SLA clocks: %w", err)
	}

	// The safety switch.
	for _, s := range settings.Switched {
		tag, err := tx.Exec(ctx, `UPDATE settings SET value = 'false'::jsonb, updated_at = now() WHERE name = $1 AND value = 'true'::jsonb`, s.Name)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			rep.SwitchedOff = append(rep.SwitchedOff, s.Label)
		}
	}
	for _, name := range []string{"hold_http", "hold_schedules", "hold_webhooks"} {
		if _, err := tx.Exec(ctx, `INSERT INTO settings (name, value) VALUES ($1, 'true'::jsonb)
			ON CONFLICT (name) DO UPDATE SET value = 'true'::jsonb, updated_at = now()`, settings.ClonePrefix+name); err != nil {
			return err
		}
	}
	return snapshot.SetSequences(ctx, tx, contents.Sequences)
}

// keepPerson puts one of the target's people back: onto the package's person when it has them (with this instance's
// name, address, base role, password, phone, roles, groups and keys), else as they were.
func keepPerson(ctx context.Context, tx pgx.Tx, p person, userColumns []string, mapped func(string) string, rep *LoadReport) (string, error) {
	var final uuid.UUID
	err := tx.QueryRow(ctx, "SELECT final FROM clone_map WHERE target = $1", p.id).Scan(&final)
	outcome := "matched"
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		outcome, final = "added", p.id
		cols := snapshot.Ident(userColumns[0])
		for _, c := range userColumns[1:] {
			cols += ", " + snapshot.Ident(c)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO users ("+cols+") SELECT "+cols+" FROM clone_t_users WHERE id = $1", p.id); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, "UPDATE users SET manager_id = "+mapped("manager_id")+" WHERE id = $1 AND manager_id IS NOT NULL", p.id); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO service_accounts (user_id, description, created_by, created_at)
			SELECT s.user_id, s.description, `+mapped("s.created_by")+`, s.created_at FROM clone_t_sa s WHERE s.user_id = $1`, p.id); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO clone_map VALUES ($1, $1) ON CONFLICT DO NOTHING", p.id); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	default:
		if _, err := tx.Exec(ctx, `UPDATE users u SET email = t.email, name = t.name, role = t.role, deactivated_at = t.deactivated_at,
			email_notifications = t.email_notifications, theme = t.theme FROM clone_t_users t WHERE u.id = $1 AND t.id = $2`, final, p.id); err != nil {
			return "", err
		}
	}
	for _, stmt := range []string{
		"DELETE FROM user_credentials WHERE user_id = $1",
		"INSERT INTO user_credentials (user_id, password_hash, updated_at) SELECT $1, password_hash, updated_at FROM clone_t_credentials WHERE user_id = $2",
		"DELETE FROM user_phones WHERE user_id = $1",
		`INSERT INTO user_phones (user_id, number, verified_at, sms_notifications, code_hash, code_expires_at, code_attempts, updated_at)
			SELECT $1, number, verified_at, sms_notifications, code_hash, code_expires_at, code_attempts, updated_at FROM clone_t_phones WHERE user_id = $2`,
		"DELETE FROM role_grants WHERE user_id = $1",
		`INSERT INTO role_grants (id, role, user_id, granted_by, created_at)
			SELECT t.id, t.role, $1, ` + mapped("t.granted_by") + `, t.created_at FROM clone_t_grants t
			WHERE t.user_id = $2 AND EXISTS (SELECT 1 FROM roles r WHERE r.name = t.role) ON CONFLICT DO NOTHING`,
		"DELETE FROM group_members WHERE user_id = $1",
		`INSERT INTO group_members (group_id, user_id, added_at)
			SELECT g.id, $1, t.added_at FROM clone_t_members t JOIN groups g ON lower(g.name) = lower(t.group_name)
			WHERE t.user_id = $2 ON CONFLICT DO NOTHING`,
	} {
		args := []any{final}
		if strings.Contains(stmt, "$2") {
			args = append(args, p.id)
		}
		if _, err := tx.Exec(ctx, stmt, args...); err != nil {
			return "", err
		}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO api_keys (id, user_id, name, prefix, secret_hash, scopes, created_by, created_at, expires_at,
			last_used_at, revoked_at, rate_limit)
		SELECT k.id, $1, k.name, k.prefix, k.secret_hash, k.scopes, `+mapped("k.created_by")+`, k.created_at, k.expires_at,
			k.last_used_at, k.revoked_at, k.rate_limit FROM clone_t_keys k WHERE k.user_id = $2 ON CONFLICT DO NOTHING`, final, p.id)
	if err != nil {
		return "", err
	}
	rep.APIKeys += tag.RowsAffected()
	rows, err := tx.Query(ctx, `
		SELECT 'group ' || t.group_name FROM clone_t_members t
		WHERE t.user_id = $1 AND NOT EXISTS (SELECT 1 FROM groups g WHERE lower(g.name) = lower(t.group_name))
		UNION ALL
		SELECT 'role ' || t.role FROM clone_t_grants t WHERE t.user_id = $1 AND NOT EXISTS (SELECT 1 FROM roles r WHERE r.name = t.role)
		ORDER BY 1`, p.id)
	if err != nil {
		return "", err
	}
	err = forEach(rows, func(r pgx.Rows) error {
		var what string
		if err := r.Scan(&what); err != nil {
			return err
		}
		rep.Dropped = append(rep.Dropped, p.name+": "+what+" (production does not have it)")
		return nil
	})
	return outcome, err
}

// ReinstallStudio installs again, over this instance's kept studio layer, every app that has one, once a load is
// committed. An app whose edits no longer compile against production's version of it has them set aside (removed
// from the layer and kept in the report), never lost.
func ReinstallStudio(ctx context.Context, pool *pgxpool.Pool, rep *LoadReport) error {
	var overrides []SetAside
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT app_name, path, content FROM app_overrides ORDER BY 1, 2`)
		if err != nil {
			return err
		}
		return forEach(rows, func(r pgx.Rows) error {
			var s SetAside
			if err := r.Scan(&s.App, &s.Path, &s.Content); err != nil {
				return err
			}
			overrides = append(overrides, s)
			return nil
		})
	}); err != nil {
		return err
	}
	byApp := map[string][]SetAside{}
	var names []string
	for _, s := range overrides {
		if byApp[s.App] == nil {
			names = append(names, s.App)
		}
		byApp[s.App] = append(byApp[s.App], s)
	}
	for _, app := range names {
		if _, _, err := apps.UpdateStudio(ctx, pool, apps.StudioUpdate{App: app, InstalledBy: "clone"}); err == nil {
			continue
		} else {
			reason := "it no longer works with production's version of the app: " + err.Error()
			if _, _, cerr := apps.UpdateStudio(ctx, pool, apps.StudioUpdate{App: app, ReplaceAll: true, InstalledBy: "clone"}); cerr != nil {
				return fmt.Errorf("setting aside the edits to %s: %w", app, cerr)
			}
			for _, s := range byApp[app] {
				s.Reason = reason
				rep.SetAside = append(rep.SetAside, s)
				rep.StudioKept = slices.DeleteFunc(rep.StudioKept, func(k string) bool { return k == s.App+"/"+s.Path })
			}
		}
	}
	return nil
}

// CheckPassphrase reads only a package's header and says whether the passphrase opens it.
func CheckPassphrase(r io.Reader, passphrase string) (Header, error) {
	head, _, done, err := open(r, passphrase)
	if done != nil {
		done()
	}
	return head, err
}
