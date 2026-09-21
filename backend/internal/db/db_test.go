package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
)

func testPool(t *testing.T) *pgxpool.Pool { return dbtest.Pool(t) }

func mkUser(t *testing.T, pool *pgxpool.Pool, email, role string) uuid.UUID {
	t.Helper()
	var u dbgen.User
	err := db.WithSystem(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		u, err = dbgen.New(tx).CreateUser(context.Background(), dbgen.CreateUserParams{Email: email, Name: email, Role: role})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func TestTicketRLS(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	alice := mkUser(t, pool, "alice@x", "end_user")
	bob := mkUser(t, pool, "bob@x", "end_user")
	agent := mkUser(t, pool, "agent@x", "agent")

	create := func(who uuid.UUID, title string) (dbgen.Task, error) {
		var tk dbgen.Task
		err := db.WithUser(ctx, pool, who, func(tx pgx.Tx) error {
			var err error
			q := dbgen.New(tx)
			if tk, err = q.CreateTask(ctx, dbgen.CreateTaskParams{Type: "incident", Title: title, Priority: 3, RequesterID: who, Status: "new", StateCategory: "open"}); err != nil {
				return err
			}
			return q.CreateIncident(ctx, tk.ID)
		})
		return tk, err
	}
	list := func(who uuid.UUID) []dbgen.ListTicketsRow {
		var out []dbgen.ListTicketsRow
		err := db.WithUser(ctx, pool, who, func(tx pgx.Tx) error {
			var err error
			out, err = dbgen.New(tx).ListTickets(ctx, dbgen.ListTicketsParams{Limit: 100})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	aliceT, err := create(alice, "alice's laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := create(bob, "bob's vpn"); err != nil {
		t.Fatal(err)
	}

	if got := list(alice); len(got) != 1 || got[0].Task.ID != aliceT.ID {
		t.Errorf("alice should see only her ticket, got %d", len(got))
	}
	if got := list(agent); len(got) != 2 {
		t.Errorf("agent should see all tickets, got %d", len(got))
	}

	// An end user cannot file a ticket on someone else's behalf.
	err = db.WithUser(ctx, pool, alice, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).CreateTask(ctx, dbgen.CreateTaskParams{Type: "incident", Title: "spoof", Priority: 3, RequesterID: bob, Status: "new", StateCategory: "open"})
		return err
	})
	if err == nil {
		t.Error("expected RLS to reject spoofed requester")
	}

	// An end user cannot update tickets (zero rows visible to UPDATE).
	err = db.WithUser(ctx, pool, alice, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).UpdateTaskStatus(ctx, dbgen.UpdateTaskStatusParams{ID: aliceT.ID, Status: "closed", StateCategory: "closed"})
		return err
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("end user update: want ErrNoRows, got %v", err)
	}
	// An agent can.
	err = db.WithUser(ctx, pool, agent, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).UpdateTaskStatus(ctx, dbgen.UpdateTaskStatusParams{ID: aliceT.ID, Status: "in_progress", StateCategory: "open"})
		return err
	})
	if err != nil {
		t.Errorf("agent update: %v", err)
	}

	// Identity must not leak to the next user of the pooled connection.
	var role string
	var uid *string
	if err := pool.QueryRow(ctx, "SELECT current_user, nullif(current_setting('app.current_user_id', true), '')").Scan(&role, &uid); err != nil {
		t.Fatal(err)
	}
	if role == "servicelater_app" || uid != nil {
		t.Errorf("identity leaked after tx: role=%s uid=%v", role, uid)
	}
}

func TestNoUserSeesNothing(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	alice := mkUser(t, pool, "alice@x", "end_user")
	if err := db.WithUser(ctx, pool, alice, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).CreateTask(ctx, dbgen.CreateTaskParams{Type: "incident", Title: "t", Priority: 3, RequesterID: alice, Status: "new", StateCategory: "open"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// servicelater_app with no user set: no rows, no directory.
	err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE servicelater_app"); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM tasks").Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("tickets visible without a user: %d", n)
		}
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("users visible without a user: %d", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An incident's own row is only ever as visible, and as editable, as its task.
func TestExtensionRowsFollowTheirTask(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	alice := mkUser(t, pool, "alice@x", "end_user")
	bob := mkUser(t, pool, "bob@x", "end_user")
	agent := mkUser(t, pool, "agent@x", "agent")

	var aliceTask dbgen.Task
	if err := db.WithUser(ctx, pool, alice, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var err error
		if aliceTask, err = q.CreateTask(ctx, dbgen.CreateTaskParams{Type: "incident", Title: "a", Priority: 3, RequesterID: alice, Status: "new", StateCategory: "open"}); err != nil {
			return err
		}
		return q.CreateIncident(ctx, aliceTask.ID)
	}); err != nil {
		t.Fatal(err)
	}

	count := func(who uuid.UUID, query string, args ...any) (n int) {
		t.Helper()
		if err := db.WithUser(ctx, pool, who, func(tx pgx.Tx) error { return tx.QueryRow(ctx, query, args...).Scan(&n) }); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(alice, "SELECT count(*) FROM incidents"); n != 1 {
		t.Errorf("the requester sees %d incident rows of their own, want 1", n)
	}
	if n := count(bob, "SELECT count(*) FROM incidents"); n != 0 {
		t.Errorf("another user sees %d of alice's incident rows, want 0", n)
	}
	if n := count(agent, "SELECT count(*) FROM incidents"); n != 1 {
		t.Errorf("staff see %d incident rows, want 1", n)
	}

	// Only staff change it. For an end user the row is simply not updatable, so zero rows change.
	update := func(who uuid.UUID) int64 {
		t.Helper()
		var n int64
		if err := db.WithUser(ctx, pool, who, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, "UPDATE incidents SET resolution = 'fixed' WHERE task_id = $1", aliceTask.ID)
			n = tag.RowsAffected()
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := update(alice); n != 0 {
		t.Error("a requester must not be able to write resolution notes")
	}
	if n := update(agent); n != 1 {
		t.Error("staff must be able to write resolution notes")
	}

	// Nobody can attach an incident row to a task they cannot see.
	err := db.WithUser(ctx, pool, bob, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO incidents (task_id) VALUES ($1)", aliceTask.ID)
		return err
	})
	if err == nil {
		t.Error("bob attached an incident row to alice's task")
	}
}

// The registry of types is readable by everyone signed in, and writable by nobody but the owner.
func TestTaskTypeRegistryIsReadOnlyForUsers(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	alice := mkUser(t, pool, "alice@x", "end_user")
	agent := mkUser(t, pool, "agent@x", "agent")

	for _, who := range []uuid.UUID{alice, agent} {
		var n int
		if err := db.WithUser(ctx, pool, who, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM task_types").Scan(&n)
		}); err != nil || n < 4 {
			t.Errorf("registry not readable: %d rows, %v", n, err)
		}
		err := db.WithUser(ctx, pool, who, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "INSERT INTO task_types (type, prefix, label) VALUES ('sneaky', 'SNK', 'x')")
			return err
		})
		if err == nil {
			t.Error("a user registered a task type")
		}
	}
}
