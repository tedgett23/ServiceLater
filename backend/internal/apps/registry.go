package apps

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

type snapshot struct {
	lifecycles map[string]*Lifecycle
	rules      map[string][]*Rule       // by record type, in run order
	workflows  map[string]*Workflow     // by "app/name"
	templates  map[string]*Notification // by "app/name"
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
	r.snap.Store(&snapshot{lifecycles: map[string]*Lifecycle{}, rules: map[string][]*Rule{}, workflows: map[string]*Workflow{}, templates: map[string]*Notification{}})
	return r
}

// Load reads every current definition and swaps them in.
func (r *Registry) Load(ctx context.Context) error {
	if r.pool == nil {
		return nil
	}
	next := &snapshot{lifecycles: map[string]*Lifecycle{}, rules: map[string][]*Rule{}, workflows: map[string]*Workflow{}, templates: map[string]*Notification{}}
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
			if d.Kind == "rule" {
				var rule Rule
				if err := json.Unmarshal(d.Body, &rule); err != nil {
					slog.Error("skipping unreadable rule", "app", d.AppName, "rule", d.Name, "err", err)
					continue
				}
				var p Problems
				if rule.validateStructure(d.AppName, &p); len(p) > 0 {
					slog.Error("skipping invalid rule", "app", d.AppName, "rule", d.Name, "problems", p.Error())
					continue
				}
				// A rule whose expressions no longer compile stays, and is blocked or skipped when it runs (per its
				// on_error), so a broken rule can never be silently ignored.
				rule.compile()
				rule.App, rule.Version = d.AppName, versions[d.AppName]
				next.rules[rule.RecordType] = append(next.rules[rule.RecordType], &rule)
				continue
			}
			if d.Kind == "workflow" {
				var wf Workflow
				if err := json.Unmarshal(d.Body, &wf); err != nil {
					slog.Error("skipping unreadable workflow", "app", d.AppName, "workflow", d.Name, "err", err)
					continue
				}
				var p Problems
				if wf.validateStructure(d.AppName, &p); len(p) > 0 {
					slog.Error("skipping invalid workflow", "app", d.AppName, "workflow", d.Name, "problems", p.Error())
					continue
				}
				// A step whose expressions no longer compile stays, and fails the execution that reaches it.
				wf.compile()
				wf.App, wf.Version = d.AppName, versions[d.AppName]
				next.workflows[wf.App+"/"+wf.Name] = &wf
				continue
			}
			if d.Kind == "notification" {
				var n Notification
				if err := json.Unmarshal(d.Body, &n); err != nil {
					slog.Error("skipping unreadable notification template", "app", d.AppName, "template", d.Name, "err", err)
					continue
				}
				var p Problems
				if n.validateStructure(d.AppName, &p); len(p) > 0 {
					slog.Error("skipping invalid notification template", "app", d.AppName, "template", d.Name, "problems", p.Error())
					continue
				}
				n.App, n.Version = d.AppName, versions[d.AppName]
				next.templates[n.App+"/"+n.Name] = &n
				continue
			}
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
	for _, list := range next.rules {
		sort.SliceStable(list, func(i, j int) bool {
			a, b := list[i], list[j]
			if a.Order != b.Order {
				return a.Order < b.Order
			}
			if a.App != b.App {
				return a.App < b.App
			}
			return a.Name < b.Name
		})
	}
	r.snap.Store(next)
	return nil
}

// Rules returns the rules to run for a record type at a point in a save: the `task` rules first, then the type's
// own, each in order (then app, then name).
func (r *Registry) Rules(recordType, timing, event string) []*Rule {
	s := r.snap.Load()
	var out []*Rule
	for _, rt := range []string{"task", recordType} {
		if rt == recordType && recordType == "task" && len(out) > 0 {
			break // asking for `task` itself must not list its rules twice
		}
		for _, rule := range s.rules[rt] {
			if rule.Applies(timing, event) {
				out = append(out, rule)
			}
		}
	}
	return out
}

// Notification finds a notification template by the app that defines it and its name. It is nil when there is none
// (the app was removed since a notification was sent with it).
func (r *Registry) Notification(app, name string) *Notification {
	return r.snap.Load().templates[app+"/"+name]
}

// Workflow finds a workflow by the app that defines it and its name. It is nil when there is none.
func (r *Registry) Workflow(app, name string) *Workflow { return r.snap.Load().workflows[app+"/"+name] }

// Workflows lists every installed workflow, by app and name.
func (r *Registry) Workflows() []*Workflow {
	m := r.snap.Load().workflows
	out := make([]*Workflow, 0, len(m))
	for _, w := range m {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App+"/"+out[i].Name < out[j].App+"/"+out[j].Name })
	return out
}

// Rule finds one rule by the app that defines it and its name, for a job that was queued for it. It is nil when the
// rule is gone (uninstalled since the save that queued the job).
func (r *Registry) Rule(app, name string) *Rule {
	for _, list := range r.snap.Load().rules {
		for _, rule := range list {
			if rule.App == app && rule.Name == name {
				return rule
			}
		}
	}
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
