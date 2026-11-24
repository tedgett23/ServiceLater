// Package snapshot takes and restores ServiceLater snapshots (docs/backup-and-clone.md §1): every table, from one
// consistent moment, in Postgres's binary COPY format, with the sequences and, if asked, the attachment files kept
// in a folder. A snapshot is one stream: a short plaintext header (format, the secrets key it is encrypted under, the
// schema version), then a tar archive compressed with zstd and encrypted with age under a key derived from the
// instance's secrets key. Restoring replaces every table in one transaction.
package snapshot

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klauspost/compress/zstd"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/secrets"
)

// Magic starts every snapshot.
const Magic = "SLSNAP1\n"

// Format is this package's snapshot format.
const Format = 1

// chunk is how much of a table's COPY output one tar entry holds: tar needs each entry's size up front, and a table
// may be larger than memory, so a table is cut into entries of at most this size.
const chunk = 8 << 20

// workFactor is age's scrypt cost for the derived key: it is 256 random bits, so stretching adds nothing.
const workFactor = 10

// Left out of every snapshot: sign-in sessions, rate counting, queued work (it would send mail again) and the
// migration table (recorded as Header.Schema instead). A restore empties them.
var skipped = []string{"sessions", "rate_limits", "rate_counts", "jobs", "goose_db_version"}

// Kept: the record of snapshots and restores is neither in a snapshot nor emptied by a restore, so restoring an old
// snapshot does not forget the ones taken since, or that it ran.
var kept = []string{"snapshots", "snapshot_restores"}

// Header is what a snapshot says about itself before anything is decrypted: enough to refuse it early and to pick
// the key.
type Header struct {
	Format int `json:"format"`
	// KeyID is the secrets key the snapshot is encrypted under (secrets.KeyID).
	KeyID string `json:"key_id"`
	// Schema is the migration version of the database it was taken from.
	Schema  int64     `json:"schema"`
	TakenAt time.Time `json:"taken_at"`
	// Instance is the instance's name, when it has one.
	Instance string `json:"instance,omitempty"`
}

// Contents is the first entry inside a snapshot: what it holds, in the order it holds it.
type Contents struct {
	Header
	Binary    string     `json:"binary"`
	Apps      []AppAt    `json:"apps"`
	Tables    []Table    `json:"tables"`
	Sequences []Sequence `json:"sequences"`
	// Attachments says whether the attachment files kept in a folder are inside.
	Attachments bool `json:"attachments"`
}

// AppAt is an installed app and its version.
type AppAt struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Table is one table: its columns in COPY order (generated ones left out).
type Table struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
}

// Sequence is a sequence's position. Value is nil when it has never been used.
type Sequence struct {
	Name  string `json:"name"`
	Value *int64 `json:"value"`
}

// Totals is the last entry: rows and a SHA-256 of each table's COPY data, and the attachment files, so a restore
// can tell a complete, unchanged snapshot from one that is not.
type Totals struct {
	Rows            map[string]int64  `json:"rows"`
	SHA256          map[string]string `json:"sha256"`
	AttachmentFiles int               `json:"attachment_files"`
	AttachmentBytes int64             `json:"attachment_bytes"`
}

// Options are what Create takes.
type Options struct {
	Keys *secrets.Keyring
	// Instance names the instance in the header.
	Instance string
	// Binary is the version of the binary taking it.
	Binary string
	// AttachmentsDir, when set, adds the attachment files kept in that folder.
	AttachmentsDir string
}

// Summary is what was written or read.
type Summary struct {
	Contents
	Totals
}

