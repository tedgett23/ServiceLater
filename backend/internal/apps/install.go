package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// NotifyChannel carries "the installed apps changed" to every instance, which then reloads (docs, D7).
const NotifyChannel = "app_changes"

var errDryRun = errors.New("dry run")

type DefRef struct{ Kind, Name string }

func (d DefRef) String() string { return d.Kind + " " + d.Name }

// Rename records tasks that move from an old status to a renamed one (renamed_from in a lifecycle).
type Rename struct {
	RecordType, From, To string // RecordType is the task type whose records move
	Records              int64

	lifecycle *Lifecycle // the lifecycle that defines To, for its category
}

// Plan is what an install will do (or did). Building it writes nothing.
type Plan struct {
	App         string
	FromVersion string // empty for a first install
	ToVersion   string
	Added       []DefRef
	Changed     []DefRef
	Unchanged   []DefRef
	Removed     []DefRef
	Sources     map[DefRef]string // which layers each definition came from
	Renames     []Rename
	affected    map[DefRef][]string // the task types each new or changed lifecycle governs
	Notes       []string
	// Permissions are what the app's principal will hold: its async rules, its workflows and its run_as: app rules
	// act with them. NewPermissions are those the installed version does not have yet, which need approving.
	Permissions    []string
	NewPermissions []string
	// RunsAsApp lists the after rules that act as the app (run_as: app) rather than as the person saving.
	RunsAsApp []string
	// permissionsChanged is set when the app's permissions differ from the installed version's, which is a change
	// even when no definition is.
	permissionsChanged bool
}

// NoChange reports that installing would leave everything as it is.
func (p *Plan) NoChange() bool {
	return p.FromVersion == p.ToVersion && len(p.Added)+len(p.Changed)+len(p.Removed) == 0 && !p.permissionsChanged
}

// layers describes a definition's origin when it is anything other than the shipped layer alone.
func (p *Plan) layers(r DefRef) string {
	if s := p.Sources[r]; s != "" && s != "default" {
		return " (" + strings.ReplaceAll(s, "+", " + ") + ")"
	}
	return ""
}

func (p *Plan) String() string {
	var b strings.Builder
	switch {
	case p.FromVersion == "":
		fmt.Fprintf(&b, "%s %s (new)\n", p.App, p.ToVersion)
	case p.NoChange():
		fmt.Fprintf(&b, "%s %s (no changes)\n", p.App, p.ToVersion)
	default:
		fmt.Fprintf(&b, "%s %s -> %s\n", p.App, p.FromVersion, p.ToVersion)
	}
	list := func(label string, refs []DefRef) {
		for _, r := range refs {
			fmt.Fprintf(&b, "  %-10s %s%s\n", label, r, p.layers(r))
		}
	}
	list("add", p.Added)
	list("change", p.Changed)
	list("remove", p.Removed)
	for _, r := range p.Renames {
		fmt.Fprintf(&b, "  %-10s %s status %q -> %q (%d records)\n", "rename", r.RecordType, r.From, r.To, r.Records)
	}
	if len(p.Permissions) > 0 {
		fmt.Fprintf(&b, "  %-10s %s\n", "permits", strings.Join(p.Permissions, ", "))
	}
	if len(p.NewPermissions) > 0 {
		fmt.Fprintf(&b, "  %-10s %s (needs approval)\n", "new", strings.Join(p.NewPermissions, ", "))
	}
	for _, r := range p.RunsAsApp {
		fmt.Fprintf(&b, "  %-10s rule %s acts as the app\n", "run_as", r)
	}
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "  note: %s\n", n)
	}
	return b.String()
}

type InstallOptions struct {
	DryRun      bool
	InstalledBy string
	// ApprovePermissions accepts the permissions an app asks for that its installed version does not have. Without
	// it such an install is refused (a dry run just shows them). First-party bundled apps are approved by shipping.
	ApprovePermissions bool
	// NoDowngrade refuses an app older than the installed version (the admin page and its API). The CLI may go back,
	// which is how a release is rolled back.
	NoDowngrade bool
	// Also runs in the install's transaction once every app is applied, so a change made with the install (the
	// builder's edit) commits or rolls back with it. Not run on a dry run.
	Also func(ctx context.Context, q *dbgen.Queries) error
}

