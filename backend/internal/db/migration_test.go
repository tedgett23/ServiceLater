package db_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/servicelater/servicelater/backend/db/migrations"
	sldb "github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
)

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool, err := sldb.Connect(ctx, dbtest.FreshDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for range 2 {
		if err := sldb.Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
}

// migrator gives a test control over which migration version a fresh database is at.
func migrator(t *testing.T) (*goose.Provider, *sql.DB) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dbtest.FreshDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	return p, sqlDB
}

func scalar[T any](t *testing.T, db *sql.DB, query string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&v); err != nil {
		t.Fatalf("%s\n%v", query, err)
	}
	return v
}

// names runs a query giving one text column.
func names(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatalf("%s\n%v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// What the migrations made: every table, column, function, policy, trigger, index and sequence in the schema, by name.
const schemaObjects = `SELECT count(*)::bigint || ':' || md5(coalesce(string_agg(o, ',' ORDER BY o), '')) FROM (
	SELECT 'table ' || c.relname || ' ' || c.relkind::text AS o FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	 WHERE n.nspname = 'public' AND c.relname NOT LIKE 'goose%'
	UNION ALL SELECT 'column ' || table_name || '.' || column_name || ' ' || data_type FROM information_schema.columns
	 WHERE table_schema = 'public' AND table_name NOT LIKE 'goose%'
	UNION ALL SELECT 'function ' || p.oid::regprocedure::text FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
	UNION ALL SELECT 'policy ' || tablename || '.' || policyname FROM pg_policies WHERE schemaname = 'public'
	UNION ALL SELECT 'trigger ' || tgrelid::regclass::text || '.' || tgname FROM pg_trigger WHERE NOT tgisinternal
) x`

// Every migration goes down as well as up: all the way down leaves nothing behind, and up again builds the same schema.
func TestMigrationsGoDownAndUpAgain(t *testing.T) {
	ctx := context.Background()
	p, db := migrator(t)
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	built := scalar[string](t, db, schemaObjects)
	if roles := scalar[int](t, db, `SELECT count(*) FROM roles`); roles != 3 {
		t.Errorf("the built-in roles are seeded: %d", roles)
	}
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	if left := names(t, db, `SELECT relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND relname NOT LIKE 'goose%'
		UNION ALL SELECT p.oid::regprocedure::text FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace`); len(left) > 0 {
		t.Errorf("going all the way down left %v", left)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if again := scalar[string](t, db, schemaObjects); again != built {
		t.Errorf("up again built a different schema: %s, then %s", built, again)
	}
}

// Every table has row-level security on: one without it would be open to every signed-in person through the app role.
// Owner-only tables have it on with no policies (and their grants revoked).
func TestEveryTableHasRowLevelSecurity(t *testing.T) {
	p, db := migrator(t)
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range names(t, db, `SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r' AND NOT c.relrowsecurity AND c.relname NOT LIKE 'goose%' ORDER BY 1`) {
		t.Errorf("%s has row-level security off", name)
	}
}
