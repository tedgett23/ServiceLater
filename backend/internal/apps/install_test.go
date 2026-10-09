package apps_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
)

type spec struct {
	name, version string
	deps          string   // manifest lines, e.g. `  - core-itsm: ">=0.1.0"`
	states        []string // "id:Label:category[:renamed_from]"
	transitions   []string // "from>to"
	extra         string   // extra manifest lines
	record        string   // the record type the lifecycle is for; incident when empty
}

// build makes a compiled app from a spec.
func (s spec) build(t *testing.T) *apps.Compiled { return s.buildWith(t, nil) }

// buildWith is build with an instance-local layer (files keyed like the contents of local/).
func (s spec) buildWith(t *testing.T, instance map[string]string) *apps.Compiled {
	t.Helper()
	var m strings.Builder
	fmt.Fprintf(&m, "apiVersion: 1\nname: %s\nversion: %s\nauthor: t\ndescription: \"Test app.\"\n", s.name, s.version)
	if s.deps != "" {
		m.WriteString("dependencies:\n" + s.deps + "\n")
	}
	m.WriteString(s.extra)
	files := map[string][]byte{"app.yaml": []byte(m.String())}
	if len(s.states) > 0 {
		var l strings.Builder
		record := s.record
		if record == "" {
			record = "incident"
		}
		l.WriteString("kind: lifecycle\nrecord_type: " + record + "\ninitial: " + strings.Split(s.states[0], ":")[0] + "\nstates:\n")
		for _, st := range s.states {
			p := strings.Split(st, ":")
			fmt.Fprintf(&l, "  %s: { label: %s, category: %s", p[0], p[1], p[2])
			if len(p) > 3 {
				fmt.Fprintf(&l, ", renamed_from: %s", p[3])
			}
			l.WriteString(" }\n")
		}
		l.WriteString("transitions:\n")
		for _, tr := range s.transitions {
			p := strings.Split(tr, ">")
			fmt.Fprintf(&l, "  - { from: %s, to: %s, verb: Go to %s }\n", p[0], p[1], p[1])
		}
		files["default/lifecycles/incident.yaml"] = []byte(l.String())
	}
	b := &apps.Bundle{Files: files}
	if instance != nil {
		b.Instance, b.InstanceRoot = map[string][]byte{}, "/instance/"+s.name
		for k, v := range instance {
			b.Instance[k] = []byte(v)
		}
	}
	c, err := b.Compile()
	if err != nil {
		t.Fatalf("test app is invalid: %v", err)
	}
	return c
}

var flow = spec{
	name: "flow", version: "1.0.0",
	states:      []string{"new:New:open", "active:Active:open", "done:Done:closed"},
	transitions: []string{"new>active", "active>done"},
}

func install(t *testing.T, pool *pgxpool.Pool, s spec) (*apps.Plan, error) {
	t.Helper()
	return apps.Install(context.Background(), pool, s.build(t), apps.InstallOptions{InstalledBy: "test"})
}

func mustInstall(t *testing.T, pool *pgxpool.Pool, s spec) *apps.Plan {
	t.Helper()
	p, err := install(t, pool, s)
	if err != nil {
		t.Fatalf("install %s: %v", s.name, err)
	}
	return p
}

