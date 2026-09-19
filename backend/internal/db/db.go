// Package db owns the pgx pool, embedded goose migrations and RLS-scoped transactions.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/servicelater/servicelater/backend/db/migrations"
)

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies pending migrations. A Postgres session-level advisory lock ensures only one instance
// migrates at a time; others wait, then find nothing to do.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("db: migrate lock: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("db: migrate provider: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("db: migrate: %w", err)
	}
	return nil
}
