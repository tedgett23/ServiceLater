package apps

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

type snapshot struct {
	lifecycles map[string]*Lifecycle
	apps       []Installed
}

// Registry serves the current definitions from memory. Installs elsewhere reach it through NOTIFY, so every
// instance updates at once with no restart. Definitions are immutable and versioned, so a reload can never serve
// a half-applied change: the whole snapshot is swapped in one step.
type Registry struct {
	pool *pgxpool.Pool
	snap atomic.Pointer[snapshot]

	mu   sync.Mutex
	subs []func()
}

// NewRegistry returns a registry that already answers with built-in fallbacks. A nil pool gives one that only
// ever does (useful in tests and before any app is installed).
func NewRegistry(pool *pgxpool.Pool) *Registry {
	r := &Registry{pool: pool}
	r.snap.Store(&snapshot{lifecycles: map[string]*Lifecycle{}})
	return r
}

// Load reads every current definition and swaps them in.
func (r *Registry) Load(ctx context.Context) error {
	if r.pool == nil {
		return nil
	}
	next := &snapshot{lifecycles: map[string]*Lifecycle{}}
	err := db.WithSystem(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		apps, err := q.ListApps(ctx)
		if err != nil {
			return err
		}
		versions := map[string]string{}
		counts := map[string]int{}
		defs, err := q.ListCurrentDefinitions(ctx)
		if err != nil {
			return err
		}
		for _, d := range defs {
			counts[d.AppName]++
		}
		for _, a := range apps {
			versions[a.Name] = a.Version
			var m Manifest
			_ = json.Unmarshal(a.Manifest, &m)
			next.apps = append(next.apps, Installed{Name: a.Name, Version: a.Version, Description: m.Description, Definitions: counts[a.Name]})
		}
		for _, d := range defs {
			if d.Kind != "lifecycle" {
				continue
			}
			var lc Lifecycle
			if err := json.Unmarshal(d.Body, &lc); err != nil {
				slog.Error("skipping unreadable lifecycle", "app", d.AppName, "err", err)
				continue
			}
			var p Problems
			if lc.validateStructure(d.AppName, &p); len(p) > 0 {
				slog.Error("skipping invalid lifecycle", "app", d.AppName, "problems", p.Error())
				continue
			}
			// A guard that no longer compiles (say a platform upgrade removed a field) blocks only its own move.
			lc.compileGuards()
			for _, t := range lc.Transitions {
				if t.guardErr != nil {
					slog.Error("lifecycle guard does not compile: that move is blocked until the app is fixed",
						"app", d.AppName, "from", t.From, "to", t.To, "err", t.guardErr)
				}
			}
			lc.App, lc.Version = d.AppName, versions[d.AppName]
			next.lifecycles[lc.RecordType] = &lc
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.snap.Store(next)
	return nil
}

// Lifecycle returns the lifecycle for a record type. A type's own lifecycle wins; otherwise the `task` lifecycle
// applies to every type, as a base applies to what extends it; otherwise the built-in fallback.
func (r *Registry) Lifecycle(recordType string) *Lifecycle {
	s := r.snap.Load()
	if lc, ok := s.lifecycles[recordType]; ok {
		return lc
	}
	if lc, ok := s.lifecycles["task"]; ok {
		return lc
	}
	return FallbackLifecycle(recordType)
}

// Apps lists the installed apps as of the last load.
func (r *Registry) Apps() []Installed { return r.snap.Load().apps }

// OnChange registers a function to run after a reload triggered by an install or uninstall.
func (r *Registry) OnChange(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs = append(r.subs, fn)
}

// Run keeps the registry current until ctx ends, reloading whenever apps change and after any reconnect
// (NOTIFY is fire-and-forget, so a reconnect may have missed one).
func (r *Registry) Run(ctx context.Context, dsn string) {
	reload := func() {
		if err := r.Load(ctx); err != nil {
			if ctx.Err() == nil {
				slog.Error("reloading apps", "err", err)
			}
			return
		}
		r.mu.Lock()
		subs := append([]func(){}, r.subs...)
		r.mu.Unlock()
		for _, fn := range subs {
			fn()
		}
	}
	db.Listen(ctx, dsn, []string{NotifyChannel},
		func(reconnected bool) {
			if reconnected {
				reload()
			}
		},
		func(string, string) { reload() })
}
