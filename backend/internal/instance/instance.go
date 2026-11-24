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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// Maintenance is a restore waiting or running: while there is one, the API answers 503 and the worker takes no jobs.
type Maintenance struct {
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

// Sync loads the instance settings and any active restore.
func (w *Watcher) Sync(ctx context.Context) error {
	var st State
	err := db.WithSystem(ctx, w.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		rows, err := q.GetSystemSettings(ctx, settings.InstancePrefix)
		if err != nil {
			return err
		}
		stored := map[string][]byte{}
		for _, r := range rows {
			stored[r.Name] = r.Value
		}
		st.Instance = settings.InstanceFrom(stored)
		r, err := q.ActiveRestore(ctx)
		if err == nil {
			since := r.CreatedAt.Time
			if r.StartedAt.Valid {
				since = r.StartedAt.Time
			}
			st.Maintenance = &Maintenance{Operation: "restore", Status: r.Status, Since: since, By: r.RequestedByName}
			return nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.state = st
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
