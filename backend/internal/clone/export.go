package clone

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klauspost/compress/zstd"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/snapshot"
)

// Magic starts every clone package.
const Magic = "SLCLONE1\n"

// Format is this package's clone package format.
const Format = 1

// MinPassphrase is the shortest passphrase a package may have: it is all that protects production's data in transit.
const MinPassphrase = 12

// Header is what a package says about itself before it is decrypted.
type Header struct {
	Format int `json:"format"`
	// Schema is the source's migration version.
	Schema  int64     `json:"schema"`
	TakenAt time.Time `json:"taken_at"`
	// Instance and Environment are the source's.
	Instance    string `json:"instance,omitempty"`
	Environment string `json:"environment,omitempty"`
	// Profile is the name of the profile it was made from.
	Profile string `json:"profile"`
}

// Contents is the first entry inside a package.
type Contents struct {
	Header
	Binary    string              `json:"binary"`
	Apps      []snapshot.AppAt    `json:"apps"`
	Tables    []snapshot.Table    `json:"tables"`
	Sequences []snapshot.Sequence `json:"sequences"`
	Config    Profile             `json:"config"`
	// IdentitySalt salts the hashes in identities.json, present when emails are masked: the target hashes its own
	// people's addresses the same way to match them.
	IdentitySalt string `json:"identity_salt,omitempty"`
}

// Identity is a masked person's real address, hashed: hex SHA-256 of the salt and the lowercased address.
type Identity struct {
	ID        uuid.UUID `json:"id"`
	EmailHash string    `json:"email_hash"`
}

// Report is what a package holds and what it left out.
type Report struct {
	Records []TypeCount `json:"records"`
	// Excluded is how many records the profile's exclusions left out.
	Excluded        int64 `json:"excluded"`
	People          int64 `json:"people"`
	PeopleTotal     int64 `json:"people_total"`
	Attachments     int64 `json:"attachments"`
	AttachmentBytes int64 `json:"attachment_bytes"`
	// MissingFiles are attachments whose file storage did not have: the row comes, the file does not.
	MissingFiles int `json:"missing_files"`
	// Cleared are links from a kept record to one left out, emptied (the first 500; ClearedTotal counts them all).
	Cleared      []Cleared `json:"cleared"`
	ClearedTotal int       `json:"cleared_total"`
}

// TypeCount is how many records of a type the package keeps, of how many there are.
type TypeCount struct {
	Type  string `json:"type"`
	Label string `json:"label"`
	Kept  int64  `json:"kept"`
	Total int64  `json:"total"`
}

// Cleared is one link emptied: Record's Link pointed at To, which stays behind.
type Cleared struct {
	Record string `json:"record"`
	Link   string `json:"link"`
	To     string `json:"to"`
}

const maxCleared = 500

// Summary is what was written or read.
type Summary struct {
	Contents
	snapshot.Totals
	Report Report `json:"report"`
}

// Options are what Export and Preview take.
type Options struct {
	Profile     Profile
	ProfileName string
	// Passphrase encrypts the package (Export only).
	Passphrase string
	// Exclusion compiles an exclusion's expression for its record type (the rule environment, with that type's form
	// fields). Needed when the profile has exclusions.
	Exclusion func(recordType, when string) (*expr.Program, error)
	// MaskedFields are the extra fields marked mask: true in installed forms and catalog items.
	MaskedFields []string
	// Files opens an attachment's file in its backend (Export only); nil leaves the files out.
	Files       func(ctx context.Context, backend, key string) (io.ReadCloser, int64, error)
	Instance    string
	Environment string
	Binary      string
}

