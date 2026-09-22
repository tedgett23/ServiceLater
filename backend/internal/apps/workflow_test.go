package apps

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const goodWorkflow = `kind: workflow
name: critical-escalation
record_type: incident
start: tag
steps:
  tag:
    type: set
    vars:
      level: =record.priority
      note: "escalated"
    next: wait_ack
  wait_ack:
    type: wait
    until: 'record.status != "new"'
    timeout: 15m
    on_timeout: escalate
    next: done
  escalate:
    type: record.update
    set:
      priority: 1
      title: '="[URGENT] " + record.title'
    next: say
  say:
    type: comment.add
    body: '=vars.note + " after " + string(vars.level)'
    internal: true
    next: cool
  cool:
    type: wait
    for: 2h
    next: decide
  decide:
    type: condition
    if: 'record.priority == 1'
    then: done
    else: done2
  done:
    type: end
  done2:
    type: end
`

func workflowApp(files map[string]string) map[string]string {
	out := map[string]string{"app.yaml": goodManifest}
	for name, content := range files {
		out["default/workflows/"+name] = content
	}
	return out
}

func TestAGoodWorkflowCompiles(t *testing.T) {
	c, p := compile(t, workflowApp(map[string]string{"esc.yaml": goodWorkflow}))
	if p != nil {
		t.Fatal(p)
	}
	w := c.Definitions[0].Workflow
	if c.Definitions[0].Kind != "workflow" || w == nil || w.Name != "critical-escalation" || len(w.Steps) != 8 || w.Start != "tag" {
		t.Fatalf("definition: %+v", c.Definitions[0])
	}
	if w.Steps[0].ID != "tag" || w.Steps[7].ID != "done2" {
		t.Errorf("the author's order was not kept: %v", w.Steps)
	}
	if w.Step("cool").ForDuration().Hours() != 2 || w.Step("wait_ack").TimeoutDuration().Minutes() != 15 {
		t.Error("durations were not parsed")
	}
}