// Create writes a snapshot of the database to w. It reads in one REPEATABLE READ, READ ONLY transaction, so every
// table is from the same moment, and takes no lock beyond what a long read takes.
func Create(ctx context.Context, pool *pgxpool.Pool, w io.Writer, o Options) (*Summary, error) {
	if !o.Keys.Configured() {
		return nil, secrets.ErrNoKey
	}
	schema, err := db.SchemaVersion(ctx, pool)
	if err != nil {
		return nil, err
	}
	keyID := o.Keys.Current()
	recipient, err := recipientFor(o.Keys, keyID)
	if err != nil {
		return nil, err
	}
	head := Header{Format: Format, KeyID: keyID, Schema: schema, TakenAt: time.Now().UTC().Truncate(time.Second), Instance: o.Instance}
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

	sum := &Summary{Contents: Contents{Header: head, Binary: o.Binary, Attachments: o.AttachmentsDir != ""},
		Totals: Totals{Rows: map[string]int64{}, SHA256: map[string]string{}}}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The first query fixes the moment every later one sees.
	if sum.Tables, err = tablesInOrder(ctx, tx); err != nil {
		return nil, err
	}
	if sum.Sequences, err = sequences(ctx, tx); err != nil {
		return nil, err
	}
	if sum.Apps, err = installedApps(ctx, tx); err != nil {
		return nil, err
	}
	if err := writeJSON(tw, "snapshot.json", sum.Contents); err != nil {
		return nil, err
	}
	for _, t := range sum.Tables {
		cw := &chunker{tw: tw, table: t.Name, hash: sha256.New(), buf: make([]byte, 0, chunk)}
		tag, err := tx.Conn().PgConn().CopyTo(ctx, cw, fmt.Sprintf("COPY %s (%s) TO STDOUT WITH (FORMAT binary)", ident(t.Name), idents(t.Columns)))
		if err != nil {
			return nil, fmt.Errorf("snapshot: copying %s: %w", t.Name, err)
		}
		if err := cw.flush(); err != nil {
			return nil, err
		}
		sum.Rows[t.Name] = tag.RowsAffected()
		sum.SHA256[t.Name] = hex.EncodeToString(cw.hash.Sum(nil))
	}
	if o.AttachmentsDir != "" {
		if err := addAttachments(ctx, tx, tw, o.AttachmentsDir, &sum.Totals); err != nil {
			return nil, err
		}
	}
	if err := writeJSON(tw, "totals.json", sum.Totals); err != nil {
		return nil, err
	}
	for _, c := range []io.Closer{tw, zw, enc} {
		if err := c.Close(); err != nil {
			return nil, err
		}
	}
	return sum, nil
}

