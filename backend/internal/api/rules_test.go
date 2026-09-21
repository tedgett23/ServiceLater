package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/ruleworker"
)

// installRules installs an app made only of rules, on top of the first-party apps, and reloads the registry.
func installRules(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry, rules map[string]string) {
	t.Helper()
	files := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: house-rules\nversion: 1.0.0\nauthor: t\ndescription: \"Rules.\"\n")}
	for name, src := range rules {
		files["default/rules/"+name+".yaml"] = []byte(src)
	}
	c, err := (&apps.Bundle{Files: files}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Install(context.Background(), pool, c, apps.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func rule(name, timing, on, body string) string {
	return "kind: rule\nname: " + name + "\nrecord_type: incident\ntiming: " + timing + "\non: [" + on + "]\n" + body
}

type commentList struct {
	Items []comment `json:"items"`
}

func TestBeforeRulesChangeWhatIsSaved(t *testing.T) {
	h, pool, reg := setupApps(t)
	installRules(t, pool, reg, map[string]string{
		"outage": rule("outage-is-critical", "before", "create, update", "when: 'record.title.contains(\"outage\")'\nrun:\n  - action: set\n    field: priority\n    value: 1\n"),
	})
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Big outage","priority":3}`))
	if tk.Priority != 1 {
		t.Errorf("created with priority %d, want 1 from the rule", tk.Priority)
	}
	calm := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Printer jam","priority":3}`))
	if calm.Priority != 3 {
		t.Errorf("a ticket the rule does not match was changed: %d", calm.Priority)
	}
	// An edit that would lower it is put back by the rule; the request's other changes still apply.
	got := decodeBody[ticket](t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"priority":4,"description":"noted"}`))
	if got.Priority != 1 {
		t.Errorf("priority after edit = %d, want 1", got.Priority)
	}
	if again := decodeBody[ticket](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")); again.Priority != 1 {
		t.Errorf("stored priority = %d", again.Priority)
	}
}

func TestBeforeRuleCanAssignAndBeSeenByLaterRules(t *testing.T) {
	h, pool, reg := setupApps(t)
	installRules(t, pool, reg, map[string]string{
		"a-claim": rule("claim", "before", "create", "order: 1\nrun:\n  - action: set\n    field: assignee_id\n    value: =user.id\n"),
		"b-check": rule("must-be-claimed", "before", "create", "order: 2\nwhen: '!has(record.assignee_id)'\nrun:\n  - action: abort\n    message: Nobody owns this.\n"),
	})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"mine"}`))
	if tk.AssigneeID == nil || *tk.AssigneeID != agent.id || tk.AssigneeName == nil {
		t.Errorf("assignee: %+v", tk)
	}
}

func TestBeforeRuleAbortsWithItsOwnMessageAndSavesNothing(t *testing.T) {
	h, pool, reg := setupApps(t)
	installRules(t, pool, reg, map[string]string{
		"crit": rule("critical-needs-assignee", "before", "update", "when: 'record.priority == 1 && !has(record.assignee_id)'\nrun:\n  - action: abort\n    message: Critical incidents need an assignee.\n"),
	})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t","priority":3}`))
	path := "/api/v1/tickets/" + tk.ID

	w := agent.do(h, "PATCH", path, `{"priority":1,"title":"renamed"}`)
	expect(t, w, 422, "critical without an assignee")
	if !strings.Contains(w.Body.String(), "Critical incidents need an assignee.") {
		t.Errorf("the rule's message should reach the person: %s", w.Body)
	}
	if got := decodeBody[ticket](t, agent.do(h, "GET", path, "")); got.Priority != 3 || got.Title != "t" {
		t.Errorf("a blocked save changed the ticket: %+v", got)
	}
	expect(t, agent.do(h, "PATCH", path, `{"priority":1,"assignee_id":"`+agent.id+`"}`), 200, "with an assignee")
}

// A rule sees a move exactly as it would leave the ticket, so it can hold a ticket back from a status.
func TestRulesRunOnTransitions(t *testing.T) {
	h, pool, reg := setupApps(t)
	installRules(t, pool, reg, map[string]string{
		"res": rule("resolution-required", "before", "update", "when: 'record.state_category == \"done\" && record.resolution == \"\"'\nrun:\n  - action: abort\n    message: Add resolution notes before resolving.\n"),
	})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t"}`))
	path := "/api/v1/tickets/" + tk.ID

	w := agent.do(h, "POST", path+"/transitions", `{"to":"resolved"}`)
	expect(t, w, 422, "resolving without notes")
	if !strings.Contains(w.Body.String(), "Add resolution notes") {
		t.Errorf("message: %s", w.Body)
	}
	if got := decodeBody[ticket](t, agent.do(h, "GET", path, "")); got.Status != "new" {
		t.Errorf("status = %s after a blocked move", got.Status)
	}
	expect(t, agent.do(h, "PATCH", path, `{"resolution":"Replaced the toner."}`), 200, "add notes")
	expect(t, agent.do(h, "POST", path+"/transitions", `{"to":"resolved"}`), 200, "resolving with notes")
}

