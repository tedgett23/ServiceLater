package rules_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/rules"
)

// A before rule's script changes the record being saved, sees the earlier rules' changes, and can refuse the save.
func TestABeforeRuleScriptChangesTheRecordOrRefuses(t *testing.T) {
	list := compile(t, "before",
		"order: 1\nrun:\n  - action: set\n    field: priority\n    value: 2\n",
		"order: 2\nrun:\n  - action: script\n    script: |\n"+
			"      words = record.title.split(\" \")\n"+
			"      if \"fire\" in words:\n"+
			"          update(record, {\"priority\": record.priority - 1, \"title\": record.title.upper()})\n"+
			"      if record.description == \"stop\":\n"+
			"          abort(\"Not like that, \" + user.name)\n")
	rec, err := rules.Before(context.Background(), list, input())
	if err != nil || rec.Priority != 1 || rec.Title != "PRINTER ON FIRE" {
		t.Fatalf("before: %v %+v", err, rec)
	}
	in := input()
	in.Record.Description = "stop"
	_, err = rules.Before(context.Background(), list, in)
	var ab *rules.Abort
	if !errors.As(err, &ab) || ab.Message != "Not like that, Ada" {
		t.Errorf("abort: %#v", err)
	}
}

// What a before rule's script may not do fails the rule (closed), with the reason.
func TestABeforeRuleScriptStaysInItsLane(t *testing.T) {
	for src, want := range map[string]string{
		`update(record, {"status": "closed"})`: "cannot move the record",
		`comment(record, "hi")`:                "comment() is not available here",
		`return {"next": "x"}`:                 "returns nothing",
		"while True:\n          pass":          "steps",
	} {
		list := compile(t, "before", "run:\n  - action: script\n    script: |\n      "+src+"\n")
		if _, err := rules.Before(context.Background(), list, input()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", src, want, err)
		}
	}
}

// An async rule's script hands its changes back to the caller, which makes them through the save path.
func TestAnAsyncRuleScriptHandsBackItsChanges(t *testing.T) {
	list := compile(t, "async", "run:\n  - action: script\n    script: |\n      comment(record, \"Seen by \" + user.name, internal=True)\n      update(record, {\"priority\": 1})\n")
	in := input()
	in.Record.ID = "r1"
	_, fx, err := rules.RunAsync(context.Background(), list[0], in)
	if err != nil || len(fx.Scripts) != 1 {
		t.Fatalf("async: %v %+v", err, fx)
	}
	res := fx.Scripts[0].Result
	if len(res.Comments) != 1 || res.Comments[0].Body != "Seen by Ada" || !res.Comments[0].Internal || res.Updates[0].Fields["priority"] != int64(1) {
		t.Errorf("result: %+v", res)
	}
}

// Script mistakes fail the install, with the line; scripts are not for after rules.
func TestScriptActionsAreCheckedAtInstall(t *testing.T) {
	for timing, tc := range map[string]struct{ src, want string }{
		"before": {"run:\n  - action: script\n    script: |\n      x = 1\n      upate(record, {})\n", "run[0]: script: line 2: undefined: upate"},
		"after":  {"run:\n  - action: script\n    script: |\n      pass\n", "not an after rule"},
		"async":  {"run:\n  - action: script\n    field: title\n    script: |\n      pass\n", "script does not take field"},
	} {
		files := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: test-app\nversion: 1.0.0\nauthor: t\ndescription: \"Test app.\"\n"),
			"default/rules/a.yaml": []byte("kind: rule\nname: rule-a\nrecord_type: incident\ntiming: " + timing + "\non: [update]\n" + tc.src)}
		_, err := (&apps.Bundle{Files: files}).Compile()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", timing, tc.want, err)
		}
	}
}
