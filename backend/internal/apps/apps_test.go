package apps

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

func TestVersions(t *testing.T) {
	for _, bad := range []string{"", "1", "1.2", "1.2.3.4", "v1.2.3", "1.2.x", "01.2.3", "-1.0.0"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) should fail", bad)
		}
	}
	cases := []struct {
		constraint, version string
		want                bool
	}{
		{">=0.1.0", "0.1.0", true}, {">=0.1.0", "0.0.9", false}, {">=1.0.0, <2.0.0", "1.9.9", true},
		{">=1.0.0, <2.0.0", "2.0.0", false}, {"1.2.3", "1.2.3", true}, {"1.2.3", "1.2.4", false},
		{"=1.2.3", "1.2.3", true}, {">1.0.0", "1.0.0", false}, {"<=1.0.0", "1.0.0", true},
	}
	for _, c := range cases {
		con, err := ParseConstraint(c.constraint)
		if err != nil {
			t.Fatalf("%q: %v", c.constraint, err)
		}
		v, _ := ParseVersion(c.version)
		if got := con.Satisfied(v); got != c.want {
			t.Errorf("%q satisfied by %s = %v, want %v", c.constraint, c.version, got, c.want)
		}
	}
	for _, bad := range []string{"", ">=", ">=x", ",", ">=1.0.0,"} {
		if _, err := ParseConstraint(bad); err == nil {
			t.Errorf("ParseConstraint(%q) should fail", bad)
		}
	}
}

const goodManifest = `apiVersion: 1
name: demo-app
version: 1.0.0
author: Someone
description: A demo.
ServiceLater_min_version: 0.1.0
dependencies:
  - core-itsm: ">=0.1.0"
permissions:
  - tickets:read_all
config:
  - { key: token, label: API token, type: secret }
`

const goodLifecycle = `kind: lifecycle
record_type: incident
initial: new
states:
  new:    { label: New,    category: open }
  active: { label: Active, category: open }
  done:   { label: Done,   category: closed }
transitions:
  - { from: new,    to: active, verb: Start, primary: true }
  - { from: active, to: done,   verb: Finish }
`

func bundle(files map[string]string) *Bundle {
	b := &Bundle{Files: map[string][]byte{}}
	for k, v := range files {
		b.Files[k] = []byte(v)
	}
	return b
}

func compile(t *testing.T, files map[string]string) (*Compiled, Problems) {
	t.Helper()
	c, err := bundle(files).Compile()
	if err == nil {
		return c, nil
	}
	var p Problems
	if !errors.As(err, &p) {
		t.Fatalf("Compile returned %T, want Problems: %v", err, err)
	}
	return nil, p
}

func TestCompileAcceptsAGoodApp(t *testing.T) {
	c, p := compile(t, map[string]string{"app.yaml": goodManifest, "default/lifecycles/incident.yaml": goodLifecycle, "README.md": "hi"})
	if p != nil {
		t.Fatal(p)
	}
	if c.Manifest.Name != "demo-app" || len(c.Manifest.Deps) != 1 || c.Manifest.Deps[0].Name != "core-itsm" {
		t.Errorf("manifest: %+v", c.Manifest)
	}
	if len(c.Definitions) != 1 || c.Definitions[0].Kind != "lifecycle" || c.Definitions[0].Name != "incident" {
		t.Fatalf("definitions: %+v", c.Definitions)
	}
	lc := c.Definitions[0].Lifecycle
	// The author's order is kept, because it is the order states are displayed in.
	if got := []string{lc.States[0].ID, lc.States[1].ID, lc.States[2].ID}; strings.Join(got, ",") != "new,active,done" {
		t.Errorf("state order %v", got)
	}
	if c.Definitions[0].Checksum == "" {
		t.Error("no checksum")
	}
	// Same input, same checksum.
	c2, _ := compile(t, map[string]string{"app.yaml": goodManifest, "default/lifecycles/incident.yaml": goodLifecycle})
	if c2.Definitions[0].Checksum != c.Definitions[0].Checksum {
		t.Error("checksum is not stable")
	}
}