func TestAfterRulesAddCommentsInTheSameSave(t *testing.T) {
	h, pool, reg := setupApps(t)
	installRules(t, pool, reg, map[string]string{
		"ack": rule("acknowledge", "after", "create", "run:\n  - action: comment.add\n    body: =\"Filed as \" + record.reference + \" by \" + user.name\n    internal: true\n"),
	})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t"}`))
	cs := decodeBody[commentList](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/comments", ""))
	if len(cs.Items) != 1 || !cs.Items[0].Internal || !strings.HasPrefix(cs.Items[0].Body, "Filed as INC") {
		t.Errorf("comments: %+v", cs.Items)
	}
}

// After rules run once the ticket is saved, and an abort still takes the whole save with it.
func TestAfterRuleAbortUndoesTheSave(t *testing.T) {
	h, pool, reg := setupApps(t)
	installRules(t, pool, reg, map[string]string{
		"no2": rule("no-priority-two", "after", "update", "when: 'record.priority == 2'\nrun:\n  - action: comment.add\n    body: written first\n  - action: abort\n    message: Priority 2 is not in use.\n"),
	})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t","priority":3}`))
	path := "/api/v1/tickets/" + tk.ID
	expect(t, agent.do(h, "PATCH", path, `{"priority":2}`), 422, "aborted after the write")
	if got := decodeBody[ticket](t, agent.do(h, "GET", path, "")); got.Priority != 3 {
		t.Errorf("priority = %d: the write should have rolled back", got.Priority)
	}
	if cs := decodeBody[commentList](t, agent.do(h, "GET", path+"/comments", "")); len(cs.Items) != 0 {
		t.Errorf("comments from an aborted save were kept: %+v", cs.Items)
	}
}

// An after rule that aborts a create leaves no ticket behind.
func TestAbortedCreateLeavesNothing(t *testing.T) {
	h, pool, reg := setupApps(t)
	installRules(t, pool, reg, map[string]string{
		"nope": rule("no-spam", "after", "create", "when: 'record.title.contains(\"spam\")'\nrun:\n  - action: abort\n    message: No spam.\n"),
	})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"spam spam"}`), 422, "spam")
	if l := decodeBody[list](t, agent.do(h, "GET", "/api/v1/tickets", "")); len(l.Items) != 0 {
		t.Errorf("tickets: %+v", l.Items)
	}
}

func TestRulesApplyToRequestersToo(t *testing.T) {
	h, pool, reg := setupApps(t)
	installRules(t, pool, reg, map[string]string{
		"title": rule("no-shouting", "before", "create", "when: 'record.title == \"HELP\"'\nrun:\n  - action: abort\n    message: Please do not shout.\n"),
		"ack":   rule("thanks", "after", "create", "run:\n  - action: comment.add\n    body: Thanks, we have it.\n"),
	})
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	w := lena.do(h, "POST", "/api/v1/tickets", `{"title":"HELP"}`)
	expect(t, w, 422, "shouting")
	if !strings.Contains(w.Body.String(), "do not shout") {
		t.Errorf("message: %s", w.Body)
	}
	tk := decodeBody[ticket](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Help please"}`))
	cs := decodeBody[commentList](t, lena.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/comments", ""))
	if len(cs.Items) != 1 || cs.Items[0].Body != "Thanks, we have it." || cs.Items[0].AuthorID != lena.id {
		t.Errorf("comments: %+v", cs.Items)
	}
}

// A rule that cannot be evaluated blocks the save and says so plainly, without leaking the rule's insides.
func TestBrokenRuleFailsClosedUnlessSkipped(t *testing.T) {
	h, pool, reg := setupApps(t)
	broken := "when: 'record.priority / (record.priority - 3) == 1'\nrun:\n  - action: abort\n    message: x\n"
	installRules(t, pool, reg, map[string]string{"broken": rule("broken", "before", "create", broken)})
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	w := agent.do(h, "POST", "/api/v1/tickets", `{"title":"t","priority":3}`)
	expect(t, w, 422, "a broken rule")
	if !strings.Contains(w.Body.String(), "A rule could not be checked") || strings.Contains(w.Body.String(), "division") {
		t.Errorf("message: %s", w.Body)
	}
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t","priority":2}`), 201, "the rule only breaks at priority 3")

	installRules(t, pool, reg, map[string]string{"broken": rule("broken", "before", "create", "on_error: skip\n"+broken)})
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t","priority":3}`), 201, "skipped")
}