// Install validates against the database, then applies a compiled app in one transaction. Any failure rolls back
// the whole install, so an app is either fully installed or not at all. A dry run returns the plan and writes nothing.
func Install(ctx context.Context, pool *pgxpool.Pool, c *Compiled, opts InstallOptions) (*Plan, error) {
	plans, err := InstallAll(ctx, pool, []*Compiled{c}, opts)
	if err != nil {
		return nil, err
	}
	return plans[0], nil
}

// InstallAll installs several apps, already in dependency order, in a single transaction: either all of them are
// installed or none is. Each app sees the ones before it, so a dry run of a whole batch is planned correctly even
// though nothing is written.
func InstallAll(ctx context.Context, pool *pgxpool.Pool, apps []*Compiled, opts InstallOptions) ([]*Plan, error) {
	var plans []*Plan
	err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		plans = plans[:0]
		// One install at a time, across every instance.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('servicelater.apps'))"); err != nil {
			return err
		}
		q := dbgen.New(tx)
		for _, c := range apps {
			plan, err := buildPlan(ctx, q, c)
			if err != nil {
				return fmt.Errorf("installing %s: %w", c.Manifest.Name, err)
			}
			plans = append(plans, plan)
			if opts.NoDowngrade && plan.FromVersion != "" {
				from, err1 := ParseVersion(plan.FromVersion)
				to, err2 := ParseVersion(plan.ToVersion)
				if err1 == nil && err2 == nil && to.Compare(from) < 0 {
					return fmt.Errorf("%s %s is older than the installed %s: install a newer version, or roll back with the command line (servicelater app install)",
						c.Manifest.Name, plan.ToVersion, plan.FromVersion)
				}
			}
			if len(plan.NewPermissions) > 0 && !opts.ApprovePermissions && !opts.DryRun {
				return fmt.Errorf("%s asks for permissions it does not have yet: %s. Its async rules, workflows and run_as: app rules act with them for anyone. Review them, then install again approving them (-approve-permissions)",
					c.Manifest.Name, strings.Join(plan.NewPermissions, ", "))
			}
			if !plan.NoChange() {
				if err := apply(ctx, tx, q, c, plan, opts.InstalledBy); err != nil {
					return err
				}
			}
			if !opts.DryRun {
				// Kept every time, not only on a change, so apps installed before files were kept get theirs.
				if err := storeFiles(ctx, q, c); err != nil {
					return err
				}
				if err := storeHistoryExclusions(ctx, q, c); err != nil {
					return err
				}
			}
		}
		if opts.DryRun {
			return errDryRun // roll everything back
		}
		if opts.Also != nil {
			return opts.Also(ctx, q)
		}
		return nil
	})
	if errors.Is(err, errDryRun) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return plans, nil
}

