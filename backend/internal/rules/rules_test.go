package rules_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/rules"
)

// compile builds rules from YAML bodies (everything after the kind/name/record_type lines is given per rule).
func compile(t *testing.T, timing string, bodies ...string) []*apps.Rule {
	t.Helper()
	files := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: test-app\nversion: 1.0.0\nauthor: t\ndescription: \"Test app.\"\n")}
	for _, name := range []string{"escalate", "never"} { // for the rules that start a workflow
		files["default/workflows/"+name+".yaml"] = []byte("kind: workflow\nname: " + name + "\nrecord_type: incident\nstart: e\nsteps:\n  e:\n    type: end\n")
	}
	for i, b := range bodies {
		name := string(rune('a' + i))
		files["default/rules/"+name+".yaml"] = []byte("kind: rule\nname: rule-" + name + "\nrecord_type: incident\ntiming: " + timing + "\non: [create, update]\n" + b)
	}
	c, err := (&apps.Bundle{Files: files}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	var out []*apps.Rule
	for _, d := range c.Definitions {
		if d.Rule != nil {
			out = append(out, d.Rule)
		}
	}
	return out
}

func input() expr.Input {
	return expr.Input{
		Record: &expr.Record{Title: "Printer on fire", Priority: 3, Status: "new"},
		Old:    &expr.Record{Title: "Printer on fire", Priority: 3, Status: "new"},
		User:   &expr.User{ID: "u1", Name: "Ada", Role: "agent"},
		Event:  "update",
	}
}

