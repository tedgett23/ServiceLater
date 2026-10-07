package apps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

const freezeRule = `kind: rule
name: respect-freezes
record_type: change
timing: before
on: [create, update]
lookups:
  freezes:
    count: freeze_window
    where:
      starts_at: { lt: =record.planned_end }
      ends_at:   { gt: =record.planned_start }
  mine:
    count: change
    where:
      requester_id: { eq: =record.requester_id }
      change_type:  { in: '=["normal", "emergency"]' }
      state_category: { ne: closed }
when: lookups.freezes > 0 && lookups['mine'] < 5
run:
  - action: abort
    message: =record.reference + ' overlaps ' + string(lookups.freezes) + ' freezes.'
`

// countRecorder is a Count that remembers what it was asked and answers a fixed number per target.
type countRecorder struct {
	asked map[string][]expr.Filter
	n     map[string]int64
	err   error
}

func (c *countRecorder) count(_ context.Context, target string, filters []expr.Filter) (int64, error) {
	if c.asked == nil {
		c.asked = map[string][]expr.Filter{}
	}
	c.asked[target] = filters
	return c.n[target], c.err
}

// A rule's lookups are counted with their filters' values worked out from the record, in a fixed order, and the
// rule's expressions read the counts.
func TestALookupCountsWithValuesFromTheRecord(t *testing.T) {
	c, p := compile(t, ruleApp(map[string]string{"freezes.yaml": freezeRule}))
	if p != nil {
		t.Fatal(p)
	}
	r := c.Definitions[0].Rule
	start := time.Date(2026, 12, 20, 22, 0, 0, 0, time.UTC)
	rec := &expr.Record{Reference: "CHG4", RequesterID: "u1", PlannedStart: start, PlannedEnd: start.Add(2 * time.Hour)}
	counter := &countRecorder{n: map[string]int64{"freeze_window": 2, "change": 1}}
	got, err := r.Lookups.Eval(context.Background(), expr.Input{Record: rec, Count: counter.count})
	if err != nil || got["freezes"] != 2 || got["mine"] != 1 {
		t.Fatalf("counts: %v %v", got, err)
	}
	want := []expr.Filter{{Field: "ends_at", Op: "gt", Value: start}, {Field: "starts_at", Op: "lt", Value: start.Add(2 * time.Hour)}}
	if b, w := mustJSON(t, counter.asked["freeze_window"]), mustJSON(t, want); b != w {
		t.Errorf("freeze filters:\n got %s\nwant %s", b, w)
	}
	mine := counter.asked["change"]
	if len(mine) != 3 || mine[0].Field != "change_type" || mine[0].Op != "in" || len(mine[0].Value.([]string)) != 2 ||
		mine[1].Value != "u1" || mine[2].Value != "closed" {
		t.Errorf("change filters: %+v", mine)
	}

	in := expr.Input{Record: rec, Lookups: got}
	if ok, err := mustProgram(t, r.Condition).Eval(context.Background(), in); err != nil || !ok {
		t.Errorf("the rule reads the counts: %v %v", ok, err)
	}
	msg, err := r.Run[0].Message.program.EvalValue(context.Background(), in)
	if err != nil || msg != "CHG4 overlaps 2 freezes." {
		t.Errorf("an action reads them too: %v %v", msg, err)
	}

	// No way to count, or a failing count, is an error: the caller fails closed.
	if _, err := r.Lookups.Eval(context.Background(), expr.Input{Record: rec}); err == nil {
		t.Error("lookups without a counter")
	}
	counter.err = errors.New("db down")
	if _, err := r.Lookups.Eval(context.Background(), expr.Input{Record: rec, Count: counter.count}); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Errorf("a failed count: %v", err)
	}

	// The lookups survive the stored form, which is how the running server gets them.
	var stored Rule
	if err := json.Unmarshal(c.Definitions[0].Body, &stored); err != nil {
		t.Fatal(err)
	}
	stored.compile()
	counter.err = nil
	if got, err := stored.Lookups.Eval(context.Background(), expr.Input{Record: rec, Count: counter.count}); err != nil || got["freezes"] != 2 {
		t.Errorf("stored: %v %v", got, err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustProgram(t *testing.T, get func() (*expr.Program, error)) *expr.Program {
	t.Helper()
	p, err := get()
	if err != nil || p == nil {
		t.Fatalf("program: %v", err)
	}
	return p
}

func TestLookupsAreCheckedAtInstall(t *testing.T) {
	rule := func(lookups, when string) string {
		return "kind: rule\nname: r\nrecord_type: change\ntiming: before\non: [update]\nlookups:\n" + lookups +
			"when: " + when + "\nrun:\n  - action: abort\n    message: no\n"
	}
	ok := "  f:\n    count: freeze_window\n    where:\n      starts_at: { lt: =now }\n"
	for name, tc := range map[string]struct{ src, want string }{
		"undeclared":    {rule(ok, "lookups.g > 0"), "reads lookups.g, which is not declared under lookups"},
		"dynamic name":  {rule(ok, "lookups[record.title] > 0"), "read a lookup as lookups.name"},
		"unknown kind":  {rule("  f:\n    count: users\n    where:\n      id: { eq: x }\n", "true"), `count "users" is not something a lookup can count`},
		"unknown field": {rule("  f:\n    count: change\n    where:\n      title: { eq: x }\n", "true"), `change cannot be filtered by "title"`},
		"unknown op":    {rule("  f:\n    count: change\n    where:\n      status: { like: x }\n", "true"), `"like" is not an operator`},
		"no filter":     {rule("  f:\n    count: change\n    where: {}\n", "true"), "needs where: at least one filter"},
		"time literal":  {rule("  f:\n    count: freeze_window\n    where:\n      starts_at: { lt: '2026-01-01' }\n", "true"), "a time is written as an =expression"},
		"wrong type":    {rule("  f:\n    count: freeze_window\n    where:\n      starts_at: { lt: =record.title }\n", "true"), "must produce google.protobuf.Timestamp"},
		"in literal":    {rule("  f:\n    count: change\n    where:\n      status: { in: open }\n", "true"), "in takes an =expression giving a list of text"},
		"reads lookups": {rule("  f:\n    count: change\n    where:\n      status: { eq: '=string(lookups.f)' }\n", "true"), "a lookup's filter cannot read lookups"},
		"bad name":      {rule("  Bad:\n    count: change\n    where:\n      status: { eq: open }\n", "true"), "a lookup's name must be lowercase"},
		"too many": {rule(func() string {
			var b strings.Builder
			for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
				b.WriteString("  " + n + ":\n    count: change\n    where:\n      status: { eq: open }\n")
			}
			return b.String()
		}(), "true"), "at most 5 per definition"},
	} {
		_, p := compile(t, ruleApp(map[string]string{"r.yaml": tc.src}))
		if p == nil || !strings.Contains(p.Error(), tc.want) {
			t.Errorf("%s: want %q in %v", name, tc.want, p)
		}
	}
}

// A move may have several guards, each with its own message, and lookups they read. The first that fails says why.
func TestAMoveCanHaveSeveralGuardsAndLookups(t *testing.T) {
	src := strings.Replace(goodLifecycle, "  - { from: active, to: done,   verb: Finish }\n", `  - from: active
    to: done
    verb: Finish
    when: has(record.assignee_id)
    error: Assign it first.
    lookups:
      open_children:
        count: incident
        where:
          state_category: { eq: open }
          requester_id: { eq: =record.requester_id }
    guards:
      - when: lookups.open_children == 0
        error: Close its open children first.
`, 1)
	c, p := compile(t, map[string]string{"app.yaml": goodManifest, "default/lifecycles/incident.yaml": src})
	if p != nil {
		t.Fatal(p)
	}
	lc := c.Definitions[0].Lifecycle
	who := "u1"
	counter := &countRecorder{n: map[string]int64{"incident": 1}}
	in := expr.Input{Record: &expr.Record{RequesterID: "u1"}, Old: &expr.Record{}, User: &expr.User{}, Count: counter.count}
	if ok, msg, err := lc.Check(context.Background(), "active", "done", in); err != nil || ok || msg != "Assign it first." {
		t.Errorf("the first guard: %v %q %v", ok, msg, err)
	}
	in.Record.AssigneeID = &who
	if ok, msg, err := lc.Check(context.Background(), "active", "done", in); err != nil || ok || msg != "Close its open children first." {
		t.Errorf("the second guard: %v %q %v", ok, msg, err)
	}
	counter.n["incident"] = 0
	if ok, msg, err := lc.Check(context.Background(), "active", "done", in); err != nil || !ok || msg != "" {
		t.Errorf("both pass: %v %q %v", ok, msg, err)
	}
	in.Count = nil
	if ok, _, err := lc.Check(context.Background(), "active", "done", in); err == nil || ok {
		t.Errorf("a lookup that cannot run blocks the move: %v %v", ok, err)
	}

	for name, tc := range map[string]struct{ old, new, want string }{
		"guard without error": {"verb: Finish }", "verb: Finish, guards: [{ when: 'true' }] }", "guards[0]: when) but no error message"},
		"undeclared lookup":   {"verb: Finish }", "verb: Finish, guards: [{ when: 'lookups.x == 0', error: no }] }", "guards[0]: when: reads lookups.x"},
		"lookups, no guard":   {"verb: Finish }", "verb: Finish, lookups: { x: { count: incident, where: { status: { eq: open } } } } }", "has lookups but no guard to read them"},
	} {
		_, p := compile(t, map[string]string{"app.yaml": goodManifest, "default/lifecycles/incident.yaml": strings.Replace(goodLifecycle, tc.old, tc.new, 1)})
		if p == nil || !strings.Contains(p.Error(), tc.want) {
			t.Errorf("%s: want %q in %v", name, tc.want, p)
		}
	}
}
