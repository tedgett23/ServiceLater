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
// servicelater_app role and sets app.current_user_id. Both are transaction-local (SET LOCAL semantics) and
// vanish at commit/rollback, so a pooled connection never leaks identity to the next caller.
//
// All queries on behalf of a user must go through here. Do not use the pool directly for user data.
func WithUser(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, fn func(pgx.Tx) error) error {
	return inTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE servicelater_app"); err != nil {
			return fmt.Errorf("db: set role: %w", err)
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_user_id', $1, true)", userID.String()); err != nil {
			return fmt.Errorf("db: set user: %w", err)
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
