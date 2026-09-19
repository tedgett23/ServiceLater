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

	create := func(who uuid.UUID, title string) (dbgen.Ticket, error) {
		var tk dbgen.Ticket
		err := db.WithUser(ctx, pool, who, func(tx pgx.Tx) error {
			var err error
			tk, err = dbgen.New(tx).CreateTicket(ctx, dbgen.CreateTicketParams{Title: title, Priority: 3, RequesterID: who})
			return err
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

	if got := list(alice); len(got) != 1 || got[0].Ticket.ID != aliceT.ID {
		t.Errorf("alice should see only her ticket, got %d", len(got))
	}
	if got := list(agent); len(got) != 2 {
		t.Errorf("agent should see all tickets, got %d", len(got))
	}

	// An end user cannot file a ticket on someone else's behalf.
	err = db.WithUser(ctx, pool, alice, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).CreateTicket(ctx, dbgen.CreateTicketParams{Title: "spoof", Priority: 3, RequesterID: bob})
		return err
	})
	if err == nil {
		t.Error("expected RLS to reject spoofed requester")
	}

	// An end user cannot update tickets (zero rows visible to UPDATE).
	err = db.WithUser(ctx, pool, alice, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).UpdateTicketStatus(ctx, dbgen.UpdateTicketStatusParams{ID: aliceT.ID, Status: "closed"})
		return err
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("end user update: want ErrNoRows, got %v", err)
	}
	// An agent can.
	err = db.WithUser(ctx, pool, agent, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).UpdateTicketStatus(ctx, dbgen.UpdateTicketStatusParams{ID: aliceT.ID, Status: "in_progress"})
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
		_, err := dbgen.New(tx).CreateTicket(ctx, dbgen.CreateTicketParams{Title: "t", Priority: 3, RequesterID: alice})
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
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM tickets").Scan(&n); err != nil {
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
