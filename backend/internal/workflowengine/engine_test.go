package workflowengine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/workflow"
	"github.com/servicelater/servicelater/backend/internal/workflowengine"
)

type world struct {
	pool      *pgxpool.Pool
	reg       *apps.Registry
	svc       *records.Service
	engine    *workflowengine.Engine
	agent     uuid.UUID
	requester uuid.UUID
	ticket    uuid.UUID
}

func setup(t *testing.T) *world {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.InstallFirstParty(t, pool)
	ctx := context.Background()
	w := &world{pool: pool}
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		a, err := q.CreateUser(ctx, dbgen.CreateUserParams{Email: "agent@x.test", Name: "Ada", Role: "agent"})
		if err != nil {
			return err
		}
		r, err := q.CreateUser(ctx, dbgen.CreateUserParams{Email: "lena@x.test", Name: "Lena", Role: "end_user"})
		if err != nil {
			return err
		}
		w.agent, w.requester = a.ID, r.ID
		task, err := q.CreateTask(ctx, dbgen.CreateTaskParams{Type: "incident", Title: "Printer", Priority: 3, RequesterID: r.ID, Status: "new", StateCategory: "open"})
		if err != nil {
			return err
		}
		w.ticket = task.ID
		return q.CreateIncident(ctx, task.ID)
	}); err != nil {
		t.Fatal(err)
	}
	w.reg = apps.NewRegistry(pool)
	if err := w.reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	w.svc = &records.Service{Apps: w.reg, Pool: pool}
	w.engine = workflowengine.New(pool, w.svc)
	return w
}

// install installs (or replaces) an app made of the given workflows and rules, and reloads the registry. The app may
// see, edit and write internal notes on tickets.
func (w *world) install(t *testing.T, name string, workflows, rules map[string]string) {
	t.Helper()
	w.installWith(t, name, "[records:read:*, records:update:*, comments:internal]", workflows, rules)
}

// installWith is install with the app's own manifest permissions.
func (w *world) installWith(t *testing.T, name, perms string, workflows, rules map[string]string) {
	t.Helper()
	files := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: " + name + "\nversion: 1.0.0\nauthor: t\ndescription: \"Test.\"\npermissions: " + perms + "\n")}
	for f, src := range workflows {
		files["default/workflows/"+f+".yaml"] = []byte(src)
	}
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

func wf(name, start, steps string) string {
	return "kind: workflow\nname: " + name + "\nrecord_type: incident\nstart: " + start + "\nsteps:\n" + steps
}