func buildPlan(ctx context.Context, q *dbgen.Queries, c *Compiled) (*Plan, error) {
	m := c.Manifest
	plan := &Plan{App: m.Name, ToVersion: m.Version, Sources: map[DefRef]string{}}
	var problems []string

	if m.MinVersion != "" {
		need, _ := ParseVersion(m.MinVersion)
		have, _ := ParseVersion(PlatformVersion)
		if need.Compare(have) > 0 {
			problems = append(problems, fmt.Sprintf("%s needs ServiceLater %s or later, but this is %s", m.Name, need, have))
		}
	}
	for _, d := range m.Deps {
		dep, err := q.GetApp(ctx, d.Name)
		if errors.Is(err, pgx.ErrNoRows) {
			problems = append(problems, fmt.Sprintf("%s needs %s (%s), which is not installed: install it first", m.Name, d.Name, d.Constraint))
			continue
		}
		if err != nil {
			return nil, err
		}
		con, _ := ParseConstraint(d.Constraint)
		have, err := ParseVersion(dep.Version)
		if err != nil || !con.Satisfied(have) {
			problems = append(problems, fmt.Sprintf("%s needs %s (%s), but %s is installed", m.Name, d.Name, d.Constraint, dep.Version))
		}
	}

	plan.Permissions = append([]string(nil), m.Permissions...)
	sort.Strings(plan.Permissions)
	held := map[string]bool{}
	if cur, err := q.GetApp(ctx, m.Name); err == nil {
		var was Manifest
		if json.Unmarshal(cur.Manifest, &was) == nil {
			for _, p := range was.Permissions {
				held[p] = true
			}
		}
	}
	for _, p := range plan.Permissions {
		if !held[p] {
			plan.NewPermissions = append(plan.NewPermissions, p)
		}
	}
	plan.permissionsChanged = len(plan.NewPermissions) > 0 || len(held) != len(plan.Permissions)
	for _, d := range c.Definitions {
		if d.Rule != nil && d.Rule.RunAs == RunAsApp {
			plan.RunsAsApp = append(plan.RunsAsApp, d.Rule.Name)
		}
	}
	sort.Strings(plan.RunsAsApp)

	if cur, err := q.GetApp(ctx, m.Name); err == nil {
		plan.FromVersion = cur.Version
		from, e1 := ParseVersion(cur.Version)
		to, e2 := ParseVersion(m.Version)
		if e1 == nil && e2 == nil && to.Compare(from) < 0 {
			plan.Notes = append(plan.Notes, fmt.Sprintf("this is a downgrade from %s", cur.Version))
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	existing, err := q.ListAppCurrentDefinitions(ctx, m.Name)
	if err != nil {
		return nil, err
	}
	have := map[DefRef]string{}
	haveSource := map[DefRef]string{}
	for _, d := range existing {
		have[DefRef{d.Kind, d.Name}] = d.Checksum
		haveSource[DefRef{d.Kind, d.Name}] = d.Source
	}

	statuses, err := q.ListTaskStatusCounts(ctx)
	if err != nil {
		return nil, err
	}
	// Which record types will have a lifecycle of their own once this app is installed. A type without one uses the
	// `task` lifecycle when there is one, so installing or changing that affects every such type.
	own := map[string]bool{}
	installed, err := q.ListCurrentDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range installed {
		if d.Kind == "lifecycle" && d.AppName != m.Name {
			own[d.Name] = true
		}
	}
	for _, d := range c.Definitions {
		if d.Kind == "lifecycle" {
			own[d.Name] = true
		}
	}
	plan.affected = map[DefRef][]string{}

	seen := map[DefRef]bool{}
	for _, d := range c.Definitions {
		ref := DefRef{d.Kind, d.Name}
		seen[ref] = true
		plan.Sources[ref] = d.Source()
		// A record type has one lifecycle and one form, and a role, a CMDB class or a relationship type name means
		// one thing, so two apps cannot both provide any of them. Rules do not compete: each is named within its own
		// app, and every applicable rule runs.
		if d.Kind == "lifecycle" || d.Kind == "role" || d.Kind == "form" || d.Kind == "class" || d.Kind == "relationship" || d.Kind == "catalog_item" || d.Kind == "record_type" {
			if other, err := q.FindDefinitionProvider(ctx, dbgen.FindDefinitionProviderParams{Kind: d.Kind, Name: d.Name, AppName: m.Name}); err == nil {
				problems = append(problems, fmt.Sprintf("the %s is already provided by %s: uninstall it first, or remove it from one of the apps", ref, other))
				continue
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
		}
		isNew := false
		switch sum, ok := have[ref]; {
		case !ok:
			plan.Added = append(plan.Added, ref)
			isNew = true
		case sum != d.Checksum:
			plan.Changed = append(plan.Changed, ref)
			isNew = true
		default:
			plan.Unchanged = append(plan.Unchanged, ref)
		}
		// A new or changed lifecycle must not leave any record it governs in a status it does not have.
		if d.Lifecycle != nil && isNew {
			for _, typ := range affectedTypes(d.Lifecycle.RecordType, statuses, own) {
				plan.affected[ref] = append(plan.affected[ref], typ)
				renames, bad := checkStatuses(d.Lifecycle, typ, statuses)
				plan.Renames = append(plan.Renames, renames...)
				problems = append(problems, bad...)
			}
		}
	}
	bad, err := checkTypeOwnership(ctx, q, c)
	if err != nil {
		return nil, err
	}
	problems = append(problems, bad...)
	for ref := range have {
		if !seen[ref] {
			plan.Removed = append(plan.Removed, ref)
			switch ref.Kind {
			case "record_type":
				plan.Notes = append(plan.Notes, fmt.Sprintf("removing the record type %s retires it: its records are kept and open by reference, but nothing lists or files them until an app defines it again", ref.Name))
			case "role":
				plan.Notes = append(plan.Notes, fmt.Sprintf("removing the role %s takes it away from everyone it was granted to", ref.Name))
			case "class":
				plan.Notes = append(plan.Notes, fmt.Sprintf("removing the class %s leaves its configuration items, whose fields cannot be changed until a class of that name is installed again", ref.Name))
			case "relationship":
				plan.Notes = append(plan.Notes, fmt.Sprintf("removing the relationship type %s leaves the relationships of that type, which no longer carry impact", ref.Name))
			}
		}
	}
	for _, refs := range [][]DefRef{plan.Added, plan.Changed, plan.Unchanged, plan.Removed} {
		sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })
	}
	// A change to the shipped layer should come with a new version. A change that only comes from adding or removing
	// local layers is expected to leave the version alone.
	if plan.FromVersion == plan.ToVersion {
		for _, ref := range plan.Changed {
			if haveSource[ref] == plan.Sources[ref] {
				plan.Notes = append(plan.Notes, "definitions changed without a version bump")
				break
			}
		}
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "\n"))
	}
	return plan, nil
}

