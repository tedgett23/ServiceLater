// Package dbtest gives tests a migrated, empty scratch database.
package dbtest

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
)

// Pool needs SL_TEST_DATABASE_URL pointing at a scratch database; its tables are truncated on every call.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("SL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SL_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil { // idempotent
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE tickets, sessions, rate_limits, user_credentials, users CASCADE"); err != nil {
		t.Fatal(err)
	}
	return pool
}
