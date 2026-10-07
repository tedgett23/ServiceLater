package apps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

const goodSLA = `kind: sla
name: resolve
label: Resolve
record_type: incident
target: '=record.priority == 1 ? duration("4h") : duration("48h")'
calendar: business-hours
warnings:
  - at: 75
    run:
      - action: notify
        template: late
        to: [assignee]
on_breach:
  - action: notify
    template: late
    to: [assignee, assignment_group]
  - action: workflow.start
    workflow: escalate
`

const lateTemplate = "kind: notification\nname: late\ntitle: \"{{record.reference}} is late\"\n"

const escalateWorkflow = `kind: workflow
name: escalate
record_type: incident
start: done
steps:
  done:
    type: end
`

func slaApp(sla string) map[string]string {
	return map[string]string{"app.yaml": goodManifest, "default/slas/s.yaml": sla,
		"default/notifications/late.yaml": lateTemplate, "default/workflows/escalate.yaml": escalateWorkflow}
}

// An SLA compiles to conditions (the defaults where it gives none), a target worked out from the record, and its
// warning and breach actions as async rules, and survives the stored form a clock keeps.
func TestAnSLACompilesToItsConditionsTargetAndActions(t *testing.T) {
	c, p := compile(t, slaApp(goodSLA))
	if p != nil {
		t.Fatal(p)
	}
	var s *SLA
	for _, d := range c.Definitions {
		if d.SLA != nil {
			s = d.SLA
		}
	}
	if s == nil || s.Start != DefaultSLAStart || s.Pause != DefaultSLAPause || s.Stop != DefaultSLAStop {
		t.Fatalf("defaults: %+v", s)
	}
	ctx := context.Background()
	start, pause, stop, err := s.Conditions()
	if err != nil {
		t.Fatal(err)
	}
	rec := &expr.Record{Priority: 1, StateCategory: "waiting"}
	in := expr.Input{Record: rec}
	if ok, _ := start.Eval(ctx, in); !ok {
		t.Error("start")
	}
	if ok, _ := pause.Eval(ctx, in); !ok {
		t.Error("a waiting record pauses")
	}
	if ok, _ := stop.Eval(ctx, in); ok {
		t.Error("a waiting record is not finished")
	}
	_, prog, err := s.TargetValue()
	if err != nil || prog == nil {
		t.Fatalf("target: %v", err)
	}
	if v, err := prog.EvalValue(ctx, in); err != nil || v != 4*time.Hour {
		t.Errorf("P1 target: %v %v", v, err)
	}
	rec.Priority = 3
	if v, _ := prog.EvalValue(ctx, in); v != 48*time.Hour {
		t.Errorf("P3 target: %v", v)
	}
	if w := s.WarningRule(0); w == nil || w.Timing != TimingAsync || len(w.Run) != 1 || w.Run[0].Template != "late" {
		t.Errorf("warning: %+v", w)
	}
	if b := s.BreachRule(); b == nil || len(b.Run) != 2 || b.RecordType != "incident" {
		t.Errorf("breach: %+v", b)
	}
	if s.WarningRule(1) != nil {
		t.Error("a warning that is not there")
	}

	body, _ := json.Marshal(s)
	stored, err := SLAFromSnapshot(body, Checksum(body), "demo-app")
	if err != nil || stored.Label != "Resolve" || stored.Calendar != "business-hours" || stored.BreachRule().App != "demo-app" {
		t.Fatalf("stored: %+v %v", stored, err)
	}
	if again, _ := SLAFromSnapshot(body, Checksum(body), "demo-app"); again != stored {
		t.Error("a snapshot is compiled once")
	}
	if _, prog, _ := stored.TargetValue(); prog == nil {
		t.Error("the stored target lost its expression")
	}
}

func TestLiteralTargets(t *testing.T) {
	for in, want := range map[string]time.Duration{"4h": 4 * time.Hour, "90m": 90 * time.Minute, "2d": 48 * time.Hour, "1d12h": 36 * time.Hour} {
		if got, err := ParseTarget(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "soon", "4", "400d", "2d4"} {
		if d, err := ParseTarget(in); err == nil && d > 0 && d <= MaxSLATarget {
			t.Errorf("%q accepted as %v", in, d)
		}
	}
}

func TestSLAsAreCheckedAtInstall(t *testing.T) {
	with := func(old, new string) string { return strings.Replace(goodSLA, old, new, 1) }
	for name, tc := range map[string]struct{ src, want string }{
		"no label":        {with("label: Resolve\n", ""), "label is required"},
		"bad type":        {with("record_type: incident", "record_type: ticket"), `record_type "ticket"`},
		"no target":       {with(`target: '=record.priority == 1 ? duration("4h") : duration("48h")'`+"\n", ""), "target is required"},
		"bare number":     {with(`'=record.priority == 1 ? duration("4h") : duration("48h")'`, "4"), "a bare number has no unit"},
		"bad literal":     {with(`'=record.priority == 1 ? duration("4h") : duration("48h")'`, "soon"), "such as 4h"},
		"too long":        {with(`'=record.priority == 1 ? duration("4h") : duration("48h")'`, "400d"), "too many days"},
		"target not time": {with(`duration("48h")'`, `"2d"'`), "no matching overload"},
		"target type":     {with(`'=record.priority == 1 ? duration("4h") : duration("48h")'`, "=record.priority"), "must produce a duration"},
		"bad condition":   {with("label: Resolve\n", "label: Resolve\npause: record.prioity == 1\n"), "pause: "},
		"lookups":         {with("label: Resolve\n", "label: Resolve\nstop: lookups.x > 0\n"), "an SLA cannot read lookups"},
		"bad calendar":    {with("calendar: business-hours", "calendar: Business Hours"), "not a calendar name"},
		"warning order":   {with("  - at: 75\n", "  - at: 75\n    run: [{action: notify, template: late, to: [assignee]}]\n  - at: 50\n"), "lowest percentage up"},
		"warning share":   {with("at: 75", "at: 100"), "from 1 to 99"},
		"no warning run":  {with("    run:\n      - action: notify\n        template: late\n        to: [assignee]\n", ""), "run is required"},
		"set action":      {with("  - action: workflow.start\n    workflow: escalate\n", "  - action: record.update\n    field: priority\n    value: 1\n"), "an SLA can notify, workflow.start, comment.add, not record.update"},
		"no template":     {with("template: late\n    to: [assignee, assignment_group]", "template: missing\n    to: [assignee]"), "on_breach: run[0]: there is no notification template \"missing\""},
		"no workflow":     {with("workflow: escalate", "workflow: absent"), "on_breach: run[1]: there is no workflow \"absent\""},
		"bad recipient":   {with("to: [assignee]", "to: [boss]"), `warnings[0]: run[0]: to[0]: "boss" is not a recipient`},
	} {
		_, p := compile(t, slaApp(tc.src))
		if p == nil || !strings.Contains(p.Error(), tc.want) {
			t.Errorf("%s: want %q in %v", name, tc.want, p)
		}
	}
}
