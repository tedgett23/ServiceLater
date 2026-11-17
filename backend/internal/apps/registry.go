package apps

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sort"
	"strings"
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
	slas       map[string][]*SLA        // by record type, by app then name
	forms      map[string]*Form         // by record type
	classes    map[string]*Class        // CMDB classes by name
	relations  map[string]*Relationship // CMDB relationship types by name
	items      map[string]*CatalogItem  // catalog items by name
	types      map[string]*RecordType   // record types apps define, by name
	retired    map[string]bool          // record types whose app was removed
	apps       []Installed
}

func emptySnapshot() *snapshot {
	return &snapshot{lifecycles: map[string]*Lifecycle{}, rules: map[string][]*Rule{}, workflows: map[string]*Workflow{},
		templates: map[string]*Notification{}, slas: map[string][]*SLA{}, forms: map[string]*Form{},
		classes: map[string]*Class{}, relations: map[string]*Relationship{}, items: map[string]*CatalogItem{},
		types: map[string]*RecordType{}, retired: map[string]bool{}}
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
	r.snap.Store(emptySnapshot())
	return r
}

// Load reads every current definition and swaps them in.
func (r *Registry) Load(ctx context.Context) error {
	if r.pool == nil {
		return nil
	}
	next := emptySnapshot()
	err := db.WithSystem(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		apps, err := q.ListApps(ctx)
		if err != nil {
			return err
		}
		retired, err := q.ListRetiredTaskTypes(ctx)
		if err != nil {
			return err
		}
		for _, t := range retired {
			next.retired[t] = true
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
			if d.Kind == "sla" {
				var sla SLA
				if err := json.Unmarshal(d.Body, &sla); err != nil {
					slog.Error("skipping unreadable SLA", "app", d.AppName, "sla", d.Name, "err", err)
					continue
				}
				var p Problems
				if sla.validateStructure(d.AppName, &p); len(p) > 0 {
					slog.Error("skipping invalid SLA", "app", d.AppName, "sla", d.Name, "problems", p.Error())
					continue
				}
				// An SLA whose expressions no longer compile still starts clocks, which show the problem.
				sla.App, sla.Version, sla.Checksum, sla.Body = d.AppName, versions[d.AppName], d.Checksum, json.RawMessage(d.Body)
				sla.compile()
				next.slas[sla.RecordType] = append(next.slas[sla.RecordType], &sla)
				continue
			}
			if d.Kind == "form" {
				var f Form
				if err := json.Unmarshal(d.Body, &f); err != nil {
					slog.Error("skipping unreadable form", "app", d.AppName, "form", d.Name, "err", err)
					continue
				}
				// A condition that no longer compiles stays, and refuses saves until the app is fixed.
				f.compile()
				f.App, f.Version = d.AppName, versions[d.AppName]
				next.forms[f.RecordType] = &f
				continue
			}
			if d.Kind == "class" {
				var c Class
				if err := json.Unmarshal(d.Body, &c); err != nil {
					slog.Error("skipping unreadable class", "app", d.AppName, "class", d.Name, "err", err)
					continue
				}
				c.App, c.Version = d.AppName, versions[d.AppName]
				next.classes[c.Name] = &c
				continue
			}
			if d.Kind == "catalog_item" {
				var it CatalogItem
				if err := json.Unmarshal(d.Body, &it); err != nil || it.Form == nil {
					slog.Error("skipping unreadable catalog item", "app", d.AppName, "item", d.Name, "err", err)
					continue
				}
				// A question's condition or a title that no longer compiles stays, and refuses orders until fixed.
				it.compile()
				it.App, it.Version = d.AppName, versions[d.AppName]
				it.Form.App, it.Form.Version = d.AppName, versions[d.AppName]
				next.items[it.Name] = &it
				continue
			}
			if d.Kind == "record_type" {
				var rt RecordType
				if err := json.Unmarshal(d.Body, &rt); err != nil {
					slog.Error("skipping unreadable record type", "app", d.AppName, "type", d.Name, "err", err)
					continue
				}
				rt.App, rt.Version = d.AppName, versions[d.AppName]
				next.types[rt.Name] = &rt
				continue
			}
			if d.Kind == "relationship" {
				var rel Relationship
				if err := json.Unmarshal(d.Body, &rel); err != nil {
					slog.Error("skipping unreadable relationship type", "app", d.AppName, "relationship", d.Name, "err", err)
					continue
				}
				rel.App, rel.Version = d.AppName, versions[d.AppName]
				next.relations[rel.Name] = &rel
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
				for _, g := range t.checks {
					if g.err != nil {
						slog.Error("lifecycle guard does not compile: that move is blocked until the app is fixed",
							"app", d.AppName, "from", t.From, "to", t.To, "err", g.err)
					}
				}
				for name, l := range t.Lookups {
					if l != nil && l.err != nil {
						slog.Error("lifecycle lookup is invalid: that move is blocked until the app is fixed",
							"app", d.AppName, "from", t.From, "to", t.To, "lookup", name, "err", l.err)
					}
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
	for _, list := range next.slas {
		sort.Slice(list, func(i, j int) bool { return list[i].App+"/"+list[i].Name < list[j].App+"/"+list[j].Name })
	}
	resolveClasses(next.classes)
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

// SLAs returns the SLAs that apply to a record type: those for `task`, then the type's own, each by app and name.
func (r *Registry) SLAs(recordType string) []*SLA {
	s := r.snap.Load()
	out := slices.Clone(s.slas["task"])
	if recordType != "task" {
		out = append(out, s.slas[recordType]...)
	}
	return out
}

// Form returns the form for a record type: an installed app's, or the one every type has without it (FormFor).
func (r *Registry) Form(recordType string) *Form {
	if f, ok := r.snap.Load().forms[recordType]; ok {
		return f
	}
	return builtinForm(recordType)
}

var builtinForms sync.Map // record type -> *Form

func builtinForm(recordType string) *Form {
	if f, ok := builtinForms.Load(recordType); ok {
		return f.(*Form)
	}
	f, _ := builtinForms.LoadOrStore(recordType, FormFor(recordType))
	return f.(*Form)
}

// Class finds a CMDB class by name; it is nil when no installed app defines it.
func (r *Registry) Class(name string) *Class { return r.snap.Load().classes[name] }

// Classes lists the installed CMDB classes by label.
func (r *Registry) Classes() []*Class {
	m := r.snap.Load().classes
	out := make([]*Class, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// ClassAndDescendants is the class and every class that extends it, however deeply: what filtering items by it finds.
func (r *Registry) ClassAndDescendants(name string) []string {
	out := []string{name}
	for _, c := range r.snap.Load().classes {
		if slices.Contains(c.Ancestors, name) {
			out = append(out, c.Name)
		}
	}
	slices.Sort(out[1:])
	return out
}

// FormOf is the form a record is filled in and checked against: the catalog item's it was ordered from, while that
// item is installed, else its type's.
func (r *Registry) FormOf(recordType, catalogItem string) *Form {
	if catalogItem != "" {
		if it := r.CatalogItem(catalogItem); it != nil {
			return it.Form
		}
	}
	return r.Form(recordType)
}

// CatalogItem returns the catalog item of this name, or nil.
func (r *Registry) CatalogItem(name string) *CatalogItem { return r.snap.Load().items[name] }

// CatalogItems are every item in the catalog, by category, then order, then label.
func (r *Registry) CatalogItems() []*CatalogItem {
	items := r.snap.Load().items
	out := make([]*CatalogItem, 0, len(items))
	for _, it := range items {
		out = append(out, it)
	}
	slices.SortFunc(out, func(a, b *CatalogItem) int {
		if c := strings.Compare(a.Category, b.Category); c != 0 {
			return c
		}
		if a.Order != b.Order {
			return a.Order - b.Order
		}
		return strings.Compare(a.Label, b.Label)
	})
	return out
}

// Relationship finds a CMDB relationship type by name; it is nil when no installed app defines it.
func (r *Registry) Relationship(name string) *Relationship { return r.snap.Load().relations[name] }

// Relationships lists the installed relationship types by label.
func (r *Registry) Relationships() []*Relationship {
	m := r.snap.Load().relations
	out := make([]*Relationship, 0, len(m))
	for _, rel := range m {
		out = append(out, rel)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// ImpactTypes are the relationship types trouble spreads along.
func (r *Registry) ImpactTypes() []string {
	var out []string
	for _, rel := range r.snap.Load().relations {
		if rel.Impact {
			out = append(out, rel.Name)
		}
	}
	slices.Sort(out)
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

// Provides reports whether the record type is in use, so people can file records of it: an installed app defines its
// own lifecycle, or, for a type an app defines, the type is installed and has a lifecycle through `task`. A core type
// that only inherits the task lifecycle is registered but not offered.
func (r *Registry) Provides(recordType string) bool {
	s := r.snap.Load()
	if recordType == "task" {
		return false
	}
	if _, ok := s.lifecycles[recordType]; ok {
		return IsCoreType(recordType) || s.types[recordType] != nil // a retired type is never in use
	}
	_, base := s.lifecycles["task"]
	return base && s.types[recordType] != nil
}

// RecordType is the definition of a record type an installed app adds; nil for a core type or one no installed app
// defines (its app was removed: the type is retired).
func (r *Registry) RecordType(name string) *RecordType { return r.snap.Load().types[name] }

// KnownType reports whether a type can be named today: a core type (task included) or one an installed app defines.
// A retired type's records can still be opened by reference, but nothing lists, filters or files them.
func (r *Registry) KnownType(t string) bool { return IsCoreType(t) || r.snap.Load().types[t] != nil }

// Exists reports whether records of a type can exist: a known type, or a retired one (its app was removed), whose
// records still open by reference and so still need their labels and form to be shown.
func (r *Registry) Exists(t string) bool { return r.KnownType(t) || r.snap.Load().retired[t] }

// RecordTypes are every type records can be held of (not task): the core types, then the installed apps' by name.
func (r *Registry) RecordTypes() []string {
	out := slices.DeleteFunc(slices.Clone(CoreRecordTypes), func(s string) bool { return s == "task" })
	n := len(out)
	for t := range r.snap.Load().types {
		out = append(out, t)
	}
	slices.Sort(out[n:])
	return out
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