// What a rule sets goes through the same checks as what a person sends.
func TestRuleChangesAreValidated(t *testing.T) {
	h, pool, reg := setupApps(t)
	installRules(t, pool, reg, map[string]string{
		"bump": rule("bump", "before", "update", "when: 'record.title == \"bump\"'\nrun:\n  - action: set\n    field: priority\n    value: =record.priority + 6\n"),
		"own":  rule("own", "before", "update", "when: 'record.title == \"own\"'\nrun:\n  - action: set\n    field: assignee_id\n    value: =record.requester_id\n"),
	})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")

	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t","priority":3}`))
	path := "/api/v1/tickets/" + tk.ID
	w := agent.do(h, "PATCH", path, `{"title":"bump"}`)
	expect(t, w, 422, "priority out of range")
	if !strings.Contains(w.Body.String(), "out of range") {
		t.Errorf("message: %s", w.Body)
	}
	filed := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t","requester_id":"`+lena.id+`"}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+filed.ID, `{"title":"own"}`), 422, "assignee who is not staff")
	if got := decodeBody[ticket](t, agent.do(h, "GET", path, "")); got.Title != "t" {
		t.Errorf("rejected save changed the title: %q", got.Title)
	}
}

// Until service principals exist (roles slice), rules act as the person who saved. A requester cannot assign a
// ticket, so a rule that tries on their behalf fails the save cleanly instead of half-applying.
func TestRuleCannotDoWhatTheActorMayNot(t *testing.T) {
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	installRules(t, pool, reg, map[string]string{
		"claim": rule("auto-assign", "before", "create", "run:\n  - action: set\n    field: assignee_id\n    value: '"+agent.id+"'\n"),
	})
	w := lena.do(h, "POST", "/api/v1/tickets", `{"title":"t"}`)
	if w.Code < 400 {
		t.Fatalf("a requester's rule assigned a ticket: %d %s", w.Code, w.Body)
	}
	if l := decodeBody[list](t, agent.do(h, "GET", "/api/v1/tickets", "")); len(l.Items) != 0 {
		t.Errorf("a failed save left tickets behind: %+v", l.Items)
	}
}

// startWorker runs a real job worker for the test, on the same registry the API uses.
func startWorker(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := jobs.NewWorker(pool, jobs.Options{Concurrency: 2, PollInterval: 50 * time.Millisecond, Grace: time.Second})
	w.Register(ruleruns.Kind, ruleworker.Handler(pool, reg))
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// An async rule is decided at the save and run after it, in a worker, as the person who saved.
func TestAsyncRulesRunAfterTheSaveInAWorker(t *testing.T) {
	h, pool, reg := setupApps(t)
	installRules(t, pool, reg, map[string]string{
		"esc": rule("escalation-note", "async", "create, update", "when: 'record.priority == 1 && (event == \"create\" || old.priority != 1)'\nrun:\n  - action: comment.add\n    body: '=\"Escalated by \" + user.name + \": \" + record.reference'\n    internal: true\n"),
	})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	startWorker(t, pool, reg)

	calm := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"calm","priority":3}`))
	if n := countRows(t, pool, `SELECT count(*) FROM jobs WHERE kind = 'rule.async'`); n != 0 {
		t.Fatalf("a change the rule's filter rejects queued %d jobs", n)
	}
	loud := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"loud","priority":1}`))
	comments := func(id string) []comment {
		return decodeBody[commentList](t, agent.do(h, "GET", "/api/v1/tickets/"+id+"/comments", "")).Items
	}
	waitFor(t, "the worker's comment", func() bool { return len(comments(loud.ID)) == 1 })
	cs := comments(loud.ID)
	if !cs[0].Internal || cs[0].AuthorID != agent.id || !strings.HasPrefix(cs[0].Body, "Escalated by agent@x.test: INC") {
		t.Errorf("comment: %+v", cs[0])
	}
	// Raising the calm one fires it on update, judged against its old priority.
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+calm.ID, `{"priority":1}`), 200, "raise")
	waitFor(t, "the second comment", func() bool { return len(comments(calm.ID)) == 1 })
	// Editing something else on a critical ticket does not fire it again.
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+calm.ID, `{"description":"more"}`), 200, "edit")
	time.Sleep(300 * time.Millisecond)
	if n := len(comments(calm.ID)); n != 1 {
		t.Errorf("a change that did not touch the priority added comments: %d", n)
	}
	waitFor(t, "the audit", func() bool {
		return countRows(t, pool, `SELECT count(*) FROM action_runs WHERE outcome = 'ok' AND timing = 'async' AND rule = 'escalation-note'`) == 2
	})
}

