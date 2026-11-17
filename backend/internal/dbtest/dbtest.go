// Package dbtest gives tests a migrated, empty scratch database.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
)

// Pool needs SL_TEST_DATABASE_URL pointing at a scratch database; its tables are truncated on every call.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// Tests sign many people up and in; production-strength hashing made that most of the suite's time.
	auth.UseFastHashing()
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
	if _, err := pool.Exec(ctx, "TRUNCATE tasks, sessions, rate_limits, user_credentials, users, apps, jobs, action_runs, workflow_executions, groups, sso_providers CASCADE"); err != nil {
		t.Fatal(err)
	}
	// A sign-in setting left at "admins only" would refuse every later test's password.
	if _, err := pool.Exec(ctx, "DELETE FROM settings WHERE name LIKE 'sign_in.%'"); err != nil {
		t.Fatal(err)
	}
	// Record types apps defined go with the apps, and their numbering with them.
	if _, err := pool.Exec(ctx, DropAppTypes); err != nil {
		t.Fatal(err)
	}
	// apps cascades to roles, built-in ones included.
	if _, err := pool.Exec(ctx, "SELECT app_seed_builtin_roles()"); err != nil {
		t.Fatal(err)
	}
	// users cascades to business calendars: put back the one incident-management's SLAs name.
	if _, err := pool.Exec(ctx, "SELECT app_seed_business_calendars()"); err != nil {
		t.Fatal(err)
	}
	// TRUNCATE leaves the per-type numbering alone, so restart it: every test starts at INC1, CHG1 and so on.
	if _, err := pool.Exec(ctx, ResetNumbering); err != nil {
		t.Fatal(err)
	}
	return pool
}

// DSN is the scratch database's connection string, for tests that need their own connection (LISTEN).
func DSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("SL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SL_TEST_DATABASE_URL not set")
	}
	return dsn
}

// AppsDir is the repository's first-party apps folder.
func AppsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "apps")
}

// InstallFirstParty installs the first-party apps bundled in the binary, as a starting server does, so tests run
// against the real incident lifecycle.
func InstallFirstParty(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	names, err := apps.BundledSelection("")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range apps.SyncBundled(context.Background(), pool, names, "") {
		if r.Err != nil {
			t.Fatalf("install %s: %v", r.App, r.Err)
		}
	}
}

// DropAppTypes deletes the record types apps defined (once their records are gone) and their number sequences.
const DropAppTypes = `DO $$
DECLARE t text;
BEGIN
    FOR t IN DELETE FROM task_types WHERE owner_app IS NOT NULL RETURNING type LOOP
        EXECUTE format('DROP SEQUENCE IF EXISTS %I', 'task_number_' || t);
    END LOOP;
END
$$`

// ResetNumbering restarts every task type's number sequence at 1.
const ResetNumbering = `DO $$
DECLARE t text;
BEGIN
    FOR t IN SELECT type FROM task_types LOOP
        EXECUTE format('ALTER SEQUENCE %I RESTART', 'task_number_' || t);
    END LOOP;
END
$$`

// FreshDatabase creates an empty database for one test and returns its connection string; it is dropped when the
// test ends. It is for tests that must control migrations themselves. It needs the CREATEDB privilege and skips
// the test without it.
func FreshDatabase(t testing.TB) string {
	t.Helper()
	dsn := DSN(t)
	ctx := context.Background()
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("slt_%d_%s", time.Now().UnixNano(), hex.EncodeToString(suffix))

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "42501" {
			t.Skip("creating a database needs the CREATEDB privilege (ALTER ROLE servicelater CREATEDB)")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