// affectedTypes are the task types a lifecycle governs: its own type, or for the `task` default every type present
// that has no lifecycle of its own.
func affectedTypes(recordType string, inUse []dbgen.ListTaskStatusCountsRow, own map[string]bool) []string {
	if recordType != "task" {
		return []string{recordType}
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range inUse {
		if !own[s.Type] && !seen[s.Type] {
			seen[s.Type] = true
			out = append(out, s.Type)
		}
	}
	sort.Strings(out)
	return out
}

// checkStatuses makes sure no record of a type is left in a status the lifecycle does not have: each status in use
// must exist, or be mapped from an old name with renamed_from.
func checkStatuses(lc *Lifecycle, typ string, inUse []dbgen.ListTaskStatusCountsRow) (renames []Rename, problems []string) {
	for _, s := range inUse {
		if s.Type != typ || s.N == 0 || lc.HasState(s.Status) {
			continue
		}
		mapped := false
		for _, st := range lc.States {
			if st.RenamedFrom == s.Status {
				renames = append(renames, Rename{RecordType: typ, From: s.Status, To: st.ID, Records: s.N, lifecycle: lc})
				mapped = true
			}
		}
		if !mapped {
			problems = append(problems, fmt.Sprintf(
				"%d %s records are in status %q, which this lifecycle does not have: keep the state, or move its records with renamed_from on the state that replaces it",
				s.N, typ, s.Status))
		}
	}
	return renames, problems
}

func apply(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, c *Compiled, plan *Plan, by string) error {
	manifest, err := json.Marshal(c.Manifest)
	if err != nil {
		return err
	}
	if err := q.UpsertApp(ctx, dbgen.UpsertAppParams{Name: c.Manifest.Name, Version: c.Manifest.Version, Manifest: manifest, InstalledBy: by}); err != nil {
		return err
	}
	// The app's principal: created on first install, brought back on a reinstall.
	if _, err := q.AppPrincipal(ctx, c.Manifest.Name); err != nil {
		return err
	}
	if err := q.SetAppPrincipalActive(ctx, dbgen.SetAppPrincipalActiveParams{App: pgtype.Text{String: c.Manifest.Name, Valid: true}, Active: true}); err != nil {
		return err
	}

	byRef := map[DefRef]Definition{}
	for _, d := range c.Definitions {
		byRef[DefRef{d.Kind, d.Name}] = d
	}
	for _, ref := range append(append([]DefRef{}, plan.Added...), plan.Changed...) {
		d := byRef[ref]
		next, err := q.MaxDefinitionVersion(ctx, dbgen.MaxDefinitionVersionParams{AppName: c.Manifest.Name, Kind: d.Kind, Name: d.Name})
		if err != nil {
			return err
		}
		if err := q.ClearCurrentDefinition(ctx, dbgen.ClearCurrentDefinitionParams{AppName: c.Manifest.Name, Kind: d.Kind, Name: d.Name}); err != nil {
			return err
		}
		if err := q.InsertDefinition(ctx, dbgen.InsertDefinitionParams{
			AppName: c.Manifest.Name, Kind: d.Kind, Name: d.Name, Version: next + 1, Checksum: d.Checksum, Body: d.Body, Source: d.Source(),
		}); err != nil {
			return err
		}
	}
	for _, ref := range plan.Removed {
		if err := q.DeleteDefinitions(ctx, dbgen.DeleteDefinitionsParams{AppName: c.Manifest.Name, Kind: ref.Kind, Name: ref.Name}); err != nil {
			return err
		}
		if ref.Kind == "role" {
			if err := q.DeleteAppRole(ctx, dbgen.DeleteAppRoleParams{Name: ref.Name, AppName: pgtype.Text{String: c.Manifest.Name, Valid: true}}); err != nil {
				return err
			}
		}
	}
	// Roles live in their own table too, which RLS and grants refer to; the definition is the source.
	for _, ref := range append(append([]DefRef{}, plan.Added...), plan.Changed...) {
		if r := byRef[ref].Role; r != nil {
			if err := q.UpsertAppRole(ctx, dbgen.UpsertAppRoleParams{Name: r.Name, Label: r.Label, Description: r.Description,
				Permissions: r.Permissions, AppName: pgtype.Text{String: c.Manifest.Name, Valid: true}}); err != nil {
				return err
			}
		}
	}

	if err := syncTaskTypes(ctx, q, c); err != nil {
		return err
	}

	// Move records out of renamed statuses, then make every stored category match the lifecycle.
	for i, r := range plan.Renames {
		cat, _ := r.lifecycle.Category(r.To)
		n, err := q.RenameTaskStatus(ctx, dbgen.RenameTaskStatusParams{Type: r.RecordType, Status: r.From, Status_2: r.To, StateCategory: string(cat)})
		if err != nil {
			return err
		}
		plan.Renames[i].Records = n
	}
	for _, ref := range append(append([]DefRef{}, plan.Added...), plan.Changed...) {
		if lc := byRef[ref].Lifecycle; lc != nil {
			for _, st := range lc.States {
				if _, err := q.SyncTaskCategory(ctx, dbgen.SyncTaskCategoryParams{Column1: plan.affected[ref], Status: st.ID, StateCategory: string(st.Category)}); err != nil {
					return err
				}
			}
		}
	}

	// Delivered on commit, so no instance reloads before the new definitions are visible.
	_, err = tx.Exec(ctx, "SELECT pg_notify($1, $2)", NotifyChannel, c.Manifest.Name)
	return err
}

// Uninstall removes an app and everything it owns. User data (tickets, comments) is never touched: records simply
// fall back to the built-in lifecycle and remain readable and closable.
func Uninstall(ctx context.Context, pool *pgxpool.Pool, name string) (removed []DefRef, err error) {
	err = db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('servicelater.apps'))"); err != nil {
			return err
		}
		q := dbgen.New(tx)
		if _, err := q.GetApp(ctx, name); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s is not installed", name)
		} else if err != nil {
			return err
		}
		all, err := q.ListApps(ctx)
		if err != nil {
			return err
		}
		var dependents []string
		for _, a := range all {
			if a.Name == name {
				continue
			}
			var m Manifest
			if json.Unmarshal(a.Manifest, &m) != nil {
				continue
			}
			for _, d := range m.Deps {
				if d.Name == name {
					dependents = append(dependents, a.Name)
				}
			}
		}
		if len(dependents) > 0 {
			return fmt.Errorf("cannot uninstall %s: %s depend%s on it", name, strings.Join(dependents, ", "), map[bool]string{true: "s", false: ""}[len(dependents) == 1])
		}
		defs, err := q.ListAppCurrentDefinitions(ctx, name)
		if err != nil {
			return err
		}
		for _, d := range defs {
			removed = append(removed, DefRef{d.Kind, d.Name})
		}
		if err := q.DeleteApp(ctx, name); err != nil { // cascades to its definitions and roles
			return err
		}
		// Its record types stay, retired, with every record of them (D10).
		if _, err := q.RetireAppTaskTypes(ctx, dbgen.RetireAppTaskTypesParams{OwnerApp: pgtype.Text{String: name, Valid: true}, Keep: []string{}}); err != nil {
			return err
		}
		// Its principal stays, deactivated, for the comments and runs it wrote.
		if err := q.SetAppPrincipalActive(ctx, dbgen.SetAppPrincipalActiveParams{App: pgtype.Text{String: name, Valid: true}, Active: false}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "SELECT pg_notify($1, $2)", NotifyChannel, name)
		return err
	})
	return removed, err
}