// The outbox: an async rule's job belongs to the save, so a save that is undone queues nothing.
func TestAnAbortedSaveQueuesNothing(t *testing.T) {
	h, pool, reg := setupApps(t)
	installRules(t, pool, reg, map[string]string{
		"note": rule("note-everything", "async", "create", "run:\n  - action: comment.add\n    body: hello\n"),
		"nope": rule("no-spam", "after", "create", "when: 'record.title.contains(\"spam\")'\nrun:\n  - action: abort\n    message: No spam.\n"),
	})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"spam"}`), 422, "spam")
	if n := countRows(t, pool, `SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("an aborted save left %d jobs behind", n)
	}
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"fine"}`), 201, "fine")
	if n := countRows(t, pool, `SELECT count(*) FROM jobs WHERE kind = 'rule.async'`); n != 1 {
		t.Errorf("a committed save queued %d jobs, want 1", n)
	}
}

// What stopped a save is recorded from a connection of its own, so it survives the save's rollback.
func TestAbortsAndBrokenRulesAreAudited(t *testing.T) {
	h, pool, reg := setupApps(t)
	installRules(t, pool, reg, map[string]string{
		"crit":   rule("critical-needs-assignee", "before", "update", "when: 'record.priority == 1 && !has(record.assignee_id)'\nrun:\n  - action: abort\n    message: Critical incidents need an assignee.\n"),
		"broken": rule("broken-rule", "after", "update", "when: 'record.priority / (record.priority - 4) == 1'\nrun:\n  - action: abort\n    message: x\n"),
		"quiet":  rule("quiet-async", "async", "update", "when: 'record.priority / (record.priority - 2) == 1'\nrun:\n  - action: comment.add\n    body: x\n"),
	})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t","priority":3}`))
	path := "/api/v1/tickets/" + tk.ID

	expect(t, agent.do(h, "PATCH", path, `{"priority":1}`), 422, "aborted")
	expect(t, agent.do(h, "PATCH", path, `{"priority":4}`), 422, "broken after rule")
	// An async rule's filter that cannot be evaluated does not block the save, but it is recorded.
	expect(t, agent.do(h, "PATCH", path, `{"priority":2}`), 200, "async filter fails, the save goes through")

	rows, err := pool.Query(context.Background(), `SELECT rule, timing, outcome, detail, record_id::text FROM action_runs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type line struct{ rule, timing, outcome, detail, record string }
	var got []line
	for rows.Next() {
		var l line
		if err := rows.Scan(&l.rule, &l.timing, &l.outcome, &l.detail, &l.record); err != nil {
			t.Fatal(err)
		}
		got = append(got, l)
	}
	want := []line{
		{"critical-needs-assignee", "before", "aborted", "Critical incidents need an assignee.", tk.ID},
		{"broken-rule", "after", "failed", "", tk.ID},
		{"quiet-async", "async", "failed", "", tk.ID},
	}
	if len(got) != 3 {
		t.Fatalf("audit: %+v", got)
	}
	for i, w := range want {
		g := got[i]
		if g.rule != w.rule || g.timing != w.timing || g.outcome != w.outcome || g.record != w.record || (w.detail != "" && g.detail != w.detail) || (w.detail == "" && g.detail == "") {
			t.Errorf("audit[%d] = %+v, want %+v", i, g, w)
		}
	}
	if got := decodeBody[ticket](t, agent.do(h, "GET", path, "")); got.Priority != 2 {
		t.Errorf("priority %d", got.Priority)
	}
}

// The whole path, with a real worker: a person's save fires a rule whose update would fire it again. The chain is
// cut at the first repeat, so the ticket is changed exactly once and the queue drains instead of running forever.
func TestASelfTriggeringRuleRunsOnceInARealWorker(t *testing.T) {
	h, pool, reg := setupApps(t)
	installRules(t, pool, reg, map[string]string{
		"bang": rule("bang", "async", "create, update", "run:\n  - action: record.update\n    field: title\n    value: '=record.title + \"!\"'\n"),
	})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	startWorker(t, pool, reg)

	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"loop"}`))
	waitFor(t, "the update", func() bool {
		return decodeBody[ticket](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")).Title == "loop!"
	})
	waitFor(t, "the queue to drain", func() bool {
		return countRows(t, pool, `SELECT count(*) FROM jobs WHERE state IN ('pending', 'running')`) == 0
	})
	time.Sleep(300 * time.Millisecond) // a second run, if the chain were not cut, would land by now
	if got := decodeBody[ticket](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")); got.Title != "loop!" {
		t.Errorf("title %q: the rule ran more than once", got.Title)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM action_runs WHERE outcome = 'ok' AND rule = 'bang'`); n != 1 {
		t.Errorf("%d successful runs, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM action_runs WHERE outcome = 'skipped' AND rule = 'bang' AND detail LIKE 'not run:%'`); n != 1 {
		t.Errorf("%d cut-loop records, want 1", n)
	}
}
