package db_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/servicelater/servicelater/backend/db/migrations"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
)

// migrator gives a test control over which migration version a database is at.
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

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s\n%v", query, err)
	}
}

func scalar[T any](t *testing.T, db *sql.DB, query string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&v); err != nil {
		t.Fatalf("%s\n%v", query, err)
	}
	return v
}

func fails(t *testing.T, db *sql.DB, why, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err == nil {
		t.Errorf("%s: was accepted", why)
	}
}

// Existing instances run this migration on real data, so it is tested that way: a populated database at the
// previous version is migrated forward, checked, rolled back, and migrated forward again.
func TestTasksMigrationKeepsExistingData(t *testing.T) {
	ctx := context.Background()
	p, db := migrator(t)
	if _, err := p.UpTo(ctx, 8); err != nil {
		t.Fatal(err)
	}

	// A populated database as it was before: tickets in every status, with comments.
	exec(t, db, `INSERT INTO users (id, email, name, role) VALUES
		('00000000-0000-0000-0000-00000000000a', 'a@x.test', 'Ann', 'end_user'),
		('00000000-0000-0000-0000-00000000000b', 'b@x.test', 'Bo', 'agent')`)
	for _, s := range [][3]string{{"one", "new", "open"}, {"two", "on_hold", "waiting"}, {"three", "closed", "closed"}} {
		exec(t, db, `INSERT INTO tickets (title, requester_id, assignee_id, status, state_category)
			VALUES ($1, '00000000-0000-0000-0000-00000000000a', '00000000-0000-0000-0000-00000000000b', $2, $3)`, s[0], s[1], s[2])
	}
	exec(t, db, `INSERT INTO ticket_comments (ticket_id, author_id, body, internal)
		SELECT id, '00000000-0000-0000-0000-00000000000b', 'note on ' || title, title = 'two' FROM tickets WHERE title IN ('one', 'two')`)
	exec(t, db, `UPDATE tickets SET title = 'three (edited)' WHERE title = 'three'`)
	before := scalar[string](t, db, `SELECT string_agg(id::text || ':' || number || ':' || status || ':' || state_category, ',' ORDER BY number) FROM tickets`)

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("migrating up: %v", err)
	}

	t.Run("every ticket became an incident with the same id, number and status", func(t *testing.T) {
		got := scalar[string](t, db, `SELECT string_agg(id::text || ':' || number || ':' || status || ':' || state_category, ',' ORDER BY number) FROM tasks WHERE type = 'incident'`)
		if got != before {
			t.Errorf("tasks differ from the old tickets:\n before %s\n after  %s", before, got)
		}
		if n := scalar[int](t, db, `SELECT count(*) FROM incidents`); n != 3 {
			t.Errorf("%d incident rows, want 3", n)
		}
		if n := scalar[int](t, db, `SELECT count(*) FROM tasks WHERE title = 'three (edited)'`); n != 1 {
			t.Error("edits were lost")
		}
	})

	t.Run("comments follow, with a real foreign key", func(t *testing.T) {
		if n := scalar[int](t, db, `SELECT count(*) FROM task_comments c JOIN tasks t ON t.id = c.task_id`); n != 2 {
			t.Errorf("%d comments reach their task, want 2", n)
		}
		fails(t, db, "a comment for a task that does not exist", `INSERT INTO task_comments (task_id, author_id, body)
			VALUES (gen_random_uuid(), '00000000-0000-0000-0000-00000000000b', 'orphan')`)
		exec(t, db, `DELETE FROM tasks WHERE title = 'one'`)
		if n := scalar[int](t, db, `SELECT count(*) FROM task_comments WHERE body = 'note on one'`); n != 0 {
			t.Error("deleting a task should delete its comments")
		}
	})

	t.Run("numbering carries on, and is per type", func(t *testing.T) {
		exec(t, db, `INSERT INTO tasks (type, title, requester_id) VALUES ('incident', 'next', '00000000-0000-0000-0000-00000000000a')`)
		if n := scalar[int](t, db, `SELECT number FROM tasks WHERE title = 'next'`); n != 4 {
			t.Errorf("the next incident is number %d, want 4 (after the migrated 1-3)", n)
		}
		exec(t, db, `INSERT INTO tasks (type, title, requester_id) VALUES ('change', 'first change', '00000000-0000-0000-0000-00000000000a')`)
		if n := scalar[int](t, db, `SELECT number FROM tasks WHERE title = 'first change'`); n != 1 {
			t.Errorf("the first change is number %d, want 1: each type numbers on its own", n)
		}
		if got := scalar[string](t, db, `SELECT tt.prefix || t.number FROM tasks t JOIN task_types tt ON tt.type = t.type WHERE title = 'first change'`); got != "CHG1" {
			t.Errorf("reference %q", got)
		}
		fails(t, db, "a duplicate number within a type", `INSERT INTO tasks (type, number, title, requester_id) VALUES ('incident', 4, 'dup', '00000000-0000-0000-0000-00000000000a')`)
	})

	t.Run("types are checked", func(t *testing.T) {
		fails(t, db, "an unregistered type", `INSERT INTO tasks (type, title, requester_id) VALUES ('widget', 'x', '00000000-0000-0000-0000-00000000000a')`)
		fails(t, db, "a badly formed type name", `INSERT INTO task_types (type, prefix, label) VALUES ('Bad-Type', 'BAD', 'x')`)
		fails(t, db, "a duplicate prefix", `INSERT INTO task_types (type, prefix, label) VALUES ('other', 'INC', 'x')`)
	})

	t.Run("an incident row cannot hang off a task of another type", func(t *testing.T) {
		fails(t, db, "an incident extension for a change", `INSERT INTO incidents (task_id) SELECT id FROM tasks WHERE type = 'change'`)
	})

	t.Run("a new type gets numbering with no further code", func(t *testing.T) {
		exec(t, db, `INSERT INTO task_types (type, prefix, label) VALUES ('hardware_order', 'HWO', 'Hardware order')`)
		exec(t, db, `INSERT INTO tasks (type, title, requester_id) VALUES ('hardware_order', 'a laptop', '00000000-0000-0000-0000-00000000000a')`)
		if n := scalar[int](t, db, `SELECT number FROM tasks WHERE type = 'hardware_order'`); n != 1 {
			t.Errorf("number %d", n)
		}
		exec(t, db, `DELETE FROM tasks WHERE type = 'hardware_order'`)
		exec(t, db, `DELETE FROM task_types WHERE type = 'hardware_order'`)
	})

	t.Run("changing the incident row touches the task", func(t *testing.T) {
		exec(t, db, `UPDATE tasks SET updated_at = now() - interval '1 day' WHERE title = 'next'`)
		exec(t, db, `INSERT INTO incidents (task_id) SELECT id FROM tasks WHERE title = 'next' ON CONFLICT DO NOTHING`)
		exec(t, db, `UPDATE incidents SET resolution = 'fixed it' WHERE task_id = (SELECT id FROM tasks WHERE title = 'next')`)
		if n := scalar[int](t, db, `SELECT count(*) FROM tasks WHERE title = 'next' AND updated_at > now() - interval '1 minute'`); n != 1 {
			t.Error("updating an incident's own columns must bump the task's updated_at")
		}
	})

	t.Run("the migrations after the tasks one roll back cleanly and forward again", func(t *testing.T) {
		// Rolled back by version, not by count, so a new migration never shifts which one this test exercises.
		if _, err := p.DownTo(ctx, 9); err != nil {
			t.Fatalf("rolling back to the tasks migration: %v", err)
		}
		for _, gone := range []string{"jobs", "action_runs"} {
			if n := scalar[int](t, db, `SELECT count(*) FROM pg_class WHERE relname = '`+gone+`'`); n != 0 {
				t.Errorf("the %s table survived its rollback", gone)
			}
		}
		if n := scalar[int](t, db, `SELECT count(*) FROM pg_proc WHERE proname = 'enqueue_job'`); n != 0 {
			t.Error("enqueue_job survived its rollback")
		}
		if _, err := p.Up(ctx); err != nil {
			t.Fatalf("migrating up again: %v", err)
		}
		for _, back := range []string{"jobs", "action_runs"} {
			if n := scalar[int](t, db, `SELECT count(*) FROM pg_class WHERE relname = '`+back+`'`); n != 1 {
				t.Errorf("the %s table is missing after migrating up again", back)
			}
		}
	})

	t.Run("rolling back is refused while another type exists, then works", func(t *testing.T) {
		if _, err := p.DownTo(ctx, 9); err != nil { // the later migrations again, so the next step is the tasks migration
			t.Fatalf("rolling back to the tasks migration: %v", err)
		}
		if _, err := p.Down(ctx); err == nil || !strings.Contains(err.Error(), "tasks of other types exist") {
			t.Fatalf("rollback with a change present should be refused: %v", err)
		}
		exec(t, db, `DELETE FROM tasks WHERE type <> 'incident'`)
		if _, err := p.Down(ctx); err != nil {
			t.Fatalf("rolling back: %v", err)
		}
		if n := scalar[int](t, db, `SELECT count(*) FROM tickets`); n != 3 {
			t.Errorf("%d tickets after rollback, want 3 (two originals, one new, one deleted earlier)", n)
		}
		if n := scalar[int](t, db, `SELECT count(*) FROM ticket_comments`); n != 1 {
			t.Errorf("%d comments after rollback, want 1", n)
		}
		// Identity numbering resumes after the highest number.
		exec(t, db, `INSERT INTO tickets (title, requester_id) VALUES ('after rollback', '00000000-0000-0000-0000-00000000000a')`)
		if n := scalar[int](t, db, `SELECT number FROM tickets WHERE title = 'after rollback'`); n != 5 {
			t.Errorf("next number after rollback is %d, want 5", n)
		}
	})

	t.Run("and forward again", func(t *testing.T) {
		if _, err := p.Up(ctx); err != nil {
			t.Fatalf("migrating up a second time: %v", err)
		}
		if n := scalar[int](t, db, `SELECT count(*) FROM tasks`); n != 4 {
			t.Errorf("%d tasks, want 4", n)
		}
	})
}
