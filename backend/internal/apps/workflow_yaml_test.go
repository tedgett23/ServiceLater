package apps

import (
	"encoding/json"
	"strings"
	"testing"
)

// The builder saves a workflow with WorkflowYAML: every shipped workflow written back must read as the same one.
func TestWorkflowsWriteBackAsTheyRead(t *testing.T) {
	n := 0
	for _, name := range []string{"change-management", "core-itsm", "incident-management", "service-catalog"} {
		b, err := LoadBundled(name)
		if err != nil {
			t.Fatal(err)
		}
		c, err := b.Compile()
		if err != nil {
			t.Fatal(err)
		}
		// The app's fields, as compiling it gave its workflows (a form's, and catalog items' questions).
		forms := formSet{}
		var p Problems
		for _, d := range c.Definitions {
			if d.Form != nil {
				forms[d.Form.RecordType] = d.Form
			}
			if d.CatalogItem != nil {
				forms.addItemFields(d.CatalogItem, d.File, &p)
			}
		}
		for _, d := range c.Definitions {
			if d.Workflow == nil {
				continue
			}
			n++
			d.Workflow.Layout = map[string]Point{d.Workflow.Start: {X: 10, Y: -20}}
			data, err := WorkflowYAML(d.Workflow, false)
			if err != nil {
				t.Fatalf("%s: %v", d.Name, err)
			}
			again := parseWorkflow(d.Path, data, forms, nil, &p)
			if again == nil {
				t.Fatalf("%s does not read back: %v\n%s", d.Name, p, data)
			}
			again.Fields = d.Workflow.Fields
			want, _ := json.Marshal(d.Workflow)
			got, _ := json.Marshal(again)
			if string(want) != string(got) {
				t.Errorf("%s changed on the way back:\nwant %s\n got %s", d.Name, want, got)
			}
		}
	}
	if n == 0 {
		t.Fatal("no shipped workflows to try")
	}
}

// An override that says override: replace replaces the shipped definition whole; one with nothing below it still
// reads.
func TestAReplaceOverrideIsNotMerged(t *testing.T) {
	base := []byte("kind: workflow\nname: x\nsteps:\n  a: {type: end}\n  b: {type: end}\n")
	over := []byte("override: replace\nkind: workflow\nname: x\nsteps:\n  a: {type: end}\n")
	out, err := mergeYAML(base, over)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "b:") || strings.Contains(string(out), "override") {
		t.Errorf("replaced: %s", out)
	}
	if merged, _ := mergeYAML(base, []byte("steps:\n  c: {type: end}\n")); !strings.Contains(string(merged), "b:") {
		t.Errorf("a plain override still merges: %s", merged)
	}
	if alone, ok := withoutReplace(over); !ok || strings.Contains(string(alone), "override") {
		t.Errorf("alone: %s", alone)
	}
}

// An edit made in the builder is kept as only what it changed, so the app's later changes to the rest arrive: here
// the app's next version rewords one comment while the edit added a step elsewhere.
func TestAnEditKeepsTheAppsLaterChanges(t *testing.T) {
	v1 := "kind: workflow\nname: greet\nrecord_type: incident\nstart: hello\nsteps:\n" +
		"  hello: {type: comment.add, body: Hello, next: done}\n  done: {type: end}\n"
	var p Problems
	wf := parseWorkflow("w", []byte(v1), formSet{}, nil, &p)
	if wf == nil {
		t.Fatal(p)
	}
	// The edit: a wait before the end.
	wf.Steps[0].Next = "pause"
	wf.Steps = append(wf.Steps, Step{ID: "pause", Type: StepWait, For: "1h", Next: "done"})
	over, err := WorkflowOverride([]byte(v1), wf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(over), "replace") || strings.Contains(string(over), "Hello") {
		t.Fatalf("not a minimal override:\n%s", over)
	}
	if none, _ := WorkflowOverride([]byte(v1), parseWorkflow("w", []byte(v1), formSet{}, nil, &p)); none != nil {
		t.Errorf("no change is no override: %s", none)
	}

	v2 := strings.Replace(v1, "body: Hello", "body: Hello there", 1)
	c, problems := compile(t, map[string]string{"app.yaml": goodManifest, "default/workflows/greet.yaml": v2, "local/workflows/greet.yaml": string(over)})
	if problems != nil {
		t.Fatal(problems)
	}
	for _, d := range c.Definitions {
		if d.Workflow == nil {
			continue
		}
		var ids []string
		for _, s := range d.Workflow.Steps {
			ids = append(ids, s.ID)
		}
		if body := d.Workflow.Steps[0].Body.Literal; body != "Hello there" || !strings.Contains(strings.Join(ids, ","), "pause") {
			t.Errorf("both changes: body %v, steps %v", body, ids)
		}
	}
}
