// Package db owns the pgx pool, embedded goose migrations and RLS-scoped transactions.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/servicelater/servicelater/backend/db/migrations"
)

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	return ConnectMax(ctx, url, 0)
}

// ConnectMax is Connect with an explicit connection limit (0 keeps the driver's default). The worker role uses its
// own small pool, so a burst of background work can never take connections that web requests need.
func ConnectMax(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// ConnectWaiting is Connect for a server starting up, perhaps beside a database that is still starting too (a fresh
// Compose or Kubernetes deployment): it tries again every two seconds until wait has passed, saying so, before it
// gives up.
func ConnectWaiting(ctx context.Context, url string, wait time.Duration) (*pgxpool.Pool, error) {
	deadline := time.Now().Add(wait)
	for {
		pool, err := Connect(ctx, url)
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			return pool, err
		}
		slog.Info("waiting for the database", "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
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
