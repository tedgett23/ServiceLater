package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeYAML(t *testing.T) {
	base := "kind: lifecycle\nstates:\n  a: { label: A, category: open }\n  b: { label: B, category: open }\ntransitions:\n  - { from: a, to: b, verb: Go }\n"
	cases := []struct {
		name, over, want string
		notWant          string
	}{
		{"one nested value changes, the rest stays", "states:\n  a:\n    label: Alpha\n", "label: Alpha", ""},
		{"the sibling is untouched", "states:\n  a:\n    label: Alpha\n", "b: {label: B", ""},
		{"a new key is added", "states:\n  c: { label: C, category: done }\n", "c:", ""},
		{"a list is replaced, not appended to", "transitions:\n  - { from: b, to: a, verb: Back }\n", "verb: Back", "verb: Go"},
		{"null removes a key", "states:\n  b: ~\n", "a:", "b: {label"},
		{"an empty override changes nothing", "", "verb: Go", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := mergeYAML([]byte(base), []byte(c.over))
			if err != nil {
				t.Fatal(err)
			}
			got := strings.ReplaceAll(string(out), " ", "")
			if !strings.Contains(got, strings.ReplaceAll(c.want, " ", "")) {
				t.Errorf("result does not contain %q:\n%s", c.want, out)
			}
			if c.notWant != "" && strings.Contains(got, strings.ReplaceAll(c.notWant, " ", "")) {
				t.Errorf("result should not contain %q:\n%s", c.notWant, out)
			}
		})
	}

	// Shipped order is kept, and additions come after it.
	out, _ := mergeYAML([]byte(base), []byte("states:\n  c: { label: C, category: done }\n  a: { label: Alpha }\n"))
	s := string(out)
	ia, ib, ic := strings.Index(s, "a:"), strings.Index(s, "b:"), strings.Index(s, "c:")
	if ia >= ib || ib >= ic {
		t.Errorf("order not preserved:\n%s", s)
	}

	for name, over := range map[string]string{"a list is not a map": "- x\n", "broken yaml": "a: [1, 2"} {
		if _, err := mergeYAML([]byte(base), []byte(over)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func layered(files map[string]string, instance map[string]string) *Bundle {
	b := bundle(files)
	if instance != nil {
		b.Instance, b.InstanceRoot = map[string][]byte{}, "/etc/servicelater/apps/demo-app"
		for k, v := range instance {
			b.Instance[k] = []byte(v)
		}
	}
	return b
}

func lifecycleOf(t *testing.T, c *Compiled) *Lifecycle {
	t.Helper()
	if len(c.Definitions) != 1 {
		t.Fatalf("want 1 definition, got %d", len(c.Definitions))
	}
	return c.Definitions[0].Lifecycle
}

func TestLocalOverridesOneValueAndNothingElse(t *testing.T) {
	b := layered(map[string]string{
		"app.yaml": goodManifest, "default/lifecycles/incident.yaml": goodLifecycle,
		"local/lifecycles/incident.yaml": "states:\n  active:\n    label: Working\n",
	}, nil)
	c, err := b.Compile()
	if err != nil {
		t.Fatal(err)
	}
	lc := lifecycleOf(t, c)
	if lc.Label("active") != "Working" || lc.Label("new") != "New" || lc.Label("done") != "Done" {
		t.Errorf("labels: %+v", lc.States)
	}
	if len(lc.Transitions) != 2 || lc.Initial != "new" {
		t.Errorf("everything else must come from the shipped file: %+v", lc)
	}
	if d := c.Definitions[0]; d.Source() != "default+local" {
		t.Errorf("source %q", d.Source())
	}

	// The shipped checksum and the effective checksum differ, so an install sees a change.
	plain, _ := bundle(map[string]string{"app.yaml": goodManifest, "default/lifecycles/incident.yaml": goodLifecycle}).Compile()
	if plain.Definitions[0].Checksum == c.Definitions[0].Checksum {
		t.Error("a local override must change the effective definition")
	}
}

// The reason for layering: an upgrade replaces default/, and the operator's local change survives it.
func TestAnUpgradeOfTheShippedLayerKeepsLocalChanges(t *testing.T) {
	local := "states:\n  active:\n    label: Working\n"
	v2 := strings.Replace(goodLifecycle, "  done:   { label: Done,   category: closed }", "  review: { label: Review, category: waiting }\n  done:   { label: Done,   category: closed }", 1)
	v2 = strings.Replace(v2, "  - { from: active, to: done,   verb: Finish }", "  - { from: active, to: review, verb: Send for review }\n  - { from: review, to: done,   verb: Approve }", 1)

	upgraded, err := layered(map[string]string{
		"app.yaml":                         strings.Replace(goodManifest, "version: 1.0.0", "version: 2.0.0", 1),
		"default/lifecycles/incident.yaml": v2, "local/lifecycles/incident.yaml": local,
	}, nil).Compile()
	if err != nil {
		t.Fatal(err)
	}
	lc := lifecycleOf(t, upgraded)
	if lc.Label("active") != "Working" {
		t.Error("the operator's change was lost in the upgrade")
	}
	if !lc.HasState("review") || !lc.CanTransition("active", "review") {
		t.Error("the upgrade's new state did not arrive")
	}
}

func TestLocalCanAddAndRemoveStatesAndReplaceTransitions(t *testing.T) {
	local := `states:
  triage: { label: Triage, category: open }
  active: ~
transitions:
  - { from: new,    to: triage, verb: Triage }
  - { from: triage, to: done,   verb: Close out }
`
	c, err := layered(map[string]string{"app.yaml": goodManifest, "default/lifecycles/incident.yaml": goodLifecycle, "local/lifecycles/incident.yaml": local}, nil).Compile()
	if err != nil {
		t.Fatal(err)
	}
	lc := lifecycleOf(t, c)
	if lc.HasState("active") || !lc.HasState("triage") || !lc.CanTransition("new", "triage") || lc.CanTransition("new", "active") {
		t.Errorf("effective lifecycle: %+v", lc)
	}
}

func TestPrecedenceIsDefaultThenLocalThenInstance(t *testing.T) {
	b := layered(map[string]string{
		"app.yaml": goodManifest, "default/lifecycles/incident.yaml": goodLifecycle,
		"local/lifecycles/incident.yaml": "states:\n  active:\n    label: FromLocal\n  done:\n    label: LocalDone\n",
	}, map[string]string{"lifecycles/incident.yaml": "states:\n  active:\n    label: FromInstance\n"})
	c, err := b.Compile()
	if err != nil {
		t.Fatal(err)
	}
	lc := lifecycleOf(t, c)
	if lc.Label("active") != "FromInstance" || lc.Label("done") != "LocalDone" || lc.Label("new") != "New" {
		t.Errorf("labels: %+v", lc.States)
	}
	if got := c.Definitions[0].Source(); got != "default+local+instance" {
		t.Errorf("source %q", got)
	}
}

func TestAFileOnlyInLocalIsAddedAsIs(t *testing.T) {
	c, err := layered(map[string]string{"app.yaml": goodManifest, "local/lifecycles/incident.yaml": goodLifecycle}, nil).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if c.Definitions[0].Source() != "local" {
		t.Errorf("source %q", c.Definitions[0].Source())
	}
}

func TestProblemsInTheCombinedResultNameTheLayers(t *testing.T) {
	_, err := layered(map[string]string{
		"app.yaml": goodManifest, "default/lifecycles/incident.yaml": goodLifecycle,
		"local/lifecycles/incident.yaml": "states:\n  active:\n    category: finished\n",
	}, nil).Compile()
	if err == nil || !strings.Contains(err.Error(), "lifecycles/incident.yaml (default + local)") || !strings.Contains(err.Error(), "category must be") {
		t.Errorf("want a problem naming both layers, got %v", err)
	}
}

func TestLayerFolderRules(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"old flat layout":          {map[string]string{"app.yaml": goodManifest, "lifecycles/incident.yaml": goodLifecycle}, "shipped definitions go in default/"},
		"scripts in local":         {map[string]string{"app.yaml": goodManifest, "local/scripts/x.star": "x"}, "local/scripts/x.star: scripts are not supported"},
		"a file straight in local": {map[string]string{"app.yaml": goodManifest, "local/notes.yaml": "x: 1"}, "not directly here"},
		"rules not yet in local":   {map[string]string{"app.yaml": goodManifest, "local/rules/a.yaml": "kind: rule"}, "rules are not supported yet"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := bundle(c.files).Compile()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestShippedLayerMustBeValidOnItsOwn(t *testing.T) {
	// The shipped file lacks a category; a local override that supplies it must not paper over that,
	// because the shipped layer is what gets distributed to other instances.
	broken := strings.Replace(goodLifecycle, "category: closed", "", 1)
	dir := writeApp(t, map[string]string{
		"app.yaml": goodManifest, "default/lifecycles/incident.yaml": broken,
		"local/lifecycles/incident.yaml": "states:\n  done:\n    category: closed\n",
	})
	if _, err := Validate(dir); err == nil {
		t.Error("an app whose shipped layer is invalid was accepted because local fixed it")
	}
}

func TestValidateExplainsWhenOnlyTheLocalLayerIsWrong(t *testing.T) {
	dir := writeApp(t, map[string]string{
		"app.yaml": goodManifest, "default/lifecycles/incident.yaml": goodLifecycle,
		"local/lifecycles/incident.yaml": "initial: nowhere\n",
	})
	_, err := Validate(dir)
	if err == nil || !strings.Contains(err.Error(), "shipped app is valid, but with local overrides applied") {
		t.Errorf("want an explanation that the local layer is the problem, got %v", err)
	}
}

func TestPackNeverIncludesLocalChanges(t *testing.T) {
	dir := writeApp(t, map[string]string{
		"app.yaml": goodManifest, "default/lifecycles/incident.yaml": goodLifecycle,
		"local/lifecycles/incident.yaml": "states:\n  active:\n    label: Secret local tweak\n", "README.md": "hi",
	})
	name, data, err := Pack(dir)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(archive, data, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(archive)
	if err != nil {
		t.Fatal(err)
	}
	if b.HasLocal() {
		t.Error("the archive contains local files")
	}
	for f, content := range b.Files {
		if strings.Contains(string(content), "Secret local tweak") {
			t.Errorf("%s leaks a local change", f)
		}
	}
	if c, err := b.Compile(); err != nil || lifecycleOf(t, c).Label("active") != "Active" {
		t.Errorf("the packed app must be the shipped one: %v", err)
	}
}

func TestInstanceLocalIsReadFromTheLocalRoot(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(root, "demo-app", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("lifecycles/incident.yaml", "states:\n  active:\n    label: Ours\n")
	write("lifecycles/.hidden.yaml", "ignored")

	b := bundle(map[string]string{"app.yaml": goodManifest, "default/lifecycles/incident.yaml": goodLifecycle})
	if err := b.AttachInstanceLocal(root); err != nil {
		t.Fatal(err)
	}
	c, err := b.Compile()
	if err != nil {
		t.Fatal(err)
	}
	if lifecycleOf(t, c).Label("active") != "Ours" {
		t.Error("the instance override was not applied")
	}
	if len(b.Instance) != 1 {
		t.Errorf("hidden files should be ignored: %v", b.Instance)
	}

	// No directory for the app, no root, or an unrelated app: nothing to apply, and that is fine.
	for _, r := range []string{"", t.TempDir()} {
		none := bundle(map[string]string{"app.yaml": goodManifest})
		if err := none.AttachInstanceLocal(r); err != nil || none.HasLocal() {
			t.Errorf("root %q: %v", r, err)
		}
	}
	// An app name that could point outside the root is ignored rather than followed.
	evil := bundle(map[string]string{"app.yaml": strings.Replace(goodManifest, "name: demo-app", "name: ../../etc", 1)})
	if err := evil.AttachInstanceLocal(root); err != nil || evil.HasLocal() {
		t.Errorf("unsafe name was followed: %v", err)
	}
}
