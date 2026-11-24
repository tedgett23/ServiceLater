// Package db owns the pgx pool, embedded goose migrations and RLS-scoped transactions.
package db

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
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
	return withMigrations(ctx, pool, func(p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	})
}

// MigrateTo brings the schema to exactly version, up or down (a snapshot restore loads into the schema it was taken
// from). Going down drops what the later migrations made, data included.
func MigrateTo(ctx context.Context, pool *pgxpool.Pool, version int64) error {
	return withMigrations(ctx, pool, func(p *goose.Provider) error {
		cur, err := p.GetDBVersion(ctx)
		if err != nil {
			return err
		}
		switch {
		case cur < version:
			_, err = p.UpTo(ctx, version)
		case cur > version:
			_, err = p.DownTo(ctx, version)
		}
		return err
	})
}

// SchemaVersion is the version the database's schema is at, and LatestVersion the one this binary brings it to.
func SchemaVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var v int64
	err := withMigrations(ctx, pool, func(p *goose.Provider) error {
		var err error
		v, err = p.GetDBVersion(ctx)
		return err
	})
	return v, err
}

// LatestVersion is the newest migration this binary carries (the number its file name starts with).
func LatestVersion() (int64, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		if v, err := strconv.ParseInt(num, 10, 64); err == nil && v > latest {
			latest = v
		}
	}
	return latest, nil
}

func withMigrations(ctx context.Context, pool *pgxpool.Pool, fn func(*goose.Provider) error) error {
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
	if err := fn(p); err != nil {
		return fmt.Errorf("db: migrate: %w", err)
	}
	return nil
}
