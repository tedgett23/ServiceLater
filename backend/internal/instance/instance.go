// Package instance is what this instance is (its name and environment, Admin → Instance) and whether it is in
// maintenance (a restore waiting or running, docs/backup-and-clone.md). Every api and worker server keeps a copy,
// reloaded every second (Watcher.Run), so the banner and the maintenance answer cost no database work per request.
package instance

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// Maintenance is a restore or a clone waiting or running: while there is one, the API answers 503 and the worker
// takes no jobs.
type Maintenance struct {
	// Operation is restore or clone.
	Operation string    `json:"operation"`
	Status    string    `json:"status"`
	Since     time.Time `json:"since"`
	By        string    `json:"by"`
}

// State is the instance as last loaded.
type State struct {
	settings.Instance
	Maintenance *Maintenance `json:"maintenance"`
}

// Watcher keeps the State.
type Watcher struct {
	pool  *pgxpool.Pool
	mu    sync.RWMutex
	state State
}

// NewWatcher starts with a production instance and no maintenance, until Sync loads the real state.
func NewWatcher(pool *pgxpool.Pool) *Watcher {
	return &Watcher{pool: pool, state: State{Instance: settings.DefaultInstance}}
}

// State is the instance as last loaded.
func (w *Watcher) State() State {
	if w == nil {
		return State{Instance: settings.DefaultInstance}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.state
}

// InMaintenance reports whether a restore is waiting or running.
func (w *Watcher) InMaintenance() bool { return w.State().Maintenance != nil }

// Sync loads any active restore or clone, and the instance settings. The settings table is locked while a restore or
// a clone replaces it, so they are read with a short lock timeout and, while it is locked, the last ones read stand:
// the maintenance state never waits for the restore it reports.
func (w *Watcher) Sync(ctx context.Context) error {
	var m *Maintenance
	err := db.WithSystem(ctx, w.pool, func(tx pgx.Tx) error {
		r, err := dbgen.New(tx).ActiveRestore(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		since := r.CreatedAt.Time
		if r.StartedAt.Valid {
			since = r.StartedAt.Time
		}
		m = &Maintenance{Operation: r.Operation, Status: r.Status, Since: since, By: r.RequestedByName}
		return nil
	})
	if err != nil {
		return err
	}
	var in settings.Instance
	read := true
	err = db.WithSystem(ctx, w.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '200ms'"); err != nil {
			return err
		}
		rows, err := dbgen.New(tx).GetSystemSettings(ctx, settings.InstancePrefix)
		if err != nil {
			return err
		}
		stored := map[string][]byte{}
		for _, r := range rows {
			stored[r.Name] = r.Value
		}
		in = settings.InstanceFrom(stored)
		return nil
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" && m != nil { // lock_not_available, during a restore or clone
		read, err = false, nil
	}
	if err != nil {
		return err
	}
	w.mu.Lock()
	if read {
		w.state.Instance = in
	}
	w.state.Maintenance = m
	w.mu.Unlock()
	return nil
}

// Run syncs every interval until ctx ends.
func (w *Watcher) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := w.Sync(ctx); err != nil && ctx.Err() == nil {
			slog.Error("instance state", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
