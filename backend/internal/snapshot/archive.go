package snapshot

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// What clone packages (internal/clone) share with snapshots: the same archive layout, reading the schema the same
// way, and the same table entries.

// AllTables lists every table but leaveOut, parents before the tables that refer to them, with their columns in COPY
// order (generated ones left out).
func AllTables(ctx context.Context, tx pgx.Tx, leaveOut []string) ([]Table, error) {
	if leaveOut == nil {
		leaveOut = []string{} // NOT relname = ANY (NULL) would leave out everything
	}
	return tablesListed(ctx, tx, leaveOut)
}

// Sequences reads every sequence's position.
func Sequences(ctx context.Context, tx pgx.Tx) ([]Sequence, error) { return sequences(ctx, tx) }

// InstalledApps lists the installed apps and their versions.
func InstalledApps(ctx context.Context, tx pgx.Tx) ([]AppAt, error) { return installedApps(ctx, tx) }

// CopyOut writes the binary COPY output of query (a SELECT giving a table's columns in order) as the table's entries,
// and gives its rows and SHA-256.
func CopyOut(ctx context.Context, tx pgx.Tx, tw *tar.Writer, table, query string) (int64, string, error) {
	cw := &chunker{tw: tw, table: table, hash: sha256.New(), buf: make([]byte, 0, chunk)}
	tag, err := tx.Conn().PgConn().CopyTo(ctx, cw, "COPY ("+query+") TO STDOUT WITH (FORMAT binary)")
	if err != nil {
		return 0, "", fmt.Errorf("copying %s: %w", table, err)
	}
	if err := cw.flush(); err != nil {
		return 0, "", err
	}
	return tag.RowsAffected(), hex.EncodeToString(cw.hash.Sum(nil)), nil
}

// WriteJSON adds one JSON entry.
func WriteJSON(tw *tar.Writer, name string, v any) error { return writeJSON(tw, name, v) }

// Ident quotes a name for SQL.
func Ident(name string) string { return ident(name) }

// CopyIn loads one table's entries, the first of which tr is at, into the table into (the table itself, or a
// temporary copy of it) with COPY FROM, and gives its rows, its SHA-256 and the entry after it (or the error that
// ended the archive).
func CopyIn(ctx context.Context, tx pgx.Tx, tr *tar.Reader, table, into string, columns []string) (int64, string, *tar.Header, error) {
	tbl := &tableReader{tr: tr, table: table, hash: sha256.New()}
	tag, err := tx.Conn().PgConn().CopyFrom(ctx, tbl, fmt.Sprintf("COPY %s (%s) FROM STDIN WITH (FORMAT binary)", ident(into), idents(columns)))
	if err != nil {
		return 0, "", nil, fmt.Errorf("loading %s: %w", table, err)
	}
	return tag.RowsAffected(), hex.EncodeToString(tbl.hash.Sum(nil)), tbl.next, tbl.err
}

// Unindex drops the plain indexes and the foreign keys of every table but the snapshot catalog, as a restore does
// before loading, and gives what puts them back (checking every row against the keys at once).
func Unindex(ctx context.Context, tx pgx.Tx) (func(context.Context) error, error) {
	indexes, err := plainIndexes(ctx, tx)
	if err != nil {
		return nil, err
	}
	keys, err := foreignKeys(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, ix := range indexes {
		if _, err := tx.Exec(ctx, "DROP INDEX "+ident(ix.name)); err != nil {
			return nil, err
		}
	}
	for _, fk := range keys {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+ident(fk.table)+" DROP CONSTRAINT "+ident(fk.name)); err != nil {
			return nil, err
		}
	}
	return func(ctx context.Context) error {
		for _, ix := range indexes {
			if _, err := tx.Exec(ctx, ix.def); err != nil {
				return fmt.Errorf("rebuilding the index %s: %w", ix.name, err)
			}
		}
		for _, fk := range keys {
			if _, err := tx.Exec(ctx, "ALTER TABLE "+ident(fk.table)+" ADD CONSTRAINT "+ident(fk.name)+" "+fk.def); err != nil {
				return fmt.Errorf("the data breaks %s on %s: %w", fk.name, fk.table, err)
			}
		}
		return nil
	}, nil
}

// SetSequences puts sequences where they were, making any that is missing (a record type's numbering).
func SetSequences(ctx context.Context, tx pgx.Tx, seqs []Sequence) error {
	return setSequences(ctx, tx, seqs)
}

// Kept are the tables that record snapshots, restores and clones: never in a snapshot or a clone, never emptied.
func Kept() []string { return slices.Clone(kept) }