// Preview works out what a package made with these options would hold, without making one: the same choices, from
// the same kind of transaction, rolled back.
func Preview(ctx context.Context, pool *pgxpool.Pool, o Options) (*Report, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rep, err := prepare(ctx, tx, o)
	if err != nil {
		return nil, err
	}
	if o.Profile.Attachments != AttachmentsNone {
		q, _ := selectFor("attachments", []string{"size"}, o.Profile)
		if err := tx.QueryRow(ctx, "SELECT count(*), COALESCE(sum(size), 0) FROM ("+q+") a").Scan(&rep.Attachments, &rep.AttachmentBytes); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

// Export writes a clone package to w. It reads in one REPEATABLE READ transaction, so every table is from the same
// moment; the transaction only fills temporary tables and is rolled back, so the instance is never changed.
func Export(ctx context.Context, pool *pgxpool.Pool, w io.Writer, o Options) (*Summary, error) {
	if utf8.RuneCountInString(o.Passphrase) < MinPassphrase {
		return nil, fmt.Errorf("the passphrase must be at least %d characters", MinPassphrase)
	}
	schema, err := db.SchemaVersion(ctx, pool)
	if err != nil {
		return nil, err
	}
	recipient, err := age.NewScryptRecipient(o.Passphrase)
	if err != nil {
		return nil, err
	}
	head := Header{Format: Format, Schema: schema, TakenAt: time.Now().UTC().Truncate(time.Second), Instance: o.Instance,
		Environment: o.Environment, Profile: o.ProfileName}
	line, _ := json.Marshal(head)
	if _, err := io.WriteString(w, Magic+string(line)+"\n"); err != nil {
		return nil, err
	}
	enc, err := age.Encrypt(w, recipient)
	if err != nil {
		return nil, err
	}
	zw, err := zstd.NewWriter(enc, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(zw)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	sum := &Summary{Contents: Contents{Header: head, Binary: o.Binary, Config: o.Profile},
		Totals: snapshot.Totals{Rows: map[string]int64{}, SHA256: map[string]string{}}}
	// The first query fixes the moment every later one sees.
	if sum.Tables, err = snapshot.AllTables(ctx, tx, never); err != nil {
		return nil, err
	}
	for _, t := range sum.Tables {
		if _, ok := policies[t.Name]; !ok {
			return nil, fmt.Errorf("clone: the table %s has no clone policy", t.Name)
		}
	}
	if sum.Sequences, err = snapshot.Sequences(ctx, tx); err != nil {
		return nil, err
	}
	if sum.Apps, err = snapshot.InstalledApps(ctx, tx); err != nil {
		return nil, err
	}
	rep, err := prepare(ctx, tx, o)
	if err != nil {
		return nil, err
	}
	sum.Report = *rep
	var ids []Identity
	if o.Profile.Mask.Emails {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		sum.IdentitySalt = hex.EncodeToString(salt)
		if ids, err = identities(ctx, tx, sum.IdentitySalt); err != nil {
			return nil, err
		}
	}
	if err := snapshot.WriteJSON(tw, "clone.json", sum.Contents); err != nil {
		return nil, err
	}
	for _, t := range sum.Tables {
		q, _ := selectFor(t.Name, t.Columns, o.Profile)
		rows, sha, err := snapshot.CopyOut(ctx, tx, tw, t.Name, q)
		if err != nil {
			return nil, fmt.Errorf("clone: %w", err)
		}
		sum.Rows[t.Name], sum.SHA256[t.Name] = rows, sha
	}
	if ids != nil {
		if err := snapshot.WriteJSON(tw, "identities.json", ids); err != nil {
			return nil, err
		}
	}
	if o.Files != nil && o.Profile.Attachments != AttachmentsNone {
		if err := addFiles(ctx, tx, tw, o, sum); err != nil {
			return nil, err
		}
	}
	if err := snapshot.WriteJSON(tw, "report.json", sum.Report); err != nil {
		return nil, err
	}
	if err := snapshot.WriteJSON(tw, "totals.json", sum.Totals); err != nil {
		return nil, err
	}
	for _, c := range []io.Closer{tw, zw, enc} {
		if err := c.Close(); err != nil {
			return nil, err
		}
	}
	return sum, nil
}

// prepare fills the temporary tables the policies read (see plan.go) and works out the report.
func prepare(ctx context.Context, tx pgx.Tx, o Options) (*Report, error) {
	p := o.Profile
	for _, stmt := range []string{
		"CREATE TEMP TABLE clone_tasks (id uuid PRIMARY KEY) ON COMMIT DROP",
		"CREATE TEMP TABLE clone_runs (id uuid PRIMARY KEY) ON COMMIT DROP",
		"CREATE TEMP TABLE clone_users (id uuid PRIMARY KEY, n bigint) ON COMMIT DROP",
		"CREATE TEMP TABLE clone_mask (field text PRIMARY KEY) ON COMMIT DROP",
		"CREATE TEMP TABLE clone_notes (id uuid PRIMARY KEY) ON COMMIT DROP",
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return nil, err
		}
	}
	rep := &Report{Cleared: []Cleared{}, Records: []TypeCount{}}

	// Records: by type, then which ones, then the exclusions.
	where := []string{"true"}
	var args []any
	if len(p.RecordTypes) > 0 {
		args = append(args, p.RecordTypes)
		where = append(where, fmt.Sprintf("t.type = ANY ($%d)", len(args)))
	}
	switch p.Records {
	case RecordsOpen:
		where = append(where, "t.state_category IN ('open', 'waiting')")
	case RecordsRecent:
		args = append(args, p.RecentDays)
		where = append(where, fmt.Sprintf("t.updated_at > now() - make_interval(days => $%d)", len(args)))
	}
	if _, err := tx.Exec(ctx, "INSERT INTO clone_tasks SELECT t.id FROM tasks t WHERE "+strings.Join(where, " AND "), args...); err != nil {
		return nil, fmt.Errorf("clone: choosing records: %w", err)
	}
	for i, e := range p.Exclude {
		n, err := exclude(ctx, tx, o, e)
		if err != nil {
			return nil, fmt.Errorf("clone: exclusion %d (%s): %w", i+1, e.RecordType, err)
		}
		rep.Excluded += n
	}

	// Workflow runs: those of kept records or of none, whose parent run is kept too.
	if _, err := tx.Exec(ctx, "INSERT INTO clone_runs SELECT t.id FROM workflow_executions t WHERE "+taskOrNull("record_id")); err != nil {
		return nil, err
	}
	for {
		tag, err := tx.Exec(ctx, `DELETE FROM clone_runs r USING workflow_executions e
			WHERE e.id = r.id AND e.parent_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM clone_runs k WHERE k.id = e.parent_id)`)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			break
		}
	}

	// People.
	if err := choosePeople(ctx, tx, p); err != nil {
		return nil, fmt.Errorf("clone: choosing people: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE clone_users k SET n = r.n
		FROM (SELECT u.id, row_number() OVER (ORDER BY u.created_at, u.id) AS n FROM users u JOIN clone_users c ON c.id = u.id) r
		WHERE r.id = k.id`); err != nil {
		return nil, err
	}
	if p.Mask.Fields && len(o.MaskedFields) > 0 {
		if _, err := tx.Exec(ctx, "INSERT INTO clone_mask SELECT DISTINCT unnest($1::text[])", o.MaskedFields); err != nil {
			return nil, err
		}
	}
	if p.Logs {
		if _, err := tx.Exec(ctx, "INSERT INTO clone_notes SELECT t.id FROM notifications t WHERE "+user("user_id")+" AND "+taskOrNull("task_id")+
			" AND (t.approval_id IS NULL OR EXISTS (SELECT 1 FROM workflow_approvals a JOIN clone_runs k ON k.id = a.execution_id WHERE a.id = t.approval_id))"); err != nil {
			return nil, err
		}
	}

	// The report.
	rows, err := tx.Query(ctx, `SELECT tt.type, tt.label, count(k.id), count(t.id)
		FROM task_types tt JOIN tasks t ON t.type = tt.type LEFT JOIN clone_tasks k ON k.id = t.id
		GROUP BY tt.type, tt.label ORDER BY tt.type`)
	if err != nil {
		return nil, err
	}
	if rep.Records, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (TypeCount, error) {
		var c TypeCount
		return c, r.Scan(&c.Type, &c.Label, &c.Kept, &c.Total)
	}); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, "SELECT (SELECT count(*) FROM clone_users k JOIN users u ON u.id = k.id), (SELECT count(*) FROM users)").
		Scan(&rep.People, &rep.PeopleTotal); err != nil {
		return nil, err
	}
	cleared := `
		SELECT tt.prefix || t.number, 'part of', pt.prefix || pr.number FROM tasks t
			JOIN clone_tasks k ON k.id = t.id JOIN task_types tt ON tt.type = t.type
			JOIN tasks pr ON pr.id = t.parent_id JOIN task_types pt ON pt.type = pr.type
			WHERE NOT EXISTS (SELECT 1 FROM clone_tasks c WHERE c.id = t.parent_id)
		UNION ALL
		SELECT tt.prefix || t.number, 'problem', pt.prefix || pr.number FROM incidents x JOIN tasks t ON t.id = x.task_id
			JOIN clone_tasks k ON k.id = t.id JOIN task_types tt ON tt.type = t.type
			JOIN tasks pr ON pr.id = x.problem_id JOIN task_types pt ON pt.type = pr.type
			WHERE NOT EXISTS (SELECT 1 FROM clone_tasks c WHERE c.id = x.problem_id)
		UNION ALL
		SELECT tt.prefix || t.number, 'fixes problem', pt.prefix || pr.number FROM changes x JOIN tasks t ON t.id = x.task_id
			JOIN clone_tasks k ON k.id = t.id JOIN task_types tt ON tt.type = t.type
			JOIN tasks pr ON pr.id = x.problem_id JOIN task_types pt ON pt.type = pr.type
			WHERE NOT EXISTS (SELECT 1 FROM clone_tasks c WHERE c.id = x.problem_id)`
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM ("+cleared+") c").Scan(&rep.ClearedTotal); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, "SELECT * FROM ("+cleared+") c ORDER BY 1, 2 LIMIT "+fmt.Sprint(maxCleared))
	if err != nil {
		return nil, err
	}
	if rep.Cleared, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Cleared, error) {
		var c Cleared
		return c, r.Scan(&c.Record, &c.Link, &c.To)
	}); err != nil {
		return nil, err
	}
	if rep.Cleared == nil {
		rep.Cleared = []Cleared{}
	}
	return rep, nil
}

// exclude drops the kept records of one exclusion's type that its expression holds for, and says how many.
func exclude(ctx context.Context, tx pgx.Tx, o Options, e Exclusion) (int64, error) {
	if o.Exclusion == nil {
		return 0, errors.New("exclusions cannot be checked here")
	}
	prog, err := o.Exclusion(e.RecordType, e.When)
	if err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, "SELECT t.id FROM tasks t JOIN clone_tasks k ON k.id = t.id WHERE t.type = $1 ORDER BY t.id", e.RecordType)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	q := dbgen.New(tx)
	now := time.Now()
	var drop []uuid.UUID
	for _, id := range ids {
		t, err := records.Load(ctx, q, id)
		if err != nil {
			return 0, err
		}
		rec := t.ExprRecord()
		out, err := prog.Eval(ctx, expr.Input{Record: rec, Now: now})
		if err != nil {
			return 0, fmt.Errorf("%s%d: %w (use has() for a field that may be unset)", t.Prefix, t.Number, err)
		}
		if out {
			drop = append(drop, id)
		}
	}
	if len(drop) == 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, "DELETE FROM clone_tasks WHERE id = ANY ($1)", drop)
	return tag.RowsAffected(), err
}

// choosePeople fills clone_users: everyone, or (People: referenced) the apps' principals, service accounts, members
// of groups, the people kept rows name, and their managers.
func choosePeople(ctx context.Context, tx pgx.Tx, p Profile) error {
	if p.People == PeopleAll {
		_, err := tx.Exec(ctx, "INSERT INTO clone_users (id) SELECT id FROM users")
		return err
	}
	parts := []string{
		"SELECT id FROM users WHERE app_name IS NOT NULL",
		"SELECT user_id FROM service_accounts",
		"SELECT user_id FROM group_members",
	}
	tables := make([]string, 0, len(policies))
	for name := range policies {
		tables = append(tables, name)
	}
	slices.Sort(tables)
	for _, name := range tables {
		pol := policies[name]
		w := pol.where(p)
		if w == "false" {
			continue
		}
		if w != "" {
			w = " WHERE " + w
		}
		for _, col := range pol.brings {
			parts = append(parts, fmt.Sprintf("SELECT t.%s FROM %s t%s", quote(col), quote(name), w))
		}
	}
	if _, err := tx.Exec(ctx, "INSERT INTO clone_users (id) SELECT DISTINCT x.id FROM ("+strings.Join(parts, " UNION ")+
		") x(id) JOIN users u ON u.id = x.id ON CONFLICT DO NOTHING"); err != nil {
		return err
	}
	for {
		tag, err := tx.Exec(ctx, `INSERT INTO clone_users (id) SELECT DISTINCT u.manager_id FROM users u
			JOIN clone_users k ON k.id = u.id WHERE u.manager_id IS NOT NULL ON CONFLICT DO NOTHING`)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
	}
}

func identities(ctx context.Context, tx pgx.Tx, salt string) ([]Identity, error) {
	rows, err := tx.Query(ctx, `SELECT t.id, lower(t.email) FROM users t JOIN clone_users k ON k.id = t.id
		WHERE t.email IS NOT NULL AND t.email <> '' AND `+isPerson+` ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	out := []Identity{}
	err = forEach(rows, func(r pgx.Rows) error {
		var id uuid.UUID
		var email string
		if err := r.Scan(&id, &email); err != nil {
			return err
		}
		out = append(out, Identity{ID: id, EmailHash: HashEmail(salt, email)})
		return nil
	})
	return out, err
}

// HashEmail is how a package hashes an address, for the target to match its own people against.
func HashEmail(salt, email string) string {
	h := sha256.Sum256([]byte(salt + "\x00" + strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(h[:])
}

func addFiles(ctx context.Context, tx pgx.Tx, tw *tar.Writer, o Options, sum *Summary) error {
	q, _ := selectFor("attachments", []string{"backend", "key"}, o.Profile)
	rows, err := tx.Query(ctx, q+" ORDER BY 2")
	if err != nil {
		return err
	}
	type file struct{ backend, key string }
	files, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (file, error) {
		var f file
		return f, r.Scan(&f.backend, &f.key)
	})
	if err != nil {
		return err
	}
	for _, f := range files {
		if !safeKey(f.key) {
			return fmt.Errorf("clone: the attachment key %q is not one ServiceLater makes", f.key)
		}
		rc, size, err := o.Files(ctx, f.backend, f.key)
		if err != nil {
			sum.Report.MissingFiles++
			continue
		}
		err = tw.WriteHeader(&tar.Header{Name: "attachments/" + f.key, Mode: 0o600, Size: size})
		if err == nil {
			var n int64
			n, err = io.Copy(tw, rc)
			if err == nil && n != size {
				err = fmt.Errorf("the file %s is %d bytes, not %d", f.key, n, size)
			}
		}
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("clone: adding the file %s: %w", f.key, err)
		}
		sum.AttachmentFiles++
		sum.AttachmentBytes += size
	}
	sum.Report.Attachments, sum.Report.AttachmentBytes = int64(sum.AttachmentFiles), sum.AttachmentBytes
	return nil
}

func safeKey(key string) bool {
	return key != "" && !strings.HasPrefix(key, "/") && path.Clean(key) == key && !strings.HasPrefix(key, "../") && key != ".."
}

func forEach(rows pgx.Rows, fn func(pgx.Rows) error) error {
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ---- reading a package ----

// ReadHeader reads a package's plaintext header, leaving r just after it.
func ReadHeader(r *bufio.Reader) (Header, error) {
	magic := make([]byte, len(Magic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != Magic {
		return Header{}, errors.New("this is not a ServiceLater clone package")
	}
	line, err := r.ReadBytes('\n')
	if err != nil {
		return Header{}, errors.New("the package's header is cut short")
	}
	var h Header
	if err := json.Unmarshal(line, &h); err != nil {
		return Header{}, fmt.Errorf("the package's header cannot be read: %w", err)
	}
	if h.Format != Format {
		return Header{}, fmt.Errorf("the package is in format %d; this version of ServiceLater reads format %d", h.Format, Format)
	}
	return h, nil
}

// ErrPassphrase is a package that does not open with the passphrase given.
var ErrPassphrase = errors.New("the package does not open with this passphrase")

// Verify reads a whole package without loading it: the passphrase opens it, and every table's rows and checksum
// match its totals.
func Verify(r io.Reader, passphrase string) (*Summary, error) {
	br := bufio.NewReader(r)
	if _, err := ReadHeader(br); err != nil {
		return nil, err
	}
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, err
	}
	dec, err := age.Decrypt(br, id)
	if err != nil {
		var wrong *age.NoIdentityMatchError
		if errors.As(err, &wrong) {
			return nil, ErrPassphrase
		}
		return nil, fmt.Errorf("the package cannot be decrypted: %w", err)
	}
	zr, err := zstd.NewReader(dec)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	sum := &Summary{Totals: snapshot.Totals{Rows: map[string]int64{}, SHA256: map[string]string{}}}
	hashes := map[string]hash.Hash{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the package ends before its totals: it is incomplete")
		}
		if err != nil {
			return nil, fmt.Errorf("reading the package: %w", err)
		}
		switch {
		case hdr.Name == "clone.json":
			if err := json.NewDecoder(tr).Decode(&sum.Contents); err != nil {
				return nil, err
			}
		case hdr.Name == "report.json":
			if err := json.NewDecoder(tr).Decode(&sum.Report); err != nil {
				return nil, err
			}
		case strings.HasPrefix(hdr.Name, "tables/"):
			table := path.Dir(strings.TrimPrefix(hdr.Name, "tables/"))
			if hashes[table] == nil {
				hashes[table] = sha256.New()
			}
			if _, err := io.Copy(hashes[table], tr); err != nil { //nolint:gosec // age authenticates the stream: only the passphrase's holder can make it
				return nil, err
			}
		case hdr.Name == "totals.json":
			if err := json.NewDecoder(tr).Decode(&sum.Totals); err != nil {
				return nil, err
			}
			for table, want := range sum.SHA256 {
				h := hashes[table]
				if h == nil {
					h = sha256.New()
				}
				if hex.EncodeToString(h.Sum(nil)) != want {
					return nil, fmt.Errorf("the table %s does not match the package's totals: the file is damaged", table)
				}
			}
			return sum, nil
		}
	}
}
