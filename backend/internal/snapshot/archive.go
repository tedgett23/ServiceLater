package snapshot

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

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