// Installed describes an installed app.
type Installed struct {
	Name        string
	Version     string
	Description string
	Definitions int
	Local       int // definitions that include local changes
	// From List only: the rest of the manifest and who installed it when.
	Author       string
	Permissions  []string
	Dependencies []Dependency
	InstalledBy  string
	InstalledAt  time.Time
	UpdatedAt    time.Time
}

func List(ctx context.Context, pool *pgxpool.Pool) ([]Installed, error) {
	var out []Installed
	err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		apps, err := q.ListApps(ctx)
		if err != nil {
			return err
		}
		defs, err := q.ListCurrentDefinitions(ctx)
		if err != nil {
			return err
		}
		counts, local := map[string]int{}, map[string]int{}
		for _, d := range defs {
			counts[d.AppName]++
			if d.Source != "default" {
				local[d.AppName]++
			}
		}
		for _, a := range apps {
			var m Manifest
			_ = json.Unmarshal(a.Manifest, &m)
			out = append(out, Installed{Name: a.Name, Version: a.Version, Description: m.Description, Definitions: counts[a.Name], Local: local[a.Name],
				Author: m.Author, Permissions: m.Permissions, Dependencies: m.Deps, InstalledBy: a.InstalledBy,
				InstalledAt: a.InstalledAt.Time, UpdatedAt: a.UpdatedAt.Time})
		}
		return nil
	})
	return out, err
}