// ReadHeader reads a snapshot's plaintext header, leaving r just after it.
func ReadHeader(r *bufio.Reader) (Header, error) {
	magic := make([]byte, len(Magic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != Magic {
		return Header{}, errors.New("this is not a ServiceLater snapshot")
	}
	line, err := r.ReadBytes('\n')
	if err != nil {
		return Header{}, errors.New("the snapshot's header is cut short")
	}
	var h Header
	if err := json.Unmarshal(line, &h); err != nil {
		return Header{}, fmt.Errorf("the snapshot's header cannot be read: %w", err)
	}
	if h.Format != Format {
		return Header{}, fmt.Errorf("the snapshot is in format %d; this version of ServiceLater reads format %d", h.Format, Format)
	}
	return h, nil
}

// RestoreOptions are what Restore takes.
type RestoreOptions struct {
	Keys *secrets.Keyring
	// ChangeSchema allows a snapshot from an older schema: the database is migrated down to it, loaded and migrated
	// up again. Without it, only a snapshot from the database's own schema version is restored.
	ChangeSchema bool
	// AttachmentsDir is where attachment files inside the snapshot are written back; empty skips them.
	AttachmentsDir string
}

// ErrNewer is a snapshot from a newer version of ServiceLater than this one.
var ErrNewer = errors.New("the snapshot is from a newer version of ServiceLater: update this one first")

// Restore replaces everything in the database with the snapshot read from r, in one transaction: on any problem
// (a wrong key, a table cut short, a checksum that does not match), nothing changes. Sessions, rate counts and
// queued work are emptied, so everyone signs in again. Every server reloads its app definitions.
func Restore(ctx context.Context, pool *pgxpool.Pool, r io.Reader, o RestoreOptions) (*Summary, error) {
	br := bufio.NewReader(r)
	head, err := ReadHeader(br)
	if err != nil {
		return nil, err
	}
	latest, err := db.LatestVersion()
	if err != nil {
		return nil, err
	}
	if head.Schema > latest {
		return nil, ErrNewer
	}
	current, err := db.SchemaVersion(ctx, pool)
	if err != nil {
		return nil, err
	}
	identity, err := identityFor(o.Keys, head.KeyID)
	if err != nil {
		return nil, err
	}
	dec, err := age.Decrypt(br, identity)
	if err != nil {
		return nil, fmt.Errorf("the snapshot cannot be decrypted with this server's secrets key: %w", err)
	}
	zr, err := zstd.NewReader(dec)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	sum := &Summary{Totals: Totals{Rows: map[string]int64{}, SHA256: map[string]string{}}}
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "snapshot.json" {
		return nil, errors.New("the snapshot does not start with its contents")
	}
	if err := json.NewDecoder(tr).Decode(&sum.Contents); err != nil {
		return nil, fmt.Errorf("the snapshot's contents cannot be read: %w", err)
	}

	if head.Schema != current {
		if !o.ChangeSchema {
			return nil, fmt.Errorf("the snapshot is from schema version %d and this database is at %d: restoring it migrates the database to %d, loads it and migrates it up again (allow that to go on)", head.Schema, current, head.Schema)
		}
		if err := db.MigrateTo(ctx, pool, head.Schema); err != nil {
			return nil, err
		}
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return load(ctx, tx, tr, sum, o.AttachmentsDir)
	})
	if head.Schema != current {
		// Loaded or not, leave the schema where this binary expects it.
		if uerr := db.Migrate(context.WithoutCancel(ctx), pool); uerr != nil {
			return nil, errors.Join(err, fmt.Errorf("migrating up again: %w", uerr))
		}
	}
	if err != nil {
		return nil, err
	}
	return sum, nil
}

func load(ctx context.Context, tx pgx.Tx, tr *tar.Reader, sum *Summary, attachmentsDir string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('servicelater:snapshot-restore'))"); err != nil {
		return err
	}
	current, err := tablesInOrder(ctx, tx)
	if err != nil {
		return err
	}
	byName := map[string]Table{}
	for _, t := range current {
		byName[t.Name] = t
	}
	for _, t := range sum.Tables {
		have, ok := byName[t.Name]
		if !ok {
			return fmt.Errorf("the snapshot has a table %s this database does not", t.Name)
		}
		if !slices.Equal(have.Columns, t.Columns) {
			return fmt.Errorf("the table %s has other columns here (%s) than in the snapshot (%s)", t.Name, strings.Join(have.Columns, ", "), strings.Join(t.Columns, ", "))
		}
	}
	all := make([]string, 0, len(current)+len(skipped))
	for _, t := range current {
		all = append(all, ident(t.Name))
	}
	// The tables a snapshot leaves out are emptied too, those that exist at this schema version.
	rows, err := tx.Query(ctx, `SELECT relname FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relkind = 'r'
		AND relname = ANY ($1) AND relname <> 'goose_db_version'`, skipped)
	if err != nil {
		return err
	}
	empty, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, s := range empty {
		all = append(all, ident(s))
	}
	if _, err := tx.Exec(ctx, "TRUNCATE "+strings.Join(all, ", ")); err != nil {
		return fmt.Errorf("emptying the tables: %w", err)
	}
	// Loading writes no history, notifications or live events: the rows are put back as they were. Foreign keys are
	// still checked (they are not user triggers).
	for _, t := range current {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+ident(t.Name)+" DISABLE TRIGGER USER"); err != nil {
			return err
		}
	}
	// Like pg_restore: load first, build indexes after. Updating every search index row by row is most of a load's
	// time. Indexes behind a constraint (primary keys, unique ones the foreign keys use) stay; the transaction puts
	// back the rest, or puts back everything if it rolls back.
	indexes, err := plainIndexes(ctx, tx)
	if err != nil {
		return err
	}
	for _, ix := range indexes {
		if _, err := tx.Exec(ctx, "DROP INDEX "+ident(ix.name)); err != nil {
			return err
		}
	}
	// Foreign keys too: checking each loaded row one by one is slow; adding the key back checks every row at once,
	// and a row that breaks one stops the restore just the same.
	keys, err := foreignKeys(ctx, tx)
	if err != nil {
		return err
	}
	for _, fk := range keys {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+ident(fk.table)+" DROP CONSTRAINT "+ident(fk.name)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, "SET LOCAL maintenance_work_mem = '256MB'"); err != nil {
		return err
	}

	hdr, err := tr.Next()
	for err == nil {
		switch {
		case strings.HasPrefix(hdr.Name, "tables/"):
			table := path.Dir(strings.TrimPrefix(hdr.Name, "tables/"))
			t, ok := byName[table]
			if !ok {
				return fmt.Errorf("the snapshot holds data for %s, which its contents do not list", table)
			}
			tbl := &tableReader{tr: tr, table: table, hash: sha256.New()}
			tag, cerr := tx.Conn().PgConn().CopyFrom(ctx, tbl, fmt.Sprintf("COPY %s (%s) FROM STDIN WITH (FORMAT binary)", ident(t.Name), idents(t.Columns)))
			if cerr != nil {
				return fmt.Errorf("loading %s: %w", table, cerr)
			}
			sum.Rows[table] = tag.RowsAffected()
			sum.SHA256[table] = hex.EncodeToString(tbl.hash.Sum(nil))
			hdr, err = tbl.next, tbl.err
			continue
		case strings.HasPrefix(hdr.Name, "attachments/"):
			if attachmentsDir != "" {
				if err := writeFile(attachmentsDir, strings.TrimPrefix(hdr.Name, "attachments/"), tr); err != nil {
					return err
				}
			}
		case hdr.Name == "totals.json":
			var want Totals
			if err := json.NewDecoder(tr).Decode(&want); err != nil {
				return fmt.Errorf("the snapshot's totals cannot be read: %w", err)
			}
			for table, n := range want.Rows {
				if sum.Rows[table] != n || sum.SHA256[table] != want.SHA256[table] {
					return fmt.Errorf("the table %s does not match the snapshot's totals (%d rows loaded, %d expected): the file is damaged", table, sum.Rows[table], n)
				}
			}
			sum.AttachmentFiles, sum.AttachmentBytes = want.AttachmentFiles, want.AttachmentBytes
			if err := setSequences(ctx, tx, sum.Sequences); err != nil {
				return err
			}
			for _, ix := range indexes {
				if _, err := tx.Exec(ctx, ix.def); err != nil {
					return fmt.Errorf("rebuilding the index %s: %w", ix.name, err)
				}
			}
			for _, fk := range keys {
				if _, err := tx.Exec(ctx, "ALTER TABLE "+ident(fk.table)+" ADD CONSTRAINT "+ident(fk.name)+" "+fk.def); err != nil {
					return fmt.Errorf("the snapshot breaks %s on %s: %w", fk.name, fk.table, err)
				}
			}
			for _, t := range current {
				if _, err := tx.Exec(ctx, "ALTER TABLE "+ident(t.Name)+" ENABLE TRIGGER USER"); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, "SELECT pg_notify($1, '*')", apps.NotifyChannel)
			return err
		}
		hdr, err = tr.Next()
	}
	if errors.Is(err, io.EOF) {
		return errors.New("the snapshot ends before its totals: it is incomplete")
	}
	return fmt.Errorf("reading the snapshot: %w", err)
}

