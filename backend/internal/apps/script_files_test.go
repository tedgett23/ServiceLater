package apps

import (
	"strings"
	"testing"
)

const helpersStar = `def raise_if_urgent(record):
    if "urgent" in record.title.lower():
        update(record, {"priority": 1})
`

const scriptStepFlow = `kind: workflow
name: triage
record_type: incident
start: s
steps:
  s:
    type: script
    script: |
      load("helpers.star", "raise_if_urgent")
      raise_if_urgent(record)
    next: done
  done:
    type: end
`

// An app's script files are loaded by its script steps and actions; each workflow and rule keeps the files it uses,
// so it compiles the same way from storage whatever the files become.
func TestScriptFilesAreKeptWithWhatLoadsThem(t *testing.T) {
	c, p := compile(t, map[string]string{"app.yaml": goodManifest, "default/scripts/helpers.star": helpersStar,
		"default/scripts/unused.star":   "def nothing():\n    pass\n",
		"default/workflows/triage.yaml": scriptStepFlow,
		"default/rules/raise.yaml":      "kind: rule\nname: raise\nrecord_type: incident\ntiming: before\non: [create]\nrun:\n  - action: script\n    script: |\n      load(\"helpers.star\", \"raise_if_urgent\")\n      raise_if_urgent(record)\n"})
	if p != nil {
		t.Fatal(p)
	}
	var files int
	for _, d := range c.Definitions {
		switch {
		case d.ScriptFile != nil:
			files++
		case d.Workflow != nil:
			if len(d.Workflow.Modules) != 1 || d.Workflow.Modules["helpers.star"] != helpersStar {
				t.Errorf("the workflow keeps: %v", d.Workflow.Modules)
			}
			if _, err := d.Workflow.Steps[0].Program(); err != nil {
				t.Errorf("the step: %v", err)
			}
		case d.Rule != nil:
			if len(d.Rule.Modules) != 1 {
				t.Errorf("the rule keeps: %v", d.Rule.Modules)
			}
		}
	}
	if files != 2 {
		t.Errorf("%d script files", files)
	}
}

func TestScriptFileMistakesFailTheInstall(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"bad file":      {map[string]string{"default/scripts/helpers.star": "def f(:\n"}, "helpers.star line 1"},
		"bad name":      {map[string]string{"default/scripts/Helpers.star": helpersStar}, "a script file is scripts/<name>.star"},
		"not star":      {map[string]string{"default/scripts/helpers.py": helpersStar}, "a script file is scripts/<name>.star"},
		"missing file":  {map[string]string{"default/workflows/triage.yaml": scriptStepFlow}, "this app has no script file helpers.star"},
		"missing name":  {map[string]string{"default/scripts/helpers.star": "def other(r):\n    pass\n", "default/workflows/triage.yaml": scriptStepFlow}, "helpers.star defines no raise_if_urgent"},
		"another app's": {map[string]string{"default/workflows/triage.yaml": strings.Replace(scriptStepFlow, `"helpers.star"`, `"@core-itsm//helpers.star"`, 1)}, "only this app's script files"},
	} {
		files := map[string]string{"app.yaml": goodManifest}
		for k, v := range tc.files {
			files[k] = v
		}
		_, p := compile(t, files)
		if p == nil || !strings.Contains(p.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, p)
		}
	}
}

// A local script file replaces the shipped one whole (code is never merged), and what loads it gets the local one.
func TestALocalScriptFileReplacesTheShippedOne(t *testing.T) {
	local := "def raise_if_urgent(record):\n    update(record, {\"priority\": 2})\n"
	b := layered(map[string]string{"app.yaml": goodManifest, "default/scripts/helpers.star": helpersStar, "default/workflows/triage.yaml": scriptStepFlow},
		map[string]string{"scripts/helpers.star": local})
	c, err := b.Compile()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range c.Definitions {
		if d.Workflow != nil && d.Workflow.Modules["helpers.star"] != local {
			t.Errorf("the workflow keeps: %q", d.Workflow.Modules["helpers.star"])
		}
		if d.ScriptFile != nil && d.Source() != "default+instance" {
			t.Errorf("layers: %s", d.Source())
		}
	}
}
