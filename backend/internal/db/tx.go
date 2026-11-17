package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WithUser runs fn in a transaction with Postgres RLS enforced for userID.
//
// The pool connects as the table owner, which bypasses RLS, so the transaction first drops to the
// servicelater_app role, then app_begin sets app.current_user_id and app.permissions (what app_can checks). All of
// it is transaction-local (SET LOCAL semantics) and vanishes at commit/rollback, so a pooled connection never leaks
// identity to the next caller.
//
// All queries on behalf of a user must go through here. Do not use the pool directly for user data.
func WithUser(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, fn func(pgx.Tx) error) error {
	return inTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE servicelater_app"); err != nil {
			return fmt.Errorf("db: set role: %w", err)
		}
		var err error
		if scopes, ok := ctx.Value(scopesKey{}).([]string); ok {
			_, err = tx.Exec(ctx, "SELECT app_begin_key($1, $2)", userID, scopes)
		} else {
			_, err = tx.Exec(ctx, "SELECT app_begin($1)", userID)
		}
		if err != nil {
			return fmt.Errorf("db: set user: %w", err)
		}
		return fn(tx)
	})
}

type scopesKey struct{}

// WithScopes narrows every WithUser transaction made with ctx to scopes (app_narrow_permissions): a request made
// with an API key that may do only some of what its account may.
func WithScopes(ctx context.Context, scopes []string) context.Context {
	return context.WithValue(ctx, scopesKey{}, scopes)
}

// WithPrincipal runs fn as an app's service principal acting on behalf of a person: RLS checks the principal's
// permissions (its app's manifest), and what it writes records whom it acted for (app_on_behalf_of). Async rules and
// workflows run this way. An onBehalfOf of uuid.Nil means the app acts for nobody (an SLA's breach, which no person's
// change set off).
func WithPrincipal(ctx context.Context, pool *pgxpool.Pool, principal, onBehalfOf uuid.UUID, fn func(pgx.Tx) error) error {
	return inTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE servicelater_app"); err != nil {
			return fmt.Errorf("db: set role: %w", err)
		}
		var behalf any = onBehalfOf
		if onBehalfOf == uuid.Nil {
			behalf = nil
		}
		if _, err := tx.Exec(ctx, "SELECT app_begin_as($1, $2)", principal, behalf); err != nil {
			return fmt.Errorf("db: set principal: %w", err)
		}
		return fn(tx)
	})
}

// WithSystem runs fn as the owner role, bypassing RLS. It is for work with no acting user: login lookups,
// bootstrap, job workers, migrations. Its use is deliberate and greppable; keep call sites few.
func WithSystem(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return inTx(ctx, pool, fn)
}

func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}
