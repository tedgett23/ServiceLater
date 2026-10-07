package ruleworker_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/ruleworker"
)

// installApp installs (or replaces) an app made only of rules, keyed by file name, and reloads the registry.
func (w *world) installApp(t *testing.T, name string, rules map[string]string) {
	t.Helper()
	w.installAppWith(t, name, "[tickets:read_all, tickets:update, comments:internal]", rules)
}

// installAppWith is installApp with the app's own manifest permissions.
func (w *world) installAppWith(t *testing.T, name, perms string, rules map[string]string) {
	t.Helper()
	files := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: " + name + "\nversion: 1.0.0\nauthor: t\ndescription: \"Rules.\"\npermissions: " + perms + "\n")}
	for f, src := range rules {
		files["default/rules/"+f+".yaml"] = []byte(src)
	}
	c, err := (&apps.Bundle{Files: files}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Install(context.Background(), w.pool, c, apps.InstallOptions{ApprovePermissions: true}); err != nil {
		t.Fatal(err)
	}
	if err := w.reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func asyncRule(name, on, body string) string {
	return "kind: rule\nname: " + name + "\nrecord_type: incident\ntiming: async\non: [" + on + "]\n" + body
}

func (w *world) jobFor(app, rule string, user uuid.UUID, cause ruleruns.Cause) jobs.Job {
	raw, _ := json.Marshal(ruleruns.Payload{App: app, Rule: rule, RecordID: w.ticket, Event: "update", UserID: user, Cause: cause})
	return jobs.Job{ID: 9, Kind: ruleruns.Kind, Payload: raw, Attempt: 1, MaxAttempts: 5}
}

func (w *world) runJob(j jobs.Job) error {
	return ruleworker.Handler(w.pool, w.reg)(context.Background(), j)
}

type ticketRow struct {
	Title    string
	Priority int16
	Assignee *uuid.UUID
}

func (w *world) row(t *testing.T) ticketRow {
	t.Helper()
	var r ticketRow
	if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT title, priority, assignee_id FROM tasks WHERE id = $1`, w.ticket).Scan(&r.Title, &r.Priority, &r.Assignee)
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (w *world) queued(t *testing.T) []ruleruns.Payload {
	t.Helper()
	var out []ruleruns.Payload
	if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT payload FROM jobs WHERE kind = 'rule.async' ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var p ruleruns.Payload
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecordUpdateIsSavedAsOneEditBySomeoneWhoMay(t *testing.T) {
	w := setup(t, "")
	w.installApp(t, "house-rules", map[string]string{"fix": asyncRule("triage", "update",
		"run:\n  - action: record.update\n    field: priority\n    value: 1\n  - action: record.update\n    field: title\n    value: '=\"[\" + string(record.priority) + \"] \" + record.title'\n    when: 'record.title == \"Printer\"'\n"+
			"  - action: record.update\n    field: assignee_id\n    value: =user.id\n")})
	if err := w.runJob(w.jobFor("house-rules", "triage", w.agent, ruleruns.Cause{})); err != nil {
		t.Fatal(err)
	}
	// One save, whose actions each saw the earlier ones' changes (the title is judged on the priority set before it).
	got := w.row(t)
	if got.Priority != 1 || got.Title != "[1] Printer" || got.Assignee == nil || *got.Assignee != w.agent {
		t.Errorf("ticket: %+v", got)
	}
	if a := w.audit(t); len(a) != 1 || a[0].Outcome != ruleruns.OK {
		t.Errorf("audit: %+v", a)
	}
}

// The update is an ordinary save, so a before rule of any app can refuse it, and the run leaves nothing behind.
func TestBeforeRulesJudgeARulesUpdate(t *testing.T) {
	w := setup(t, "")
	w.installApp(t, "guards", map[string]string{"g": "kind: rule\nname: no-priority-one\nrecord_type: incident\ntiming: before\non: [update]\nwhen: 'record.priority == 1'\nrun:\n  - action: abort\n    message: Nobody may set priority 1.\n"})
	w.installApp(t, "house-rules", map[string]string{"esc": asyncRule("escalate", "update",
		"run:\n  - action: comment.add\n    body: escalating\n  - action: record.update\n    field: priority\n    value: 1\n")})
	err := w.runJob(w.jobFor("house-rules", "escalate", w.agent, ruleruns.Cause{}))
	if !jobs.IsPermanent(err) {
		t.Fatalf("a refused change is not worth retrying: %v", err)
	}
	if w.row(t).Priority != 3 || len(w.comments(t)) != 0 {
		t.Errorf("a refused run left changes: %+v, %d comments", w.row(t), len(w.comments(t)))
	}
	a := w.audit(t)
	if len(a) != 2 { // the guard's own line (aborted) and the run's (failed)
		t.Fatalf("audit: %+v", a)
	}
	var sawRun, sawAbort bool
	for _, l := range a {
		sawRun = sawRun || l.Timing == "async" && l.Outcome == ruleruns.Failed && strings.Contains(l.Detail, "Nobody may set priority 1.")
		sawAbort = sawAbort || l.Timing == "before" && l.Outcome == ruleruns.Aborted && l.Rule == "no-priority-one"
	}
	if !sawRun || !sawAbort {
		t.Errorf("audit: %+v", a)
	}
}

// Only staff can edit a ticket. A requester's change that sets off an update is refused the way their own edit would be.
// A requester's change can set off an update when the app may edit tickets (this used to be impossible: rules ran as
// the requester). An app that may not edit tickets is refused, whoever set it off.
func TestARequestersChangeCanSetOffAnAppsUpdate(t *testing.T) {
	w := setup(t, "")
	bump := map[string]string{"up": asyncRule("bump", "update", "run:\n  - action: record.update\n    field: priority\n    value: 1\n")}
	w.installAppWith(t, "meek", "[tickets:read_all]", bump)
	err := w.runJob(w.jobFor("meek", "bump", w.agent, ruleruns.Cause{}))
	if !jobs.IsPermanent(err) || w.row(t).Priority != 3 {
		t.Fatalf("an app that may not edit tickets, set off by an agent: %v", err)
	}
	if a := w.audit(t); len(a) < 1 || a[0].Outcome != ruleruns.Failed || !strings.Contains(a[0].Detail, "tickets:update") {
		t.Errorf("audit: %+v", a)
	}

	w.installApp(t, "house-rules", bump)
	if err := w.runJob(w.jobFor("house-rules", "bump", w.requester, ruleruns.Cause{})); err != nil || w.row(t).Priority != 1 {
		t.Fatalf("err %v, row %+v", err, w.row(t))
	}
}

// A rule that fires on every update, and updates, is the simplest loop. Its own save must not queue it again.
func TestARuleCannotTriggerItselfThroughItsOwnUpdate(t *testing.T) {
	w := setup(t, "")
	w.installApp(t, "house-rules", map[string]string{"loop": asyncRule("bang", "update",
		"run:\n  - action: record.update\n    field: title\n    value: '=record.title + \"!\"'\n")})
	if err := w.runJob(w.jobFor("house-rules", "bang", w.agent, ruleruns.Cause{Depth: 1, Chain: []string{"house-rules/bang"}})); err != nil {
		t.Fatal(err)
	}
	if got := w.row(t).Title; got != "Printer!" {
		t.Errorf("title %q", got)
	}
	if q := w.queued(t); len(q) != 0 {
		t.Fatalf("the rule queued itself again: %+v", q)
	}
	var skipped bool
	for _, l := range w.audit(t) {
		skipped = skipped || l.Outcome == ruleruns.Skipped && l.Rule == "bang" && strings.Contains(l.Detail, "house-rules/bang")
	}
	if !skipped {
		t.Errorf("the cut loop left no trace: %+v", w.audit(t))
	}
}

// Rules of different apps may set each other off, so apps can cooperate; each link carries the chain so far.
func TestAChainAcrossAppsCarriesItsCause(t *testing.T) {
	w := setup(t, "")
	w.installApp(t, "first", map[string]string{"a": asyncRule("one", "update", "run:\n  - action: record.update\n    field: priority\n    value: 2\n")})
	w.installApp(t, "second", map[string]string{"b": asyncRule("two", "update", "when: 'record.priority == 2'\nrun:\n  - action: comment.add\n    body: second saw it\n")})
	if err := w.runJob(w.jobFor("first", "one", w.agent, ruleruns.Cause{Depth: 1, Chain: []string{"first/one"}})); err != nil {
		t.Fatal(err)
	}
	q := w.queued(t)
	if len(q) != 1 || q[0].App != "second" || q[0].Cause.Depth != 2 || len(q[0].Cause.Chain) != 2 || q[0].Cause.Chain[0] != "first/one" || q[0].Cause.Chain[1] != "second/two" {
		t.Fatalf("queued: %+v", q)
	}
	// And the second link runs, as the same person, without loop trouble.
	if err := w.runJob(w.jobFor("second", "two", w.agent, q[0].Cause)); err != nil {
		t.Fatal(err)
	}
	if cs := w.comments(t); len(cs) != 1 || cs[0].TaskComment.Body != "second saw it" {
		t.Errorf("comments: %+v", cs)
	}
}

// However many apps a chain crosses, it stops at MaxDepth.
func TestAChainIsCutAtMaxDepth(t *testing.T) {
	w := setup(t, "")
	w.installApp(t, "first", map[string]string{"a": asyncRule("one", "update", "run:\n  - action: record.update\n    field: priority\n    value: 2\n")})
	w.installApp(t, "second", map[string]string{"b": asyncRule("two", "update", "run:\n  - action: comment.add\n    body: never\n")})
	if err := w.runJob(w.jobFor("first", "one", w.agent, ruleruns.Cause{Depth: ruleruns.MaxDepth, Chain: []string{"x/y"}})); err != nil {
		t.Fatal(err)
	}
	if q := w.queued(t); len(q) != 0 {
		t.Fatalf("queued past the limit: %+v", q)
	}
	var skipped bool
	for _, l := range w.audit(t) {
		skipped = skipped || l.Outcome == ruleruns.Skipped && l.Rule == "two" && strings.Contains(l.Detail, "deep")
	}
	if !skipped {
		t.Errorf("audit: %+v", w.audit(t))
	}
}