func TestWorkflowsAreRejectedWhenTheyAreWrong(t *testing.T) {
	with := func(old, new string) map[string]string {
		return workflowApp(map[string]string{"esc.yaml": strings.Replace(goodWorkflow, old, new, 1)})
	}
	one := func(steps string) map[string]string {
		return workflowApp(map[string]string{"w.yaml": "kind: workflow\nname: ww\nrecord_type: incident\nstart: a\nsteps:\n" + steps})
	}
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"a bad name", with("name: critical-escalation", "name: Bad_Name"), `name "Bad_Name"`},
		{"an unknown record type", with("record_type: incident", "record_type: widget"), `record_type "widget"`},
		{"no start", with("start: tag\n", ""), "start is required"},
		{"a start that is not a step", with("start: tag", "start: nowhere"), `start: there is no step "nowhere"`},
		{"the wrong kind", with("kind: workflow", "kind: rule"), "kind must be workflow"},
		{"an unknown field", with("record_type: incident", "record_type: incident\nowner: me"), "field owner not found"},
		{"an unknown step field", with("    type: end\n  done2", "    type: end\n    nxt: x\n  done2"), "step \"done\""},
		{"no steps", workflowApp(map[string]string{"w.yaml": "kind: workflow\nname: ww\nrecord_type: incident\nstart: a\nsteps: {}\n"}), "a workflow needs steps"},
		{"a step id with capitals", one("  Bad:\n    type: end\n"), `step id "Bad"`},
		{"an unknown step type", one("  a:\n    type: explode\n"), `unknown step type "explode"`},
		{"a step with no type", one("  a:\n    next: a\n"), "a step needs a type"},
		{"a next that does not exist", with("next: wait_ack", "next: nowhere"), `there is no step "nowhere"`},
		{"a step that goes to itself", one("  a:\n    type: set\n    vars: { x: 1 }\n    next: a\n"), "a step cannot go to itself"},
		{"a step with no next", one("  a:\n    type: set\n    vars: { x: 1 }\n"), "set needs next"},
		{"a field the type does not use", one("  a:\n    type: end\n    next: a\n"), "end does not take next"},
		{"a loop", one("  a:\n    type: set\n    vars: { x: 1 }\n    next: b\n  b:\n    type: set\n    vars: { y: 1 }\n    next: c\n  c:\n    type: condition\n    if: 'true'\n    then: a\n    else: d\n  d:\n    type: end\n"), "the steps loop: a -> b -> c -> a"},
		{"a step nothing leads to", with("  done2:\n    type: end\n", "  done2:\n    type: end\n  orphan:\n    type: end\n"), "step orphan can never run"},
		{"set with no vars", one("  a:\n    type: set\n    next: b\n  b:\n    type: end\n"), "set needs vars"},
		{"a bad variable name", one("  a:\n    type: set\n    vars: { Bad-Name: 1 }\n    next: b\n  b:\n    type: end\n"), `"Bad-Name" is not a variable name`},
		{"an expression with a typo", with("if: 'record.priority == 1'", "if: 'record.prioity == 1'"), "step decide: if: column"},
		{"a condition that is not true or false", with("if: 'record.priority == 1'", "if: 'record.priority'"), "must produce true or false"},
		{"a variable expression that does not compile", with("level: =record.priority", "level: =record.prioity"), "vars.level: column"},
		{"a condition with the same branch twice", with("else: done2", "else: done"), "decides nothing"},
		{"wait with neither for nor until", one("  a:\n    type: wait\n    next: b\n  b:\n    type: end\n"), "wait needs for"},
		{"wait with both", one("  a:\n    type: wait\n    for: 1m\n    until: 'true'\n    timeout: 1m\n    next: b\n  b:\n    type: end\n"), "either for or until"},
		{"wait until with no timeout", one("  a:\n    type: wait\n    until: 'true'\n    next: b\n  b:\n    type: end\n"), "wait until needs a timeout"},
		{"a wait that is too long", one("  a:\n    type: wait\n    for: 31d\n    next: b\n  b:\n    type: end\n"), "for must be between"},
		{"a wait that is not a duration", one("  a:\n    type: wait\n    for: soon\n    next: b\n  b:\n    type: end\n"), `"soon" is not a duration`},
		{"a timeout on a plain wait", one("  a:\n    type: wait\n    for: 1m\n    timeout: 1m\n    next: b\n  b:\n    type: end\n"), "wait does not take timeout"},
		{"record.update with no set", one("  a:\n    type: record.update\n    next: b\n  b:\n    type: end\n"), "record.update needs set"},
		{"record.update of a field that cannot be set", one("  a:\n    type: record.update\n    set: { status: closed }\n    next: b\n  b:\n    type: end\n"), `cannot change "status"`},
		{"record.update with a priority out of range", one("  a:\n    type: record.update\n    set: { priority: 9 }\n    next: b\n  b:\n    type: end\n"), "priority must be 1 (critical) to 4 (low)"},
		{"record.update with an expression of the wrong type", one("  a:\n    type: record.update\n    set: { priority: '=record.title' }\n    next: b\n  b:\n    type: end\n"), "must produce a whole number"},
		{"comment.add with no body", one("  a:\n    type: comment.add\n    next: b\n  b:\n    type: end\n"), "comment.add needs a body"},
		{"too many steps", workflowApp(map[string]string{"w.yaml": manySteps(51)}), "at most 50 steps"},
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

func manySteps(n int) string {
	var b strings.Builder
	b.WriteString("kind: workflow\nname: many\nrecord_type: incident\nstart: s0\nsteps:\n")
	for i := 0; i < n; i++ {
		if i == n-1 {
			fmt.Fprintf(&b, "  s%02d:\n    type: end\n", i)
		} else {
			fmt.Fprintf(&b, "  s%02d:\n    type: set\n    vars: { x: 1 }\n    next: s%02d\n", i, i+1)
		}
	}
	return b.String()
}