func TestBeforeRulesSetFieldsInOrderAndSeeEachOther(t *testing.T) {
	list := compile(t, "before",
		"order: 1\nrun:\n  - action: set\n    field: priority\n    value: 1\n  - action: set\n    field: title\n    value: '=\"[\" + string(record.priority) + \"] \" + record.title'\n",
		"order: 2\nwhen: 'record.priority == 1'\nrun:\n  - action: set\n    field: assignee_id\n    value: =user.id\n  - action: set\n    field: description\n    value: escalated\n",
	)
	in := input()
	got, err := rules.Before(context.Background(), list, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Priority != 1 || got.Title != "[1] Printer on fire" || got.Description != "escalated" || got.AssigneeID == nil || *got.AssigneeID != "u1" {
		t.Errorf("record: %+v", got)
	}
	if in.Record.Priority != 3 || in.Record.Title != "Printer on fire" {
		t.Errorf("the caller's record was changed: %+v", in.Record)
	}
}

func TestAnEmptyAssigneeUnassigns(t *testing.T) {
	list := compile(t, "before", "run:\n  - action: set\n    field: assignee_id\n    value: \"\"\n")
	in := input()
	id := "u9"
	in.Record.AssigneeID = &id
	got, err := rules.Before(context.Background(), list, in)
	if err != nil || got.AssigneeID != nil {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestFiltersDecideWhatRuns(t *testing.T) {
	list := compile(t, "before",
		"when: 'record.priority == 1'\nrun:\n  - action: abort\n    message: never\n",
		"run:\n  - action: abort\n    message: skipped\n    when: 'event == \"create\"'\n  - action: set\n    field: title\n    value: kept\n",
	)
	got, err := rules.Before(context.Background(), list, input())
	if err != nil || got.Title != "kept" {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestAbortStopsEverything(t *testing.T) {
	list := compile(t, "before",
		"run:\n  - action: abort\n    message: =\"No \" + record.title\n",
		"run:\n  - action: set\n    field: title\n    value: unreachable\n",
	)
	got, err := rules.Before(context.Background(), list, input())
	var ab *rules.Abort
	if !errors.As(err, &ab) || ab.Message != "No Printer on fire" || ab.Rule != "rule-a" || got != nil {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestAfterRulesCollectCommentsAndCanAbort(t *testing.T) {
	list := compile(t, "after",
		"run:\n  - action: comment.add\n    body: =\"Hi \" + user.name\n    internal: true\n  - action: comment.add\n    body: second\n",
	)
	fx, err := rules.After(context.Background(), list, input())
	comments := fx.Comments
	if err != nil || len(comments) != 2 || comments[0].Body != "Hi Ada" || !comments[0].Internal || comments[1].Internal || comments[1].Rule != "rule-a" {
		t.Errorf("got %+v, %v", comments, err)
	}
	list = compile(t, "after", "run:\n  - action: comment.add\n    body: x\n  - action: abort\n    message: stop\n")
	fx, err = rules.After(context.Background(), list, input())
	comments = fx.Comments
	if err == nil || comments != nil {
		t.Errorf("an aborted save must not hand back comments: %+v, %v", comments, err)
	}
}

// A value that cannot be computed stops the save unless the rule says on_error: skip.
func TestBrokenRulesFailClosedUnlessSkipped(t *testing.T) {
	cases := map[string]string{
		"a value of the wrong type":    "run:\n  - action: set\n    field: priority\n    value: '=record.priority / (record.priority - 3)'\n",
		"a condition that errors":      "when: 'record.priority / (record.priority - 3) == 1'\nrun:\n  - action: abort\n    message: x\n",
		"an action filter that errors": "run:\n  - action: abort\n    message: x\n    when: 'record.priority / (record.priority - 3) == 1'\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := rules.Before(context.Background(), compile(t, "before", body), input())
			var br *rules.Broken
			if !errors.As(err, &br) || br.Rule != "rule-a" {
				t.Fatalf("blocking: %v", err)
			}
			got, err := rules.Before(context.Background(), compile(t, "before", "on_error: skip\n"+body), input())
			if err != nil || got == nil {
				t.Errorf("skipping: %+v, %v", got, err)
			}
		})
	}
}

// A skipped rule leaves the record as it was before it, and the rules after it still run.
func TestSkippedRuleDoesNotStopTheNext(t *testing.T) {
	list := compile(t, "before",
		"on_error: skip\nrun:\n  - action: set\n    field: title\n    value: half\n  - action: set\n    field: priority\n    value: '=record.priority / (record.priority - 3)'\n",
		"run:\n  - action: set\n    field: description\n    value: ran\n",
	)
	got, err := rules.Before(context.Background(), list, input())
	if err != nil || got.Description != "ran" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestATimeoutCannotBeSkipped(t *testing.T) {
	list := compile(t, "before", "on_error: skip\nrun:\n  - action: set\n    field: title\n    value: x\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := rules.Before(ctx, list, input())
	var br *rules.Broken
	if !errors.As(err, &br) {
		t.Errorf("got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "rule-a") {
		t.Errorf("error should name the rule: %v", err)
	}
}

// Whether an async rule fires is decided at the save, where the old and new record are both known.
func TestFireJudgesTheChange(t *testing.T) {
	list := compile(t, "async",
		"when: 'record.priority != old.priority'\nrun:\n  - action: comment.add\n    body: changed\n",
		"when: 'record.priority == old.priority'\nrun:\n  - action: comment.add\n    body: same\n",
	)
	in := input()
	in.Record.Priority = 1
	fired, failed := rules.Fire(context.Background(), list, in)
	if len(failed) != 0 || len(fired) != 1 || fired[0].Name != "rule-a" {
		t.Errorf("fired %v, failed %v", fired, failed)
	}
}

// An async rule never blocks a save, so a filter that cannot be evaluated is reported, not raised.
func TestFireReportsFiltersThatFail(t *testing.T) {
	list := compile(t, "async",
		"when: 'record.priority / (record.priority - 3) == 1'\nrun:\n  - action: comment.add\n    body: x\n",
		"run:\n  - action: comment.add\n    body: fine\n",
	)
	fired, failed := rules.Fire(context.Background(), list, input())
	if len(fired) != 1 || fired[0].Name != "rule-b" || len(failed) != 1 || failed[0].Rule != "rule-a" {
		t.Errorf("fired %v, failed %v", fired, failed)
	}
}

func TestRunAsyncTakesTheRuleAsFiredButJudgesEachAction(t *testing.T) {
	// The rule's own filter is false for the current data: it fired at the save, and is not asked again.
	list := compile(t, "async", "when: 'record.priority == 1'\nrun:\n"+
		"  - action: comment.add\n    body: =\"now \" + string(record.priority)\n"+
		"  - action: comment.add\n    body: only while critical\n    when: 'record.priority == 1'\n")
	_, fx, err := rules.RunAsync(context.Background(), list[0], input()) // priority is 3
	comments := fx.Comments
	if err != nil || len(comments) != 1 || comments[0].Body != "now 3" {
		t.Errorf("got %+v, %v", comments, err)
	}
}

// Async runs report every error, on_error or not, so the job retries; and they carry the rule's app.
func TestRunAsyncFailsWhole(t *testing.T) {
	list := compile(t, "async", "run:\n  - action: comment.add\n    body: first\n  - action: comment.add\n    body: '=string(record.priority / (record.priority - 3))'\n")
	list[0].App = "house"
	_, fx, err := rules.RunAsync(context.Background(), list[0], input())
	comments := fx.Comments
	var br *rules.Broken
	if !errors.As(err, &br) || br.App != "house" || br.Rule != "rule-a" || comments != nil {
		t.Errorf("got %+v, %v", comments, err)
	}
}

func TestAWorkflowStartIsAnEffectNotAnAction(t *testing.T) {
	list := compile(t, "after", "run:\n  - action: workflow.start\n    workflow: escalate\n  - action: workflow.start\n    workflow: never\n    when: 'record.priority == 1'\n")
	list[0].App = "house"
	fx, err := rules.After(context.Background(), list, input())
	if err != nil || len(fx.Starts) != 1 || fx.Starts[0] != (rules.Start{App: "house", Workflow: "escalate", Rule: "rule-a"}) {
		t.Errorf("got %+v, %v", fx, err)
	}
}