// Each case must be rejected, and the message must say what is wrong.
func TestCompileRejectsBadApps(t *testing.T) {
	withManifest := func(replace, with string) map[string]string {
		return map[string]string{"app.yaml": strings.Replace(goodManifest, replace, with, 1)}
	}
	withLifecycle := func(replace, with string) map[string]string {
		return map[string]string{"app.yaml": goodManifest, "default/lifecycles/incident.yaml": strings.Replace(goodLifecycle, replace, with, 1)}
	}
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"no manifest", map[string]string{"default/lifecycles/incident.yaml": goodLifecycle}, "app.yaml is missing"},
		{"wrong format version", withManifest("apiVersion: 1", "apiVersion: 2"), "apiVersion must be 1"},
		{"bad name", withManifest("name: demo-app", "name: Demo_App"), "name \"Demo_App\""},
		{"bad version", withManifest("version: 1.0.0", "version: 1.0"), "version:"},
		{"no description", withManifest("description: A demo.", "description: ''"), "description is required"},
		{"typo in a field", withManifest("author:", "authr:"), "field authr not found"},
		{"bad dependency constraint", withManifest(">=0.1.0", "soon"), "dependency core-itsm"},
		{"self dependency", withManifest("core-itsm:", "demo-app:"), "cannot depend on itself"},
		{"bad permission", withManifest("tickets:read_all", "everything"), "permission \"everything\" is not one the platform knows"},
		{"bad config type", withManifest("type: secret", "type: blob"), "type must be string, secret, bool or int"},
		{"unknown record type", withLifecycle("record_type: incident", "record_type: widget"), "record_type \"widget\""},
		{"initial is not a state", withLifecycle("initial: new", "initial: nowhere"), "initial state \"nowhere\""},
		{"bad category", withLifecycle("category: closed", "category: finished"), "category must be open, waiting, done or closed"},
		{"transition to nowhere", withLifecycle("to: done", "to: gone"), "refers to a state that does not exist"},
		{"missing verb", withLifecycle("verb: Finish", "verb: ''"), "needs a verb"},
		{"self transition", withLifecycle("from: active, to: done", "from: active, to: active"), "cannot move to itself"},
		{"unreachable state", withLifecycle("- { from: new,    to: active, verb: Start, primary: true }\n", ""), "can never be reached"},
		{"duplicate transition", withLifecycle("  - { from: active, to: done,   verb: Finish }", "  - { from: active, to: done,   verb: Finish }\n  - { from: active, to: done,   verb: Again }"), "listed twice"},
		{"two primaries", withLifecycle("verb: Finish", "verb: Finish, primary: true }\n  - { from: active, to: new, verb: Back, primary: true"), "more than one primary"},
		{"a guard with a typo", withLifecycle("verb: Finish", "verb: Finish, when: 'record.prioity > 1', error: Nope"), "when: column"},
		{"a guard that is not true or false", withLifecycle("verb: Finish", "verb: Finish, when: 'record.priority + 1', error: Nope"), "must produce true or false"},
		{"a guard comparing with null", withLifecycle("verb: Finish", "verb: Finish, when: 'record.assignee_id != null', error: Nope"), "do not compare with null"},
		{"a guard without a message", withLifecycle("verb: Finish", "verb: Finish, when: 'record.priority > 1'"), "no error message"},
		{"a message without a guard", withLifecycle("verb: Finish", "verb: Finish, error: Nope"), "no guard (when)"},
		{"a message that is too long", withLifecycle("verb: Finish", "verb: Finish, when: 'record.priority > 1', error: "+strings.Repeat("x", 201)), "longer than 200"},
		{"state field typo", withLifecycle("label: New", "lable: New"), "unknown field \"lable\""},
		{"wrong kind", withLifecycle("kind: lifecycle", "kind: rule"), "kind must be lifecycle"},
		{"stale rename", withLifecycle("active: { label: Active,", "active: { renamed_from: done, label: Active,"), "renamed_from \"done\" is still a state"},
		{"scripts are gone", map[string]string{"app.yaml": goodManifest, "default/scripts/x.star": "x"}, "apps contain no code"},
		{"migrations are reserved", map[string]string{"app.yaml": goodManifest, "default/migrations/1.sql": "x"}, "migrations are reserved"},
		{"forms not yet", map[string]string{"app.yaml": goodManifest, "default/forms/a.yaml": "kind: form"}, "forms are not supported yet"},
		{"unknown folder", map[string]string{"app.yaml": goodManifest, "default/widgets/a.yaml": "x"}, "unknown folder"},
		{"stray top-level file", map[string]string{"app.yaml": goodManifest, "notes.txt": "x"}, "unexpected file"},
		{"not yaml", map[string]string{"app.yaml": goodManifest, "default/lifecycles/incident.json": "{}"}, "only .yaml files"},
		{"two definitions of one thing", map[string]string{"app.yaml": goodManifest, "default/lifecycles/a.yaml": goodLifecycle, "default/lifecycles/b.yaml": goodLifecycle}, "already defines"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, p := compile(t, c.files)
			if p == nil {
				t.Fatal("was accepted")
			}
			if !strings.Contains(p.Error(), c.want) {
				t.Errorf("error %q does not mention %q", p.Error(), c.want)
			}
		})
	}
}