// A workflow is stored as JSON and read back by the registry, and pinned in every execution. A literal 2 must still
// be a whole number, or the workflow installs fine and is then dropped as invalid.
func TestWorkflowsSurviveTheStoredForm(t *testing.T) {
	c, p := compile(t, workflowApp(map[string]string{"esc.yaml": goodWorkflow}))
	if p != nil {
		t.Fatal(p)
	}
	body, err := json.Marshal(c.Definitions[0].Workflow)
	if err != nil {
		t.Fatal(err)
	}
	var back Workflow
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	var problems Problems
	back.validate("stored", &problems)
	if problems.Error() != "" {
		t.Fatalf("a stored workflow no longer validates: %v", problems.Error())
	}
	if n, ok := back.Step("escalate").Set[0].Value.Literal.(int64); !ok || n != 1 {
		t.Errorf("literal came back as %T", back.Step("escalate").Set[0].Value.Literal)
	}
	if back.Steps[0].ID != "tag" || back.Step("say").Body.Expr == "" || !back.Step("say").Internal {
		t.Errorf("steps: %+v", back.Steps)
	}
	// Compiling a pinned snapshot works the same as compiling the definition.
	back.Compile()
	if _, err := back.Step("decide").Condition(); err != nil {
		t.Error(err)
	}
}

func TestARuleMayStartAWorkflowOfItsOwnApp(t *testing.T) {
	rule := func(action string) string {
		return "kind: rule\nname: start-it\nrecord_type: incident\ntiming: after\non: [create]\nrun:\n" + action
	}
	files := func(action string) map[string]string {
		f := workflowApp(map[string]string{"esc.yaml": goodWorkflow})
		f["default/rules/start.yaml"] = rule(action)
		return f
	}
	if _, p := compile(t, files("  - action: workflow.start\n    workflow: critical-escalation\n")); p != nil {
		t.Errorf("a valid start: %v", p)
	}
	async := strings.Replace(rule("  - action: workflow.start\n    workflow: critical-escalation\n"), "timing: after", "timing: async", 1)
	f := workflowApp(map[string]string{"esc.yaml": goodWorkflow})
	f["default/rules/start.yaml"] = async
	if _, p := compile(t, f); p != nil {
		t.Errorf("an async start: %v", p)
	}
	for name, c := range map[string]struct{ action, want string }{
		"a workflow that does not exist": {"  - action: workflow.start\n    workflow: nope\n", `there is no workflow "nope" in this app (workflows: critical-escalation)`},
		"no workflow named":              {"  - action: workflow.start\n", "workflow.start needs workflow"},
		"a stray field":                  {"  - action: workflow.start\n    workflow: critical-escalation\n    body: x\n", "workflow.start does not take body"},
		"a bad workflow name":            {"  - action: workflow.start\n    workflow: Bad_Name\n", "is not a workflow name"},
	} {
		t.Run(name, func(t *testing.T) {
			_, p := compile(t, files(c.action))
			if p == nil || !strings.Contains(p.Error(), c.want) {
				t.Errorf("got %v, want %q", p, c.want)
			}
		})
	}
	// Not in a before rule: the ticket does not exist yet.
	f = workflowApp(map[string]string{"esc.yaml": goodWorkflow})
	f["default/rules/start.yaml"] = strings.Replace(rule("  - action: workflow.start\n    workflow: critical-escalation\n"), "timing: after", "timing: before", 1)
	if _, p := compile(t, f); p == nil || !strings.Contains(p.Error(), "only allowed in an after or async rule") {
		t.Errorf("before: %v", p)
	}
	// A workflow for another kind of record cannot be started by this rule.
	f = workflowApp(map[string]string{"esc.yaml": strings.Replace(goodWorkflow, "record_type: incident", "record_type: change", 1)})
	f["default/rules/start.yaml"] = rule("  - action: workflow.start\n    workflow: critical-escalation\n")
	if _, p := compile(t, f); p == nil || !strings.Contains(p.Error(), "is for change records") {
		t.Errorf("wrong record type: %v", p)
	}
}

// Layers apply to workflows like everything else: a local file changes one step and keeps the rest.
func TestALocalFileCanAdjustOneStepOfAWorkflow(t *testing.T) {
	b := layered(map[string]string{
		"app.yaml": goodManifest, "default/workflows/esc.yaml": goodWorkflow,
		"local/workflows/esc.yaml": "steps:\n  wait_ack:\n    timeout: 1h\n",
	}, nil)
	c, err := b.Compile()
	if err != nil {
		t.Fatal(err)
	}
	w := c.Definitions[0].Workflow
	if w.Step("wait_ack").TimeoutDuration().Hours() != 1 || w.Step("wait_ack").Until == "" || len(w.Steps) != 8 {
		t.Errorf("merged: %+v", w.Step("wait_ack"))
	}
}