// Verify reads a whole snapshot without restoring it: the key opens it, and every table's rows and checksum match its
// totals. Nothing is written.
func Verify(r io.Reader, keys *secrets.Keyring) (*Summary, error) {
	br := bufio.NewReader(r)
	head, err := ReadHeader(br)
	if err != nil {
		return nil, err
	}
	identity, err := identityFor(keys, head.KeyID)
	if err != nil {
		return nil, err
	}
	dec, err := age.Decrypt(br, identity)
	if err != nil {
		return nil, fmt.Errorf("the snapshot cannot be decrypted with this server's secrets key: %w", err)
	}
	zr, err := zstd.NewReader(dec)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	sum := &Summary{Totals: Totals{Rows: map[string]int64{}, SHA256: map[string]string{}}}
	hashes := map[string]hash.Hash{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the snapshot ends before its totals: it is incomplete")
		}
		if err != nil {
			return nil, fmt.Errorf("reading the snapshot: %w", err)
		}
		switch {
		case hdr.Name == "snapshot.json":
			if err := json.NewDecoder(tr).Decode(&sum.Contents); err != nil {
				return nil, err
			}
		case strings.HasPrefix(hdr.Name, "tables/"):
			table := path.Dir(strings.TrimPrefix(hdr.Name, "tables/"))
			if hashes[table] == nil {
				hashes[table] = sha256.New()
			}
			if _, err := io.Copy(hashes[table], tr); err != nil { //nolint:gosec // age authenticates the stream: only the key's holder can make it
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
					return nil, fmt.Errorf("the table %s does not match the snapshot's totals: the file is damaged", table)
				}
			}
			return sum, nil
		}
	}
}

// ---- reading the schema ----