// checkTypeOwnership finds what would go wrong with the app's record types in this database: a name or prefix another
// type has, a prefix change on a type that has records, and types it names from its dependencies that they do not
// define.
func checkTypeOwnership(ctx context.Context, q *dbgen.Queries, c *Compiled) (problems []string, err error) {
	app := c.Manifest.Name
	for _, d := range c.Definitions {
		t := d.RecordType
		if t == nil {
			continue
		}
		cur, err := q.GetTaskType(ctx, t.Name)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return nil, err
		case !cur.OwnerApp.Valid:
			problems = append(problems, fmt.Sprintf("the record type %s is a core type", t.Name))
			continue
		case cur.OwnerApp.String != app:
			problems = append(problems, fmt.Sprintf("the record type %s belongs to %s (whose records keep it even when that app is removed): choose another name", t.Name, cur.OwnerApp.String))
			continue
		case cur.Prefix != t.Prefix:
			n, err := q.CountTasksOfType(ctx, t.Name)
			if err != nil {
				return nil, err
			}
			if n > 0 {
				problems = append(problems, fmt.Sprintf("the record type %s has %d records, so its prefix stays %s (it is part of their references): it cannot become %s", t.Name, n, cur.Prefix, t.Prefix))
				continue
			}
		}
		other, err := q.GetTaskTypeByPrefix(ctx, t.Prefix)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return nil, err
		case other.Type != t.Name:
			problems = append(problems, fmt.Sprintf("the prefix %s of the record type %s is already the prefix of %s: choose another", t.Prefix, t.Name, other.Type))
		}
	}
	deps := map[string]bool{}
	for _, d := range c.Manifest.Deps {
		deps[d.Name] = true
	}
	for _, name := range c.ForeignTypes {
		cur, err := q.GetTaskType(ctx, name)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			problems = append(problems, fmt.Sprintf("%s names the record type %q, which is neither a core type, nor its own, nor one its dependencies define", app, name))
		case err != nil:
			return nil, err
		case !cur.OwnerApp.Valid || !deps[cur.OwnerApp.String] || cur.RetiredAt.Valid:
			problems = append(problems, fmt.Sprintf("%s names the record type %q, which none of its dependencies define: add the app that does to its dependencies", app, name))
		}
	}
	return problems, nil
}

// syncTaskTypes writes the app's record types to task_types and retires those it no longer defines. A new row gets
// its number sequence from the table's trigger; an existing one keeps its numbering.
func syncTaskTypes(ctx context.Context, q *dbgen.Queries, c *Compiled) error {
	owner := pgtype.Text{String: c.Manifest.Name, Valid: true}
	keep := []string{}
	for _, d := range c.Definitions {
		t := d.RecordType
		if t == nil {
			continue
		}
		keep = append(keep, t.Name)
		if err := q.UpsertAppTaskType(ctx, dbgen.UpsertAppTaskTypeParams{Type: t.Name, Prefix: t.Prefix, Label: t.Label, Plural: t.Plural,
			AnyoneMayFile: t.AnyoneMayFile, Description: t.Description, OwnerApp: owner}); err != nil {
			return err
		}
	}
	_, err := q.RetireAppTaskTypes(ctx, dbgen.RetireAppTaskTypesParams{OwnerApp: owner, Keep: keep})
	return err
}