func TestCompileReportsEveryProblemAtOnce(t *testing.T) {
	_, p := compile(t, map[string]string{
		"app.yaml":                         strings.Replace(goodManifest, "version: 1.0.0", "version: x", 1),
		"default/lifecycles/incident.yaml": strings.Replace(goodLifecycle, "initial: new", "initial: nope", 1),
		"default/scripts/a.star":           "x",
	})
	if len(p) < 3 {
		t.Errorf("want at least 3 problems, got %d:\n%v", len(p), p)
	}
}

func TestLifecycleBehaviour(t *testing.T) {
	c, _ := compile(t, map[string]string{"app.yaml": goodManifest, "default/lifecycles/incident.yaml": goodLifecycle})
	lc := c.Definitions[0].Lifecycle
	if !lc.CanTransition("new", "active") || lc.CanTransition("new", "done") || lc.CanTransition("done", "new") {
		t.Error("CanTransition disagrees with the definition")
	}
	if lc.Label("active") != "Active" || lc.Label("mystery") != "mystery" {
		t.Error("Label")
	}
	if cat, ok := lc.Category("done"); !ok || cat != CategoryClosed {
		t.Error("Category")
	}
	if _, ok := lc.Category("mystery"); ok {
		t.Error("unknown state has a category")
	}
	// A record stranded in a state that no longer exists can still be closed, but nothing else.
	if !lc.CanTransition("gone", "done") || lc.CanTransition("gone", "active") {
		t.Error("stranded records must be closable and nothing more")
	}
	if st := lc.StrandedTransitions(); len(st) != 1 || st[0].To != "done" || st[0].Verb != "Close" {
		t.Errorf("stranded moves: %+v", st)
	}
	if got := lc.TransitionsFrom("new"); len(got) != 1 || got[0].To != "active" {
		t.Errorf("TransitionsFrom: %+v", got)
	}
}

func TestGuardsAreCheckedWhenAMoveIsMade(t *testing.T) {
	src := strings.Replace(goodLifecycle, "verb: Finish", "verb: Finish, when: 'has(record.assignee_id) && record.priority <= 2', error: Assign it and raise its priority first", 1)
	c, p := compile(t, map[string]string{"app.yaml": goodManifest, "default/lifecycles/incident.yaml": src})
	if p != nil {
		t.Fatal(p)
	}
	lc := c.Definitions[0].Lifecycle
	who := "u1"
	in := expr.Input{Record: &expr.Record{Priority: 1, AssigneeID: &who}, Old: &expr.Record{}, User: &expr.User{}}

	ok, msg, err := lc.Check(context.Background(), "active", "done", in)
	if err != nil || !ok || msg != "Assign it and raise its priority first" {
		t.Errorf("guard should pass: %v %q %v", ok, msg, err)
	}
	in.Record.AssigneeID = nil
	ok, msg, err = lc.Check(context.Background(), "active", "done", in)
	if err != nil || ok || msg != "Assign it and raise its priority first" {
		t.Errorf("guard should fail with its message: %v %q %v", ok, msg, err)
	}
	// Moves without a guard are unaffected.
	if ok, _, err := lc.Check(context.Background(), "new", "active", in); err != nil || !ok {
		t.Errorf("an unguarded move: %v %v", ok, err)
	}
	// The guard survives a round trip through storage, which is how the running server gets it.
	body := c.Definitions[0].Body
	var stored Lifecycle
	if err := json.Unmarshal(body, &stored); err != nil {
		t.Fatal(err)
	}
	stored.compileGuards()
	if ok, _, err := stored.Check(context.Background(), "active", "done", in); err != nil || ok {
		t.Errorf("a stored lifecycle must enforce its guard: %v %v", ok, err)
	}
}