// start creates an execution the way a rule or a person would, as the given user, and returns its id.
func (w *world) start(t *testing.T, app, name string, user uuid.UUID) uuid.UUID {
	t.Helper()
	def := w.reg.Workflow(app, name)
	if def == nil {
		t.Fatalf("no workflow %s/%s", app, name)
	}
	var id uuid.UUID
	if err := db.WithUser(context.Background(), w.pool, user, func(tx pgx.Tx) (err error) {
		id, err = workflow.Start(context.Background(), dbgen.New(tx), workflow.StartParams{Def: def, RecordID: &w.ticket, UserID: user})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (w *world) run(id uuid.UUID) error {
	raw, _ := json.Marshal(workflow.RunPayload{ExecutionID: id})
	return w.engine.Handler()(context.Background(), jobs.Job{ID: 1, Kind: workflow.RunKind, Payload: raw, Attempt: 1, MaxAttempts: 5})
}

func (w *world) execution(t *testing.T, id uuid.UUID) dbgen.WorkflowExecution {
	t.Helper()
	var ex dbgen.WorkflowExecution
	if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) (err error) {
		ex, err = dbgen.New(tx).GetExecution(context.Background(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ex
}

func (w *world) steps(t *testing.T, id uuid.UUID) []dbgen.WorkflowStep {
	t.Helper()
	var out []dbgen.WorkflowStep
	if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) (err error) {
		out, err = dbgen.New(tx).ListSteps(context.Background(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func (w *world) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := w.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (w *world) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type ticketRow struct {
	Title    string
	Priority int16
	Assignee *uuid.UUID
}

func (w *world) row(t *testing.T) (r ticketRow) {
	t.Helper()
	if err := w.pool.QueryRow(context.Background(), `SELECT title, priority, assignee_id FROM tasks WHERE id = $1`, w.ticket).Scan(&r.Title, &r.Priority, &r.Assignee); err != nil {
		t.Fatal(err)
	}
	return r
}

func (w *world) comments(t *testing.T) []string {
	t.Helper()
	rows, err := w.pool.Query(context.Background(), `SELECT body FROM task_comments WHERE task_id = $1 ORDER BY created_at, id`, w.ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func stepIDs(steps []dbgen.WorkflowStep) string {
	var ids []string
	for _, s := range steps {
		ids = append(ids, s.StepID+":"+s.Outcome)
	}
	return strings.Join(ids, " ")
}

const straight = `  tag:
    type: set
    vars:
      level: =record.priority
      note: escalated
    next: bump
  bump:
    type: record.update
    set:
      priority: 1
      title: '="[" + string(vars.level) + "->" + string(record.priority) + "] " + record.title'
      assignee_id: =user.id
    next: say
  say:
    type: comment.add
    body: '=vars.note + " by " + user.name'
    internal: true
    next: done
  done:
    type: end
`

func TestAWorkflowRunsStraightThroughAndSavesAsThePerson(t *testing.T) {
	w := setup(t)
	w.install(t, "house", map[string]string{"esc": wf("esc", "tag", straight)}, nil)
	id := w.start(t, "house", "esc", w.agent)
	if ex := w.execution(t, id); ex.Status != workflow.Queued || ex.CurrentStep != "tag" {
		t.Fatalf("a new execution should be queued at its start: %s %s", ex.Status, ex.CurrentStep)
	}
	if n := w.count(t, `SELECT count(*) FROM jobs WHERE kind = 'workflow.run'`); n != 1 {
		t.Fatalf("starting queued %d jobs, want 1", n)
	}
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	ex := w.execution(t, id)
	if ex.Status != workflow.Done || ex.StepsRun != 4 || ex.FinishedAt.Valid == false {
		t.Errorf("execution: %+v", ex)
	}
	if got := stepIDs(w.steps(t, id)); got != "tag:ok bump:ok say:ok done:ok" {
		t.Errorf("history: %s", got)
	}
	// Each step saw the one before: the title uses the variable set first, and the priority from before and after.
	r := w.row(t)
	if r.Priority != 1 || r.Title != "[3->1] Printer" || r.Assignee == nil || *r.Assignee != w.agent {
		t.Errorf("ticket: %+v", r)
	}
	if cs := w.comments(t); len(cs) != 1 || cs[0] != "escalated by Ada" {
		t.Errorf("comments: %v", cs)
	}
	var vars map[string]any
	_ = json.Unmarshal(ex.Vars, &vars)
	if vars["note"] != "escalated" || vars["level"] != float64(3) {
		t.Errorf("vars: %v", vars)
	}
	// Running it again does nothing: it is over.
	if err := w.run(id); err != nil || w.execution(t, id).StepsRun != 4 {
		t.Errorf("a finished execution moved: %v", err)
	}
}

func TestConditionsChooseTheBranch(t *testing.T) {
	w := setup(t)
	steps := `  decide:
    type: condition
    if: 'record.priority == 1'
    then: crit
    else: calm
  crit:
    type: comment.add
    body: critical
    next: done
  calm:
    type: comment.add
    body: calm
    next: done
  done:
    type: end
`
	w.install(t, "house", map[string]string{"branch": wf("branch", "decide", steps)}, nil)
	for _, c := range []struct {
		prio int
		want string
	}{{3, "calm"}, {1, "critical"}} {
		w.exec(t, `UPDATE tasks SET priority = $1 WHERE id = $2`, c.prio, w.ticket)
		id := w.start(t, "house", "branch", w.agent)
		if err := w.run(id); err != nil {
			t.Fatal(err)
		}
		cs := w.comments(t)
		if got := cs[len(cs)-1]; got != c.want {
			t.Errorf("priority %d went to %q, want %q", c.prio, got, c.want)
		}
	}
}

// A wait is a row with a wake-up time. Nothing runs while it sleeps; the timer queues it when due.
func TestAFixedWaitSleepsAsDataAndTheTimerWakesIt(t *testing.T) {
	w := setup(t)
	w.install(t, "house", map[string]string{"nap": wf("nap", "before", "  before:\n    type: comment.add\n    body: before\n    next: nap\n  nap:\n    type: wait\n    for: 1h\n    next: after\n  after:\n    type: comment.add\n    body: after\n    next: done\n  done:\n    type: end\n")}, nil)
	id := w.start(t, "house", "nap", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	ex := w.execution(t, id)
	if ex.Status != workflow.Waiting || ex.CurrentStep != "nap" || !ex.WakeAt.Valid || time.Until(ex.WakeAt.Time) < 59*time.Minute {
		t.Fatalf("should sleep for an hour: %+v", ex)
	}
	if got := stepIDs(w.steps(t, id)); got != "before:ok nap:waiting" {
		t.Errorf("history: %s", got)
	}
	// Not due: the timer leaves it alone, and a stray job for it does nothing.
	if n, err := w.engine.WakeDue(context.Background()); err != nil || n != 0 {
		t.Fatalf("woke %d, %v", n, err)
	}
	if err := w.run(id); err != nil || w.execution(t, id).Status != workflow.Waiting || len(w.comments(t)) != 1 {
		t.Fatalf("a stray job moved a sleeping execution: %v", err)
	}
	// Due: woken, queued with a job, and it carries on from the wait.
	w.exec(t, `UPDATE workflow_executions SET wake_at = now() - interval '1 second' WHERE id = $1`, id)
	before := w.count(t, `SELECT count(*) FROM jobs WHERE kind = 'workflow.run'`)
	if n, err := w.engine.WakeDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("woke %d, %v", n, err)
	}
	if w.execution(t, id).Status != workflow.Queued || w.count(t, `SELECT count(*) FROM jobs WHERE kind = 'workflow.run'`) != before+1 {
		t.Fatal("waking should queue the execution and a job for it")
	}
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if ex := w.execution(t, id); ex.Status != workflow.Done {
		t.Errorf("status %s", ex.Status)
	}
	if got := stepIDs(w.steps(t, id)); got != "before:ok nap:waiting nap:ok after:ok done:ok" {
		t.Errorf("history: %s", got)
	}
}

const untilFlow = `  wait_ack:
    type: wait
    until: 'record.status != "new"'
    timeout: 1h
    on_timeout: nag
    next: thanks
  nag:
    type: comment.add
    body: nobody answered
    next: done
  thanks:
    type: comment.add
    body: thanks for answering
    next: done
  done:
    type: end
`

// A wait on a condition is re-checked when the record changes, without waiting for its own timer.
func TestAWaitOnAConditionWakesWhenTheRecordChanges(t *testing.T) {
	w := setup(t)
	w.install(t, "house", map[string]string{"ack": wf("ack", "wait_ack", untilFlow)}, nil)
	id := w.start(t, "house", "ack", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	ex := w.execution(t, id)
	if ex.Status != workflow.Waiting || !ex.WaitUntil || !ex.WaitDeadline.Valid {
		t.Fatalf("should wait on the condition: %+v", ex)
	}
	// A change that does not satisfy it wakes the execution, which looks and goes back to sleep.
	w.exec(t, `UPDATE tasks SET priority = 2 WHERE id = $1`, w.ticket)
	if time.Until(w.execution(t, id).WakeAt.Time) > 2*time.Second {
		t.Fatal("a change to the record should bring the wake-up forward")
	}
	if n, _ := w.engine.WakeDue(context.Background()); n != 1 {
		t.Fatalf("woke %d", n)
	}
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if ex := w.execution(t, id); ex.Status != workflow.Waiting {
		t.Fatalf("condition still false, should sleep again: %s", ex.Status)
	}
	// The change that satisfies it: awake at once, and it takes the normal path.
	w.exec(t, `UPDATE tasks SET status = 'in_progress', state_category = 'open' WHERE id = $1`, w.ticket)
	if n, _ := w.engine.WakeDue(context.Background()); n != 1 {
		t.Fatalf("woke %d", n)
	}
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if ex := w.execution(t, id); ex.Status != workflow.Done {
		t.Fatalf("status %s", ex.Status)
	}
	if cs := w.comments(t); len(cs) != 1 || cs[0] != "thanks for answering" {
		t.Errorf("comments: %v", cs)
	}
	if got := stepIDs(w.steps(t, id)); got != "wait_ack:waiting wait_ack:waiting wait_ack:ok thanks:ok done:ok" {
		t.Errorf("history: %s", got)
	}
}

func TestAConditionThatIsAlreadyTrueDoesNotWait(t *testing.T) {
	w := setup(t)
	w.install(t, "house", map[string]string{"ack": wf("ack", "wait_ack", untilFlow)}, nil)
	w.exec(t, `UPDATE tasks SET status = 'in_progress' WHERE id = $1`, w.ticket)
	id := w.start(t, "house", "ack", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if got := stepIDs(w.steps(t, id)); got != "wait_ack:ok thanks:ok done:ok" || w.execution(t, id).Status != workflow.Done {
		t.Errorf("history: %s", got)
	}
}

func TestATimeoutTakesItsBranchOrEndsTheRun(t *testing.T) {
	w := setup(t)
	w.install(t, "house", map[string]string{"ack": wf("ack", "wait_ack", untilFlow),
		"strict": wf("strict", "w", "  w:\n    type: wait\n    until: 'record.status != \"new\"'\n    timeout: 1h\n    next: done\n  done:\n    type: end\n")}, nil)
	for _, name := range []string{"ack", "strict"} {
		id := w.start(t, "house", name, w.agent)
		if err := w.run(id); err != nil {
			t.Fatal(err)
		}
		// The deadline passes with the condition still false.
		w.exec(t, `UPDATE workflow_executions SET wait_deadline = now() - interval '1 second', wake_at = now() WHERE id = $1`, id)
		if n, _ := w.engine.WakeDue(context.Background()); n != 1 {
			t.Fatalf("woke %d", n)
		}
		if err := w.run(id); err != nil {
			t.Fatal(err)
		}
		ex := w.execution(t, id)
		if name == "ack" {
			if ex.Status != workflow.Done || !strings.Contains(stepIDs(w.steps(t, id)), "wait_ack:timeout nag:ok") {
				t.Errorf("on_timeout: %s %s", ex.Status, stepIDs(w.steps(t, id)))
			}
		} else if ex.Status != workflow.Failed || !strings.Contains(ex.Error, "timed out after 1h") {
			t.Errorf("no on_timeout should fail: %s %q", ex.Status, ex.Error)
		}
	}
}

// A step that cannot succeed ends the execution and says where, and the steps before it stay in the history with
// their effects; the failing step's own effects are undone.
func TestAFailingStepEndsTheRunAndKeepsTheHistory(t *testing.T) {
	w := setup(t)
	w.install(t, "guards", nil, map[string]string{"g": "kind: rule\nname: no-priority-one\nrecord_type: incident\ntiming: before\non: [update]\nwhen: 'record.priority == 1'\nrun:\n  - action: abort\n    message: Nobody may set priority 1.\n"})
	w.install(t, "house", map[string]string{"bad": wf("bad", "one", "  one:\n    type: comment.add\n    body: first\n    next: two\n  two:\n    type: record.update\n    set:\n      title: changed\n      priority: 1\n    next: three\n  three:\n    type: end\n"),
		"div": wf("div", "one", "  one:\n    type: comment.add\n    body: first\n    next: two\n  two:\n    type: set\n    vars:\n      x: '=string(record.priority / (record.priority - 3))'\n    next: three\n  three:\n    type: end\n")}, nil)

	id := w.start(t, "house", "bad", w.agent)
	if err := w.run(id); err != nil {
		t.Fatalf("a failed execution is a finished job, not a retry: %v", err)
	}
	ex := w.execution(t, id)
	if ex.Status != workflow.Failed || ex.CurrentStep != "two" || !strings.Contains(ex.Error, "Nobody may set priority 1.") {
		t.Errorf("execution: %s at %s: %q", ex.Status, ex.CurrentStep, ex.Error)
	}
	if got := stepIDs(w.steps(t, id)); got != "one:ok two:failed" {
		t.Errorf("history: %s", got)
	}
	steps := w.steps(t, id)
	if steps[1].Error == "" {
		t.Error("the failed step should carry its error")
	}
	if r := w.row(t); r.Title != "Printer" || r.Priority != 3 {
		t.Errorf("a refused step left changes: %+v", r)
	}
	if cs := w.comments(t); len(cs) != 1 || cs[0] != "first" {
		t.Errorf("the step before kept its effect: %v", cs)
	}

	id = w.start(t, "house", "div", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if ex := w.execution(t, id); ex.Status != workflow.Failed || !strings.Contains(ex.Error, "could not be evaluated") {
		t.Errorf("an expression error: %s %q", ex.Status, ex.Error)
	}
}

// Executions keep the version of the workflow they started on, whatever is installed after.
func TestARunningExecutionKeepsItsVersion(t *testing.T) {
	w := setup(t)
	old := wf("say", "a", "  a:\n    type: wait\n    for: 1h\n    next: b\n  b:\n    type: comment.add\n    body: the old wording\n    next: c\n  c:\n    type: end\n")
	w.install(t, "house", map[string]string{"say": old}, nil)
	id := w.start(t, "house", "say", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	// The app is upgraded while the execution sleeps.
	w.install(t, "house", map[string]string{"say": strings.Replace(old, "the old wording", "the new wording", 1)}, nil)
	w.exec(t, `UPDATE workflow_executions SET wake_at = now() WHERE id = $1`, id)
	_, _ = w.engine.WakeDue(context.Background())
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if cs := w.comments(t); len(cs) != 1 || cs[0] != "the old wording" {
		t.Errorf("comments: %v", cs)
	}
	// A new execution takes the new version.
	id2 := w.start(t, "house", "say", w.agent)
	if err := w.run(id2); err != nil {
		t.Fatal(err)
	}
	w.exec(t, `UPDATE workflow_executions SET wake_at = now() WHERE id = $1`, id2)
	_, _ = w.engine.WakeDue(context.Background())
	if err := w.run(id2); err != nil {
		t.Fatal(err)
	}
	if cs := w.comments(t); len(cs) != 2 || cs[1] != "the new wording" {
		t.Errorf("comments: %v", cs)
	}
}

func TestACancelledExecutionDoesNothingMore(t *testing.T) {
	w := setup(t)
	w.install(t, "house", map[string]string{"nap": wf("nap", "a", "  a:\n    type: wait\n    for: 1h\n    next: b\n  b:\n    type: comment.add\n    body: never\n    next: c\n  c:\n    type: end\n")}, nil)
	id := w.start(t, "house", "nap", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if err := db.WithUser(context.Background(), w.pool, w.agent, func(tx pgx.Tx) error {
		n, err := dbgen.New(tx).CancelExecution(context.Background(), id)
		if n != 1 {
			t.Errorf("cancelled %d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w.exec(t, `UPDATE workflow_executions SET wake_at = now() WHERE id = $1`, id)
	if n, _ := w.engine.WakeDue(context.Background()); n != 0 {
		t.Errorf("the timer woke a cancelled execution")
	}
	if err := w.run(id); err != nil || w.execution(t, id).Status != workflow.Cancelled || len(w.comments(t)) != 0 {
		t.Errorf("a cancelled execution moved: %v", err)
	}
}

// Steps run as the person who started the workflow. A requester cannot edit tickets, so a workflow started by one that
// tries to is stopped by the same rule that stops them by hand.
// Steps run as the workflow's app, with what its manifest declares, for the person who started it. So a requester's
// workflow can edit the ticket and write an internal note when its app may, and the note says whom it was for; an
// app that may not is refused, whoever started it.
func TestStepsRunWithTheAppsPermissions(t *testing.T) {
	w := setup(t)
	steps := map[string]string{
		"up":  wf("up", "a", "  a:\n    type: record.update\n    set: { priority: 1 }\n    next: b\n  b:\n    type: end\n"),
		"int": wf("int", "a", "  a:\n    type: comment.add\n    body: private\n    internal: true\n    next: b\n  b:\n    type: end\n")}
	w.install(t, "house", steps, nil)
	w.installWith(t, "meek", "[records:read:*]", steps, nil)

	for _, app := range []string{"meek", "house"} { // meek first: once house has set the priority, there is nothing to save
		for _, flow := range []string{"up", "int"} {
			id := w.start(t, app, flow, w.requester)
			if err := w.run(id); err != nil {
				t.Fatal(err)
			}
			ex := w.execution(t, id)
			switch {
			case app == "house" && ex.Status != workflow.Done:
				t.Errorf("%s/%s, which may: %s %q", app, flow, ex.Status, ex.Error)
			case app == "meek" && (ex.Status != workflow.Failed || !strings.Contains(ex.Error, "meek app") && !strings.Contains(ex.Error, "records:update:incident")):
				t.Errorf("%s/%s, which may not: %s %q", app, flow, ex.Status, ex.Error)
			}
		}
	}
	if w.row(t).Priority != 1 {
		t.Error("house's workflow did not edit the ticket")
	}
	var author, behalf string
	if err := w.pool.QueryRow(context.Background(), `SELECT a.name, b.name FROM task_comments c JOIN users a ON a.id = c.author_id
		JOIN users b ON b.id = c.on_behalf_of WHERE c.task_id = $1`, w.ticket).Scan(&author, &behalf); err != nil {
		t.Fatal(err)
	}
	if author != "house" || behalf != "Lena" {
		t.Errorf("the note was written by %q for %q", author, behalf)
	}
}

// A long workflow hands its execution back to the queue after its share of steps, so others get a turn.
func TestALongRunYieldsAndCarriesOn(t *testing.T) {
	w := setup(t)
	var b strings.Builder
	const n = 40
	for i := 0; i < n; i++ {
		if i == n-1 {
			b.WriteString("  s39:\n    type: end\n")
			continue
		}
		fmt.Fprintf(&b, "  s%02d:\n    type: set\n    vars: { n: %d }\n    next: s%02d\n", i, i, i+1)
	}
	w.install(t, "house", map[string]string{"long": wf("long", "s00", b.String())}, nil)
	id := w.start(t, "house", "long", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	ex := w.execution(t, id)
	if ex.Status != workflow.Queued || ex.StepsRun != 25 {
		t.Fatalf("should yield after 25 steps: %s, %d", ex.Status, ex.StepsRun)
	}
	if w.count(t, `SELECT count(*) FROM jobs WHERE kind = 'workflow.run'`) != 2 {
		t.Fatal("yielding should queue a new job")
	}
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if ex := w.execution(t, id); ex.Status != workflow.Done || ex.StepsRun != n {
		t.Errorf("after the second run: %s, %d", ex.Status, ex.StepsRun)
	}
}

// A job that runs out of attempts must not leave its execution queued for ever.
func TestAnExecutionWhoseJobGivesUpIsMarkedFailed(t *testing.T) {
	w := setup(t)
	w.install(t, "house", map[string]string{"xx": wf("xx", "a", "  a:\n    type: comment.add\n    body: hi\n    next: b\n  b:\n    type: end\n")}, nil)
	id := w.start(t, "house", "xx", w.agent)
	// Break the database for this step alone: a trigger that fails every comment.
	w.exec(t, `CREATE FUNCTION boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'the database is unwell'; END $$`)
	w.exec(t, `CREATE TRIGGER boom BEFORE INSERT ON task_comments FOR EACH ROW EXECUTE FUNCTION boom()`)
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), `DROP TRIGGER boom ON task_comments`)
		_, _ = w.pool.Exec(context.Background(), `DROP FUNCTION boom()`)
	})
	raw, _ := json.Marshal(workflow.RunPayload{ExecutionID: id})
	h := w.engine.Handler()
	if err := h(context.Background(), jobs.Job{ID: 1, Payload: raw, Attempt: 1, MaxAttempts: 3}); err == nil {
		t.Fatal("a database fault should be returned so the job retries")
	}
	if ex := w.execution(t, id); ex.Status != workflow.Queued {
		t.Fatalf("a retryable fault should leave it queued: %s", ex.Status)
	}
	if err := h(context.Background(), jobs.Job{ID: 1, Payload: raw, Attempt: 3, MaxAttempts: 3}); err == nil {
		t.Fatal("still a fault")
	}
	if ex := w.execution(t, id); ex.Status != workflow.Failed || !strings.Contains(ex.Error, "gave up after 3 attempts") {
		t.Errorf("execution: %s %q", ex.Status, ex.Error)
	}
}

const startRule = `kind: rule
name: escalate-critical
record_type: incident
timing: %s
on: [create]
when: 'record.priority == 1'
run:
  - action: workflow.start
    workflow: esc
`

// A rule starts a workflow in the save that sets it off, so it exists only if the save commits.
func TestARuleStartsAWorkflowInTheSameTransaction(t *testing.T) {
	w := setup(t)
	w.install(t, "house", map[string]string{"esc": wf("esc", "tag", straight)},
		map[string]string{"start": strings.Replace(startRule, "%s", "after", 1),
			"veto": "kind: rule\nname: veto-spam\nrecord_type: incident\ntiming: after\non: [create]\nwhen: 'record.title.contains(\"spam\")'\nrun:\n  - action: abort\n    message: No spam.\n"})
	create := func(title string, prio int16) (records.Ticket, error) {
		var tk records.Ticket
		err := db.WithUser(context.Background(), w.pool, w.agent, func(tx pgx.Tx) (err error) {
			actor, err := records.ActorIn(context.Background(), dbgen.New(tx), w.agent)
			if err != nil {
				return err
			}
			tk, err = w.svc.Create(context.Background(), dbgen.New(tx), actor, ruleruns.Cause{},
				records.CreateInput{Type: "incident", Title: title, Priority: &prio})
			return err
		})
		return tk, err
	}
	if _, err := create("calm", 3); err != nil {
		t.Fatal(err)
	}
	if n := w.count(t, `SELECT count(*) FROM workflow_executions`); n != 0 {
		t.Fatalf("a change the rule's filter rejects started %d workflows", n)
	}
	// An aborted save starts nothing: the execution and its job are rolled back with it.
	if _, err := create("spam", 1); err == nil {
		t.Fatal("the veto rule should abort")
	}
	if n := w.count(t, `SELECT count(*) FROM workflow_executions`) + w.count(t, `SELECT count(*) FROM jobs WHERE kind = 'workflow.run'`); n != 0 {
		t.Fatalf("an aborted save left %d rows behind", n)
	}
	tk, err := create("loud", 1)
	if err != nil {
		t.Fatal(err)
	}
	var ex dbgen.WorkflowExecution
	if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) error {
		rows, err := dbgen.New(tx).ListExecutions(context.Background(), dbgen.ListExecutionsParams{Limit: 10})
		if len(rows) != 1 {
			t.Fatalf("%d executions, %v", len(rows), err)
		}
		ex = rows[0]
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ex.RecordID.UUID != tk.ID || ex.StartedBy != w.agent || ex.Workflow != "esc" || ex.App != "house" || ex.Status != workflow.Queued {
		t.Errorf("execution: %+v", ex)
	}
	var cause ruleruns.Cause
	_ = json.Unmarshal(ex.Cause, &cause)
	if cause.Depth != 1 || len(cause.Chain) != 1 || cause.Chain[0] != "house/esc" {
		t.Errorf("cause: %+v", cause)
	}
}

// What a workflow saves carries its cause, so an app's rules do not answer its own workflow's edits.
func TestAWorkflowsOwnEditsDoNotSetOffItsAppsRules(t *testing.T) {
	w := setup(t)
	async := "kind: rule\nname: on-priority\nrecord_type: incident\ntiming: async\non: [update]\nrun:\n  - action: comment.add\n    body: a rule saw the edit\n"
	w.install(t, "other", nil, map[string]string{"r": strings.Replace(async, "on-priority", "other-rule", 1)})
	w.install(t, "house", map[string]string{"up": wf("up", "a", "  a:\n    type: record.update\n    set: { priority: 2 }\n    next: b\n  b:\n    type: end\n")},
		map[string]string{"r": async})
	id := w.start(t, "house", "up", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	var queued []ruleruns.Payload
	rows, err := w.pool.Query(context.Background(), `SELECT payload FROM jobs WHERE kind = 'rule.async'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		_ = rows.Scan(&raw)
		var p ruleruns.Payload
		_ = json.Unmarshal(raw, &p)
		queued = append(queued, p)
	}
	// The other app's rule may answer; the workflow's own app's may not, and says so in the audit.
	if len(queued) != 1 || queued[0].App != "other" || queued[0].Cause.Depth != 2 || queued[0].Cause.Chain[0] != "house/up" {
		t.Errorf("queued: %+v", queued)
	}
	if n := w.count(t, `SELECT count(*) FROM action_runs WHERE outcome = 'skipped' AND app = 'house' AND rule = 'on-priority' AND detail LIKE 'not run:%'`); n != 1 {
		t.Errorf("%d records of the cut", n)
	}
}

// The timer loop does the waking, with no one calling it.
func TestTheTimerLoopQueuesDueExecutions(t *testing.T) {
	w := setup(t)
	w.install(t, "house", map[string]string{"nap": wf("nap", "a", "  a:\n    type: wait\n    for: 1h\n    next: b\n  b:\n    type: end\n")}, nil)
	id := w.start(t, "house", "nap", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	w.exec(t, `UPDATE workflow_executions SET wake_at = now() WHERE id = $1`, id)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.engine.RunTimer(ctx, 20*time.Millisecond); close(done) }()
	defer func() { cancel(); <-done }()
	for i := 0; i < 100; i++ {
		if w.execution(t, id).Status == workflow.Queued {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the timer never woke the execution")
}

// record.transition moves the record through the one save path, as the app for the person: a move the lifecycle
// has goes through, and a move it does not have fails the run with the reason, leaving the record alone.
func TestATransitionStepMovesTheRecordThroughItsLifecycle(t *testing.T) {
	w := setup(t)
	w.install(t, "house", map[string]string{
		"start":   wf("start", "go", "  go:\n    type: record.transition\n    to: in_progress\n    next: done\n  done:\n    type: end\n"),
		"nowhere": wf("nowhere", "go", "  go:\n    type: record.transition\n    to: archived\n    next: done\n  done:\n    type: end\n"),
	}, nil)
	status := func() (s string) {
		t.Helper()
		if err := w.pool.QueryRow(context.Background(), `SELECT status FROM tasks WHERE id = $1`, w.ticket).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	id := w.start(t, "house", "start", w.requester) // an end user's change can set it off; the app makes the move
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if ex := w.execution(t, id); ex.Status != workflow.Done || status() != "in_progress" {
		t.Fatalf("moved: %s %q, status %s", ex.Status, ex.Error, status())
	}

	id = w.start(t, "house", "nowhere", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if ex := w.execution(t, id); ex.Status != workflow.Failed || !strings.Contains(ex.Error, "the move to archived was refused: unknown status") {
		t.Errorf("refused: %s %q", ex.Status, ex.Error)
	}
	if status() != "in_progress" {
		t.Errorf("a refused move changed the status to %s", status())
	}
}