// tablesInOrder lists the tables a snapshot holds, parents before the tables that refer to them (there are no cycles
// between tables; a table referring to itself is loaded in one COPY, which checks its keys at the end).
func tablesInOrder(ctx context.Context, tx pgx.Tx) ([]Table, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.relname, array_agg(a.attname ORDER BY a.attnum)
		FROM pg_class c
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''
		WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r' AND NOT c.relname = ANY ($1)
		GROUP BY c.relname ORDER BY c.relname`, append(slices.Clone(skipped), kept...))
	if err != nil {
		return nil, err
	}
	tables, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Table, error) {
		var t Table
		return t, r.Scan(&t.Name, &t.Columns)
	})
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `
		SELECT DISTINCT conrelid::regclass::text, confrelid::regclass::text FROM pg_constraint
		WHERE contype = 'f' AND conrelid <> confrelid AND connamespace = 'public'::regnamespace`)
	if err != nil {
		return nil, err
	}
	parents := map[string][]string{}
	err = forEach(rows, func(r pgx.Rows) error {
		var child, parent string
		if err := r.Scan(&child, &parent); err != nil {
			return err
		}
		parents[child] = append(parents[child], parent)
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []Table
	done := map[string]bool{}
	for len(out) < len(tables) {
		progress := false
		for _, t := range tables {
			if done[t.Name] {
				continue
			}
			ready := true
			for _, p := range parents[t.Name] {
				if !done[p] && !slices.Contains(skipped, p) && !slices.Contains(kept, p) {
					ready = false
					break
				}
			}
			if ready {
				out, done[t.Name], progress = append(out, t), true, true
			}
		}
		if !progress {
			return nil, errors.New("snapshot: the tables refer to each other in a cycle")
		}
	}
	return out, nil
}

type index struct{ name, def string }

// plainIndexes are the indexes on public tables that no constraint owns, with what makes them again.
func plainIndexes(ctx context.Context, tx pgx.Tx) ([]index, error) {
	rows, err := tx.Query(ctx, `
		SELECT i.relname, pg_get_indexdef(i.oid)
		FROM pg_index x
		JOIN pg_class i ON i.oid = x.indexrelid
		JOIN pg_class t ON t.oid = x.indrelid
		WHERE t.relnamespace = 'public'::regnamespace AND t.relkind = 'r' AND t.relname <> 'goose_db_version'
		  AND NOT t.relname = ANY ($1)
		  AND NOT EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = i.oid)
		ORDER BY i.relname`, kept)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (index, error) {
		var ix index
		return ix, r.Scan(&ix.name, &ix.def)
	})
}

type foreignKey struct{ table, name, def string }

// foreignKeys are the foreign keys between public tables, with what makes them again.
func foreignKeys(ctx context.Context, tx pgx.Tx) ([]foreignKey, error) {
	rows, err := tx.Query(ctx, `
		SELECT conrelid::regclass::text, conname, pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE contype = 'f' AND connamespace = 'public'::regnamespace AND NOT conrelid::regclass::text = ANY ($1)
		ORDER BY conrelid::regclass::text, conname`, kept)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (foreignKey, error) {
		var fk foreignKey
		return fk, r.Scan(&fk.table, &fk.name, &fk.def)
	})
}

func sequences(ctx context.Context, tx pgx.Tx) ([]Sequence, error) {
	rows, err := tx.Query(ctx, `SELECT sequencename, last_value FROM pg_sequences
		WHERE schemaname = 'public' AND sequencename NOT LIKE 'goose%' ORDER BY sequencename`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Sequence, error) {
		var s Sequence
		return s, r.Scan(&s.Name, &s.Value)
	})
}

// setSequences puts every sequence where it was. A record type's numbering sequence is made by a trigger when the
// type is added, which loading skips, so any that is missing is made here.
func setSequences(ctx context.Context, tx pgx.Tx, seqs []Sequence) error {
	for _, s := range seqs {
		if _, err := tx.Exec(ctx, "CREATE SEQUENCE IF NOT EXISTS "+ident(s.Name)); err != nil {
			return err
		}
		var err error
		if s.Value == nil {
			_, err = tx.Exec(ctx, "SELECT setval($1::regclass, 1, false)", s.Name)
		} else {
			_, err = tx.Exec(ctx, "SELECT setval($1::regclass, $2, true)", s.Name, *s.Value)
		}
		if err != nil {
			return fmt.Errorf("setting the sequence %s: %w", s.Name, err)
		}
	}
	return nil
}

func installedApps(ctx context.Context, tx pgx.Tx) ([]AppAt, error) {
	rows, err := tx.Query(ctx, `SELECT name, version FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AppAt, error) {
		var a AppAt
		return a, r.Scan(&a.Name, &a.Version)
	})
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

// ---- attachments ----