// If a stored guard stops compiling (a platform upgrade removed a field), its move is blocked, never allowed.
func TestAGuardThatCannotBeCheckedBlocksItsMove(t *testing.T) {
	lc := &Lifecycle{States: []State{{ID: "a", Label: "A", Category: CategoryOpen}, {ID: "b", Label: "B", Category: CategoryClosed}},
		Transitions: []Transition{{From: "a", To: "b", Verb: "Go", When: "record.a_field_that_was_removed > 1", Error: "no"}}}
	lc.compileGuards()
	ok, _, err := lc.Check(context.Background(), "a", "b", expr.Input{})
	if ok || err == nil || !strings.Contains(err.Error(), "cannot be checked") {
		t.Errorf("a broken guard must fail closed: %v %v", ok, err)
	}
}

func TestPrimaryTransitionSortsFirst(t *testing.T) {
	lc := &Lifecycle{Transitions: []Transition{
		{From: "a", To: "x", Verb: "one"}, {From: "a", To: "y", Verb: "two", Primary: true}, {From: "a", To: "z", Verb: "three"},
	}}
	got := lc.TransitionsFrom("a")
	if got[0].Verb != "two" || got[1].Verb != "one" || got[2].Verb != "three" {
		t.Errorf("order %+v", got)
	}
}

func TestFallbackLifecycleIsValidAndWorks(t *testing.T) {
	fb := FallbackLifecycle("incident")
	var p Problems
	fb.validate("fallback", &p)
	if len(p) != 0 {
		t.Fatalf("the built-in lifecycle is invalid: %v", p)
	}
	if fb.Initial != "new" || !fb.CanTransition("new", "open") || !fb.CanTransition("open", "closed") {
		t.Error("fallback flow")
	}
}

func writeApp(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadDirIgnoresHiddenFilesAndRejectsSymlinks(t *testing.T) {
	dir := writeApp(t, map[string]string{"app.yaml": goodManifest, "default/lifecycles/.gitkeep": "", ".DS_Store": "x", "default/forms/.gitkeep": ""})
	if _, err := Validate(dir); err != nil {
		t.Fatalf("hidden files should be ignored: %v", err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "lifecycles", "evil.yaml")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("symlink accepted: %v", err)
	}
}

func TestPackRoundTripIsDeterministic(t *testing.T) {
	dir := writeApp(t, map[string]string{"app.yaml": goodManifest, "default/lifecycles/incident.yaml": goodLifecycle, "README.md": "hello"})
	name, first, err := Pack(dir)
	if err != nil {
		t.Fatal(err)
	}
	if name != "demo-app-1.0.0.swapp" {
		t.Errorf("name %q", name)
	}
	_, second, _ := Pack(dir)
	if !bytes.Equal(first, second) {
		t.Error("packing twice gave different bytes")
	}

	archive := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(archive, first, 0o600); err != nil {
		t.Fatal(err)
	}
	fromDir, err := Validate(dir)
	if err != nil {
		t.Fatal(err)
	}
	fromArchive, err := Validate(archive)
	if err != nil {
		t.Fatalf("the archive does not validate: %v", err)
	}
	if fromDir.Definitions[0].Checksum != fromArchive.Definitions[0].Checksum {
		t.Error("archive and folder disagree")
	}
}

func TestPackRefusesAnInvalidApp(t *testing.T) {
	dir := writeApp(t, map[string]string{"app.yaml": goodManifest, "default/scripts/x.star": "x"})
	if _, _, err := Pack(dir); err == nil {
		t.Error("packed an invalid app")
	}
}

func TestArchiveCannotEscapeItsFolder(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"app.yaml", "../evil.yaml"} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte("x"))
	}
	_ = zw.Close()
	f := filepath.Join(t.TempDir(), "evil.swapp")
	_ = os.WriteFile(f, buf.Bytes(), 0o600)
	if _, err := Load(f); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Errorf("zip-slip accepted: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.swapp")); err == nil {
		t.Error("missing file accepted")
	}
	junk := filepath.Join(t.TempDir(), "junk.swapp")
	_ = os.WriteFile(junk, []byte("not a zip"), 0o600)
	if _, err := Load(junk); err == nil {
		t.Error("junk accepted")
	}
}

// The apps shipped in this repository must always be installable.
func TestFirstPartyAppsValidate(t *testing.T) {
	root := filepath.Join("..", "..", "..", "apps")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n++
		if _, err := Validate(filepath.Join(root, e.Name())); err != nil {
			t.Errorf("apps/%s:\n%v", e.Name(), err)
		}
	}
	if n < 6 {
		t.Errorf("found only %d first-party apps", n)
	}
}