func loaded(t *testing.T, pool *pgxpool.Pool) *apps.Registry {
	t.Helper()
	r := apps.NewRegistry(pool)
	if err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func addTicket(t *testing.T, pool *pgxpool.Pool, status, category string) {
	t.Helper()
	var uid string
	if err := pool.QueryRow(context.Background(), `INSERT INTO users (email, name, role) VALUES ($1, 'U', 'end_user')
		ON CONFLICT (email) DO UPDATE SET name = 'U' RETURNING id`, "u@x.test").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO tasks (type, title, requester_id, status, state_category) VALUES ('incident', 't', $1, $2, $3) RETURNING id`,
		uid, status, category).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO incidents (task_id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInstallAndRegistry(t *testing.T) {
	pool := dbtest.Pool(t)
	reg := loaded(t, pool)
	if got := reg.Lifecycle("incident"); got.App != "built-in" {
		t.Fatalf("before any install the lifecycle should be the built-in one, got %q", got.App)
	}

	plan := mustInstall(t, pool, flow)
	if len(plan.Added) != 1 || plan.FromVersion != "" || plan.ToVersion != "1.0.0" {
		t.Errorf("plan: %+v", plan)
	}
	reg = loaded(t, pool)
	lc := reg.Lifecycle("incident")
	if lc.App != "flow" || lc.Version != "1.0.0" || lc.Initial != "new" || !lc.CanTransition("new", "active") {
		t.Errorf("lifecycle from registry: %+v", lc)
	}
	list, _ := apps.List(context.Background(), pool)
	if len(list) != 1 || list[0].Name != "flow" || list[0].Definitions != 1 {
		t.Errorf("list: %+v", list)
	}
}

func TestReinstallIsANoOp(t *testing.T) {
	pool := dbtest.Pool(t)
	mustInstall(t, pool, flow)
	plan := mustInstall(t, pool, flow)
	if !plan.NoChange() || len(plan.Unchanged) != 1 {
		t.Errorf("plan: %+v", plan)
	}
	if n := count(t, pool, "SELECT count(*) FROM app_definitions"); n != 1 {
		t.Errorf("%d definition rows, want 1: a no-op install must not add versions", n)
	}
}

func TestUpdateAddsAnImmutableVersion(t *testing.T) {
	pool := dbtest.Pool(t)
	mustInstall(t, pool, flow)

	v2 := flow
	v2.version = "1.1.0"
	v2.states = []string{"new:New:open", "active:Working:open", "done:Done:closed"}
	plan := mustInstall(t, pool, v2)
	if len(plan.Changed) != 1 || plan.FromVersion != "1.0.0" {
		t.Errorf("plan: %+v", plan)
	}
	if n := count(t, pool, "SELECT count(*) FROM app_definitions"); n != 2 {
		t.Errorf("%d definition rows, want 2 (old version kept)", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM app_definitions WHERE current"); n != 1 {
		t.Errorf("%d current rows, want exactly 1", n)
	}
	if got := loaded(t, pool).Lifecycle("incident").Label("active"); got != "Working" {
		t.Errorf("label %q", got)
	}
}

func TestUpdateThatStrandsRecordsIsRejectedAndChangesNothing(t *testing.T) {
	pool := dbtest.Pool(t)
	mustInstall(t, pool, flow)
	addTicket(t, pool, "active", "open")

	bad := flow
	bad.version = "2.0.0"
	bad.states = []string{"new:New:open", "finished:Finished:closed"}
	bad.transitions = []string{"new>finished"}
	_, err := install(t, pool, bad)
	if err == nil || !strings.Contains(err.Error(), "renamed_from") || !strings.Contains(err.Error(), `"active"`) {
		t.Fatalf("want an error about status active and renamed_from, got: %v", err)
	}
	// All or nothing: the old version is still what is installed.
	list, _ := apps.List(context.Background(), pool)
	if list[0].Version != "1.0.0" {
		t.Errorf("a failed install changed the app: %+v", list)
	}
	if got := loaded(t, pool).Lifecycle("incident"); !got.HasState("active") {
		t.Error("a failed install changed the lifecycle")
	}
}

func TestRenamedFromMovesRecords(t *testing.T) {
	pool := dbtest.Pool(t)
	mustInstall(t, pool, flow)
	addTicket(t, pool, "active", "open")
	addTicket(t, pool, "active", "open")
	addTicket(t, pool, "done", "closed")

	v2 := flow
	v2.version = "2.0.0"
	v2.states = []string{"new:New:open", "working:Working:waiting:active", "done:Done:closed"}
	v2.transitions = []string{"new>working", "working>done"}
	plan := mustInstall(t, pool, v2)
	if len(plan.Renames) != 1 || plan.Renames[0].Records != 2 || plan.Renames[0].To != "working" {
		t.Fatalf("renames: %+v", plan.Renames)
	}
	if n := count(t, pool, "SELECT count(*) FROM tasks WHERE status = 'working' AND state_category = 'waiting'"); n != 2 {
		t.Errorf("%d tickets moved to working/waiting, want 2", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM tasks WHERE status = 'active'"); n != 0 {
		t.Errorf("%d tickets left behind", n)
	}
}

func TestStoredCategoriesFollowTheLifecycle(t *testing.T) {
	pool := dbtest.Pool(t)
	mustInstall(t, pool, flow)
	addTicket(t, pool, "active", "open")

	v2 := flow
	v2.version = "1.1.0"
	v2.states = []string{"new:New:open", "active:Active:waiting", "done:Done:closed"} // active now means waiting
	mustInstall(t, pool, v2)
	if n := count(t, pool, "SELECT count(*) FROM tasks WHERE status = 'active' AND state_category = 'waiting'"); n != 1 {
		t.Error("the stored category did not follow the lifecycle")
	}
}

func TestFreshInstallAlsoChecksExistingRecords(t *testing.T) {
	pool := dbtest.Pool(t)
	addTicket(t, pool, "mystery", "open")
	if _, err := install(t, pool, flow); err == nil || !strings.Contains(err.Error(), `"mystery"`) {
		t.Errorf("want an error naming status mystery, got %v", err)
	}
}

func TestDependencies(t *testing.T) {
	pool := dbtest.Pool(t)
	child := spec{name: "child", version: "1.0.0", deps: `  - base: ">=1.0.0"`}

	if _, err := install(t, pool, child); err == nil || !strings.Contains(err.Error(), "base") || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing dependency: %v", err)
	}
	mustInstall(t, pool, spec{name: "base", version: "0.9.0"})
	if _, err := install(t, pool, child); err == nil || !strings.Contains(err.Error(), "0.9.0 is installed") {
		t.Fatalf("dependency too old: %v", err)
	}
	mustInstall(t, pool, spec{name: "base", version: "1.2.0"})
	mustInstall(t, pool, child)
}

func TestPlatformVersionIsChecked(t *testing.T) {
	pool := dbtest.Pool(t)
	future := spec{name: "future", version: "1.0.0", extra: "ServiceLater_min_version: 99.0.0\n"}
	if _, err := install(t, pool, future); err == nil || !strings.Contains(err.Error(), "99.0.0") {
		t.Errorf("an app needing a newer platform was accepted: %v", err)
	}
}

func TestTwoAppsCannotProvideTheSameLifecycle(t *testing.T) {
	pool := dbtest.Pool(t)
	mustInstall(t, pool, flow)
	other := flow
	other.name = "other"
	if _, err := install(t, pool, other); err == nil || !strings.Contains(err.Error(), "already provided by flow") {
		t.Errorf("conflict not reported: %v", err)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	pool := dbtest.Pool(t)
	plan, err := apps.Install(context.Background(), pool, flow.build(t), apps.InstallOptions{DryRun: true})
	if err != nil || len(plan.Added) != 1 {
		t.Fatalf("dry run: %+v %v", plan, err)
	}
	if n := count(t, pool, "SELECT count(*) FROM apps"); n != 0 {
		t.Error("a dry run installed something")
	}
}

func TestUninstall(t *testing.T) {
	pool := dbtest.Pool(t)
	mustInstall(t, pool, spec{name: "base", version: "1.0.0"})
	mustInstall(t, pool, spec{name: "flow", version: "1.0.0", deps: `  - base: ">=1.0.0"`, states: flow.states, transitions: flow.transitions})
	addTicket(t, pool, "active", "open")

	if _, err := apps.Uninstall(context.Background(), pool, "base"); err == nil || !strings.Contains(err.Error(), "flow depends on it") {
		t.Errorf("uninstalling a dependency: %v", err)
	}
	if _, err := apps.Uninstall(context.Background(), pool, "nope"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("uninstalling an unknown app: %v", err)
	}

	removed, err := apps.Uninstall(context.Background(), pool, "flow")
	if err != nil || len(removed) != 1 {
		t.Fatalf("uninstall: %v %v", removed, err)
	}
	if n := count(t, pool, "SELECT count(*) FROM app_definitions WHERE app_name = 'flow'"); n != 0 {
		t.Error("definitions left behind")
	}
	// User data survives, and the ticket still works: it falls back to the built-in lifecycle and can be closed.
	if n := count(t, pool, "SELECT count(*) FROM tasks"); n != 1 {
		t.Error("uninstall touched user data")
	}
	lc := loaded(t, pool).Lifecycle("incident")
	if lc.App != "built-in" || !lc.CanTransition("active", "closed") || lc.CanTransition("active", "open") {
		t.Errorf("a stranded ticket must be closable, and only that: %+v", lc)
	}
	mustInstall(t, pool, spec{name: "flow", version: "1.0.0", deps: `  - base: ">=1.0.0"`, states: flow.states, transitions: flow.transitions})
	if _, err := apps.Uninstall(context.Background(), pool, "flow"); err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Uninstall(context.Background(), pool, "base"); err != nil {
		t.Errorf("base should be removable once nothing depends on it: %v", err)
	}
}

// Installing on one instance must reach the others without a restart.
func TestRegistryReloadsWhenAppsChange(t *testing.T) {
	pool := dbtest.Pool(t)
	dsn := dbtest.DSN(t)
	reg := loaded(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan struct{}, 4)
	reg.OnChange(func() { changed <- struct{}{} })
	go reg.Run(ctx, dsn)
	time.Sleep(300 * time.Millisecond) // let it start listening

	mustInstall(t, pool, flow)
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("the registry was not told about the install")
	}
	if got := reg.Lifecycle("incident"); got.App != "flow" {
		t.Errorf("registry still serves %q", got.App)
	}

	if _, err := apps.Uninstall(ctx, pool, "flow"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("the registry was not told about the uninstall")
	}
	if got := reg.Lifecycle("incident"); got.App != "built-in" {
		t.Errorf("registry still serves %q", got.App)
	}
}

func TestPlanReadsWell(t *testing.T) {
	pool := dbtest.Pool(t)
	plan := mustInstall(t, pool, flow)
	out := plan.String()
	if !strings.Contains(out, "flow 1.0.0 (new)") || !strings.Contains(out, "add") || !strings.Contains(out, "lifecycle incident") {
		t.Errorf("plan text:\n%s", out)
	}
}

func TestEveryBundledAppIsValid(t *testing.T) {
	names, err := apps.BundledNames()
	if err != nil || len(names) < 6 {
		t.Fatalf("bundled apps: %v %v", names, err)
	}
	for _, n := range names {
		b, err := apps.LoadBundled(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if _, err := b.Compile(); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
}

func TestBundledSelection(t *testing.T) {
	def, _ := apps.BundledSelection("")
	if len(def) != 6 || def[0] != "core-itsm" || def[1] != "incident-management" || def[2] != "change-management" || def[3] != "cmdb" || def[4] != "knowledge-base" || def[5] != "service-catalog" {
		t.Errorf("default: %v", def)
	}
	if none, _ := apps.BundledSelection("none"); len(none) != 0 {
		t.Errorf("none: %v", none)
	}
	if all, _ := apps.BundledSelection("all"); len(all) < 6 {
		t.Errorf("all: %v", all)
	}
	if got, _ := apps.BundledSelection(" cmdb , core-itsm "); len(got) != 2 || got[0] != "cmdb" {
		t.Errorf("list: %v", got)
	}
	if _, err := apps.BundledSelection("core-itsm,nope"); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("unknown name: %v", err)
	}
}

func syncDefault(t *testing.T, pool *pgxpool.Pool, localRoot string) []apps.SyncResult {
	t.Helper()
	names, _ := apps.BundledSelection("")
	return apps.SyncBundled(context.Background(), pool, names, localRoot)
}

func writeLocal(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, "incident-management", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSyncBundledInstallsThenIsIdempotent(t *testing.T) {
	pool := dbtest.Pool(t)
	for _, r := range syncDefault(t, pool, "") {
		if r.Err != nil || r.Plan.FromVersion != "" {
			t.Fatalf("first sync: %+v", r)
		}
	}
	for _, r := range syncDefault(t, pool, "") {
		if r.Err != nil || !r.Plan.NoChange() {
			t.Fatalf("second sync should change nothing: %+v", r)
		}
	}
	if got := loaded(t, pool).Lifecycle("incident"); got.App != "incident-management" || got.Label("in_progress") != "In progress" {
		t.Errorf("lifecycle: %+v", got)
	}
}

func TestLocalLayerOfABundledAppComesAndGoes(t *testing.T) {
	pool := dbtest.Pool(t)
	root := t.TempDir()
	syncDefault(t, pool, root)

	writeLocal(t, root, "lifecycles/incident.yaml", "states:\n  in_progress:\n    label: Working\n")
	for _, r := range syncDefault(t, pool, root) {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	lc := loaded(t, pool).Lifecycle("incident")
	if lc.Label("in_progress") != "Working" || lc.Label("new") != "New" {
		t.Errorf("only the overridden label may change: %+v", lc.States)
	}
	list, _ := apps.List(context.Background(), pool)
	for _, a := range list {
		if a.Name == "incident-management" && a.Local != 1 {
			t.Errorf("list should report one definition with local changes: %+v", a)
		}
	}

	if err := os.Remove(filepath.Join(root, "incident-management", "lifecycles", "incident.yaml")); err != nil {
		t.Fatal(err)
	}
	syncDefault(t, pool, root)
	if got := loaded(t, pool).Lifecycle("incident").Label("in_progress"); got != "In progress" {
		t.Errorf("removing the local file must restore the shipped label, got %q", got)
	}
}

// A bad local file must not take anything down or half-apply: the app stays as it was, and says why.
func TestABrokenLocalFileLeavesTheAppAsItWas(t *testing.T) {
	pool := dbtest.Pool(t)
	root := t.TempDir()
	writeLocal(t, root, "lifecycles/incident.yaml", "states:\n  in_progress:\n    label: Working\n")
	syncDefault(t, pool, root)

	writeLocal(t, root, "lifecycles/incident.yaml", "states:\n  in_progress:\n    category: not-a-category\n")
	var failed *apps.SyncResult
	for _, r := range syncDefault(t, pool, root) {
		if r.App == "incident-management" {
			failed = &r
		}
	}
	if failed == nil || failed.Err == nil || !strings.Contains(failed.Err.Error(), "unchanged") || !strings.Contains(failed.Err.Error(), "category must be") {
		t.Fatalf("want a clear error that leaves the app unchanged, got %+v", failed)
	}
	if got := loaded(t, pool).Lifecycle("incident").Label("in_progress"); got != "Working" {
		t.Errorf("the previous good version must still be in force, got %q", got)
	}
}

// The point of layering: upgrade the shipped layer and the operator's change is still there.
func TestUpgradingTheShippedLayerKeepsTheLocalLayer(t *testing.T) {
	pool := dbtest.Pool(t)
	local := map[string]string{"lifecycles/incident.yaml": "states:\n  active:\n    label: Working\n"}
	ctx := context.Background()

	if _, err := apps.Install(ctx, pool, flow.buildWith(t, local), apps.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := loaded(t, pool).Lifecycle("incident").Label("active"); got != "Working" {
		t.Fatalf("local override not applied: %q", got)
	}

	// Version 2 of the app ships a new state. The operator's file is untouched.
	v2 := flow
	v2.version = "2.0.0"
	v2.states = []string{"new:New:open", "active:Active:open", "review:Review:waiting", "done:Done:closed"}
	v2.transitions = []string{"new>active", "active>review", "review>done"}
	plan, err := apps.Install(ctx, pool, v2.buildWith(t, local), apps.InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.FromVersion != "1.0.0" || len(plan.Changed) != 1 {
		t.Errorf("plan: %+v", plan)
	}
	lc := loaded(t, pool).Lifecycle("incident")
	if lc.Label("active") != "Working" {
		t.Error("the operator's change did not survive the upgrade")
	}
	if !lc.HasState("review") || !lc.CanTransition("active", "review") {
		t.Error("the upgrade's new state did not arrive")
	}
	if src := plan.Sources[apps.DefRef{Kind: "lifecycle", Name: "incident"}]; src != "default+instance" {
		t.Errorf("source %q", src)
	}
}

// addTaskOfType inserts a base-only task of another type, as an app-defined type would be.
func addTaskOfType(t *testing.T, pool *pgxpool.Pool, typ, status, category string) {
	t.Helper()
	var uid string
	if err := pool.QueryRow(context.Background(), `INSERT INTO users (email, name, role) VALUES ('u@x.test', 'U', 'end_user')
		ON CONFLICT (email) DO UPDATE SET name = 'U' RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO tasks (type, title, requester_id, status, state_category) VALUES ($1, 't', $2, $3, $4)`,
		typ, uid, status, category); err != nil {
		t.Fatal(err)
	}
}

func TestKnownRecordTypesMatchTheRegistry(t *testing.T) {
	pool := dbtest.Pool(t)
	rows, err := pool.Query(context.Background(), "SELECT type FROM task_types")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	registered := map[string]bool{}
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatal(err)
		}
		registered[typ] = true
	}
	for _, typ := range apps.KnownRecordTypes {
		if typ != "task" && !registered[typ] {
			t.Errorf("%q is a known record type but is not in the task_types registry", typ)
		}
		delete(registered, typ)
	}
	if len(registered) > 0 {
		t.Errorf("registered task types that apps cannot write a lifecycle for: %v", registered)
	}
}

// A lifecycle for `task` is the default for every type, so it must only be checked against, and only rewrite, the
// records of types that do not have a lifecycle of their own.
func TestATaskLifecycleGovernsOnlyTypesWithoutTheirOwn(t *testing.T) {
	pool := dbtest.Pool(t)
	mustInstall(t, pool, flow) // incident has its own lifecycle
	addTaskOfType(t, pool, "change", "mystery", "open")
	addTicket(t, pool, "active", "open") // an incident, in a state the task lifecycle will not have

	base := spec{name: "base", version: "1.0.0", record: "task",
		states:      []string{"todo:To do:open", "doing:Doing:waiting", "finished:Finished:closed"},
		transitions: []string{"todo>doing", "doing>finished"}}

	// The change is stranded by it, and the error says which type. The incident is not mentioned: it is not governed.
	_, err := install(t, pool, base)
	if err == nil || !strings.Contains(err.Error(), "change records are in status \"mystery\"") {
		t.Fatalf("want the change named, got %v", err)
	}
	if strings.Contains(err.Error(), "incident records") {
		t.Errorf("incidents have their own lifecycle and must not be checked against the default: %v", err)
	}

	// Mapping the stranded status lets it through. Only the change moves; the incident is left alone.
	mapped := base
	mapped.states = []string{"todo:To do:open:mystery", "doing:Doing:waiting", "finished:Finished:closed"}
	plan := mustInstall(t, pool, mapped)
	if len(plan.Renames) != 1 || plan.Renames[0].RecordType != "change" || plan.Renames[0].To != "todo" {
		t.Errorf("renames: %+v", plan.Renames)
	}
	if n := count(t, pool, "SELECT count(*) FROM tasks WHERE type = 'change' AND status = 'todo' AND state_category = 'open'"); n != 1 {
		t.Error("the change was not moved")
	}
	if n := count(t, pool, "SELECT count(*) FROM tasks WHERE type = 'incident' AND status = 'active'"); n != 1 {
		t.Error("the default lifecycle rewrote an incident, which has its own")
	}
}

func TestRegistryFallsBackFromTypeToTaskToBuiltIn(t *testing.T) {
	pool := dbtest.Pool(t)
	if got := loaded(t, pool).Lifecycle("change"); got.App != "built-in" {
		t.Fatalf("with nothing installed: %q", got.App)
	}

	mustInstall(t, pool, spec{name: "base", version: "1.0.0", record: "task",
		states: []string{"todo:To do:open", "done:Done:closed"}, transitions: []string{"todo>done"}})
	reg := loaded(t, pool)
	if got := reg.Lifecycle("change"); got.App != "base" || got.Initial != "todo" {
		t.Errorf("a type without its own uses the task default: %+v", got)
	}
	if got := reg.Lifecycle("incident"); got.App != "base" {
		t.Errorf("so does incident until it has its own: %q", got.App)
	}

	mustInstall(t, pool, spec{name: "incidents", version: "1.0.0", states: flow.states, transitions: flow.transitions})
	reg = loaded(t, pool)
	if got := reg.Lifecycle("incident"); got.App != "incidents" {
		t.Errorf("a type's own lifecycle wins: %q", got.App)
	}
	if got := reg.Lifecycle("change"); got.App != "base" {
		t.Errorf("other types are unaffected: %q", got.App)
	}
}