func addAttachments(ctx context.Context, tx pgx.Tx, tw *tar.Writer, dir string, totals *Totals) error {
	rows, err := tx.Query(ctx, `SELECT key FROM attachments WHERE backend = 'local' AND status = 'ready' ORDER BY key`)
	if err != nil {
		return err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, key := range keys {
		full, err := within(dir, key)
		if err != nil {
			return err
		}
		f, err := os.Open(full) //nolint:gosec // a key from the attachments table, kept within dir by within()
		if errors.Is(err, os.ErrNotExist) {
			continue // a file the folder lost; the row is restored, and opening it says it is missing
		}
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err == nil {
			err = tw.WriteHeader(&tar.Header{Name: "attachments/" + key, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime()})
		}
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("snapshot: adding the file %s: %w", key, err)
		}
		totals.AttachmentFiles++
		totals.AttachmentBytes += info.Size()
	}
	return nil
}

func writeFile(dir, key string, r io.Reader) error {
	full, err := within(dir, key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	tmp := full + ".restoring"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // kept within dir by within()
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, full)
}

// within joins an attachment key to the folder, refusing one that would leave it.
func within(dir, key string) (string, error) {
	full := filepath.Join(dir, filepath.FromSlash(key))
	rel, err := filepath.Rel(dir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(key) {
		return "", fmt.Errorf("snapshot: the attachment key %q leaves the folder", key)
	}
	return full, nil
}

// ---- writing and reading the archive ----

// chunker cuts one table's COPY output into tar entries tables/<table>/<n> of at most chunk bytes.
type chunker struct {
	tw    *tar.Writer
	table string
	n     int
	buf   []byte
	hash  hash.Hash
}

func (c *chunker) Write(p []byte) (int, error) {
	written := len(p)
	c.hash.Write(p)
	for len(p) > 0 {
		room := chunk - len(c.buf)
		take := min(room, len(p))
		c.buf = append(c.buf, p[:take]...)
		p = p[take:]
		if len(c.buf) == chunk {
			if err := c.flush(); err != nil {
				return 0, err
			}
		}
	}
	return written, nil
}

func (c *chunker) flush() error {
	if len(c.buf) == 0 && c.n > 0 {
		return nil
	}
	c.n++
	if err := c.tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("tables/%s/%06d", c.table, c.n), Mode: 0o600, Size: int64(len(c.buf))}); err != nil {
		return err
	}
	if _, err := c.tw.Write(c.buf); err != nil {
		return err
	}
	c.buf = c.buf[:0]
	return nil
}

// tableReader reads one table's entries as one stream, and keeps the first entry after them (or the error that
// ended them) for the caller.
type tableReader struct {
	tr    *tar.Reader
	table string
	hash  hash.Hash
	next  *tar.Header
	err   error
	done  bool
}

func (t *tableReader) Read(p []byte) (int, error) {
	for !t.done {
		n, err := t.tr.Read(p)
		if n > 0 {
			t.hash.Write(p[:n])
			return n, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		hdr, err := t.tr.Next()
		if err != nil || !strings.HasPrefix(hdr.Name, "tables/"+t.table+"/") {
			t.next, t.err, t.done = hdr, err, true
		}
	}
	return 0, io.EOF
}

func writeJSON(tw *tar.Writer, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}); err != nil {
		return err
	}
	_, err = tw.Write(data)
	return err
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

func idents(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = ident(n)
	}
	return strings.Join(out, ", ")
}

// ---- keys ----

func passphrase(keys *secrets.Keyring, keyID string) (string, error) {
	key, err := keys.Derive(keyID, "snapshot")
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}

func recipientFor(keys *secrets.Keyring, keyID string) (age.Recipient, error) {
	p, err := passphrase(keys, keyID)
	if err != nil {
		return nil, err
	}
	r, err := age.NewScryptRecipient(p)
	if err != nil {
		return nil, err
	}
	r.SetWorkFactor(workFactor)
	return r, nil
}

func identityFor(keys *secrets.Keyring, keyID string) (age.Identity, error) {
	p, err := passphrase(keys, keyID)
	if err != nil {
		return nil, fmt.Errorf("the snapshot was taken with the secrets key %s, which this server does not have (set it as SL_SECRETS_KEY or in SL_SECRETS_OLD_KEYS)", keyID)
	}
	id, err := age.NewScryptIdentity(p)
	if err != nil {
		return nil, err
	}
	id.SetMaxWorkFactor(workFactor)
	return id, nil
}
