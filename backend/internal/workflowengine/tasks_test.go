package workflowengine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/workflow"
)

// request files a request for the world's requester and makes it the record its workflows run on.
func (w *world) request(t *testing.T) uuid.UUID {
	t.Helper()
	if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		r, err := q.CreateTask(context.Background(), dbgen.CreateTaskParams{Type: "request", Title: "New laptop", Priority: 3,
			RequesterID: w.requester, Status: "in_progress", StateCategory: "open"})
		w.ticket = r.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return w.ticket
}

// move moves a record as the agent, through the one save path.
func (w *world) move(t *testing.T, id uuid.UUID, to string) {
	t.Helper()
	if err := db.WithUser(context.Background(), w.pool, w.agent, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		a, err := records.ActorIn(context.Background(), q, w.agent)
		if err != nil {
			return err
		}
		_, err = w.svc.Transition(context.Background(), q, a, ruleruns.Cause{}, id, to)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// wake runs what is due: an execution a change has woken.
func (w *world) wake(t *testing.T, id uuid.UUID) {
	t.Helper()
	if until := time.Until(w.execution(t, id).WakeAt.Time); until > 2*time.Second {
		t.Fatalf("the execution is not woken (due in %s)", until)
	}
	if _, err := w.engine.WakeDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
}

// taskPerms is what an app filing tasks under requests needs.
const taskPerms = "[records:read:*, records:update:*, records:create:work_task, comments:internal]"

// A request's workflow files tasks for teams under it: one it waits for, then two at once, collected by waiting until
// none is left open. Each task's change wakes the workflow; a cancelled task is told apart from a done one.
func TestTasksAreFiledUnderARequestAndWaitedFor(t *testing.T) {
	w := setup(t)
	w.group(t, "Procurement")
	w.group(t, "Desktop Support")
	req := w.request(t)
	w.installWith(t, "house", taskPerms, map[string]string{"fulfil": "kind: workflow\nname: fulfil\nrecord_type: request\nstart: order\nsteps:\n" +
		"  order:\n    type: task.create\n    title: '=\"Order a laptop for \" + record.requester_name'\n    group: Procurement\n    next: image\n" +
		"  image:\n    type: task.create\n    title: Image the laptop\n    group: '=\"Desktop \" + \"Support\"'\n    wait: false\n    next: hand\n" +
		"  hand:\n    type: task.create\n    title: Hand it over\n    description: Book a time with them.\n    wait: false\n    next: gather\n" +
		"  gather:\n    type: wait\n    until: record.open_tasks == 0\n    timeout: 7d\n    next: say\n" +
		"  say:\n    type: comment.add\n    body: '=\"ordered: \" + vars.order_outcome + \", \" + vars.order_task'\n    next: done\n" +
		"  done:\n    type: end\n"}, nil)
	id := w.start(t, "house", "fulfil", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	ex := w.execution(t, id)
	v := vars(t, ex)
	if ex.Status != workflow.Waiting || ex.CurrentStep != "order" || v["order_task"] != "TASK1" {
		t.Fatalf("should file TASK1 and wait for it: %+v %v", ex, v)
	}
	type child struct {
		ref, title, group, status string
		requester                 uuid.UUID
	}
	children := func() []child {
		rows, err := w.pool.Query(context.Background(), `SELECT 'TASK' || t.number, t.title, coalesce(g.name, ''), t.status, t.requester_id
			FROM tasks t LEFT JOIN groups g ON g.id = t.assignment_group_id WHERE t.parent_id = $1 ORDER BY t.number`, req)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []child
		for rows.Next() {
			var c child
			if err := rows.Scan(&c.ref, &c.title, &c.group, &c.status, &c.requester); err != nil {
				t.Fatal(err)
			}
			out = append(out, c)
		}
		return out
	}
	if c := children(); len(c) != 1 || c[0].title != "Order a laptop for Lena" || c[0].group != "Procurement" || c[0].requester != w.requester {
		t.Fatalf("the first task: %+v", c)
	}
	// A stray run while it waits files nothing more.
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if n := len(children()); n != 1 {
		t.Fatalf("a second visit filed another task: %d", n)
	}

	var first uuid.UUID
	_ = w.pool.QueryRow(context.Background(), `SELECT id FROM tasks WHERE parent_id = $1`, req).Scan(&first)
	w.move(t, first, "done")
	w.wake(t, id)
	ex = w.execution(t, id)
	if ex.Status != workflow.Waiting || ex.CurrentStep != "gather" {
		t.Fatalf("should file the other two and wait for them: %+v", ex)
	}
	c := children()
	if len(c) != 3 || c[1].group != "Desktop Support" || c[2].group != "" {
		t.Fatalf("tasks: %+v", c)
	}

	// The requester sees the tasks under their request, as they do their own records.
	if err := db.WithUser(context.Background(), w.pool, w.requester, func(tx pgx.Tx) error {
		rows, err := dbgen.New(tx).ListChildTasks(context.Background(), uuid.NullUUID{UUID: req, Valid: true})
		if len(rows) != 3 {
			t.Errorf("the requester sees %d tasks", len(rows))
		}
		r, err2 := records.Load(context.Background(), dbgen.New(tx), req)
		if err2 == nil && r.OpenTasks != 2 {
			t.Errorf("open tasks: %d", r.OpenTasks)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var ids []uuid.UUID
	rows, _ := w.pool.Query(context.Background(), `SELECT id FROM tasks WHERE parent_id = $1 AND status = 'open' ORDER BY number`, req)
	for rows.Next() {
		var x uuid.UUID
		_ = rows.Scan(&x)
		ids = append(ids, x)
	}
	rows.Close()
	w.move(t, ids[0], "cancelled")
	w.wake(t, id)
	if ex := w.execution(t, id); ex.Status != workflow.Waiting || ex.CurrentStep != "gather" {
		t.Fatalf("one task is still open: %+v", ex)
	}
	w.move(t, ids[1], "done")
	w.wake(t, id)
	if ex := w.execution(t, id); ex.Status != workflow.Done {
		t.Fatalf("all tasks are finished: %+v %s", ex, stepIDs(w.steps(t, id)))
	}
	if got := strings.Join(w.comments(t), "|"); got != "ordered: done, TASK1" {
		t.Errorf("comments: %q", got)
	}
}

// A task the workflow waits for that is cancelled is reported as such, and a task for a team that does not exist is
// filed without one, saying so, rather than stopping the request.
func TestATaskStepSaysWhatHappened(t *testing.T) {
	w := setup(t)
	w.request(t)
	w.installWith(t, "house", taskPerms, map[string]string{
		"one": "kind: workflow\nname: one\nrecord_type: request\nstart: ask\nsteps:\n" +
			"  ask:\n    type: task.create\n    title: Check stock\n    next: say\n" +
			"  say:\n    type: comment.add\n    body: '=string(vars.ask_outcome)'\n    next: done\n  done:\n    type: end\n",
		"bad": "kind: workflow\nname: bad\nrecord_type: request\nstart: ask\nsteps:\n" +
			"  ask:\n    type: task.create\n    title: Check stock\n    group: Nobody\n    next: done\n  done:\n    type: end\n",
	}, nil)
	id := w.start(t, "house", "one", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	var task uuid.UUID
	_ = w.pool.QueryRow(context.Background(), `SELECT id FROM tasks WHERE parent_id = $1`, w.ticket).Scan(&task)
	w.move(t, task, "cancelled")
	w.wake(t, id)
	if got := w.comments(t); len(got) != 1 || got[0] != "cancelled" {
		t.Errorf("comments: %v", got)
	}

	bad := w.start(t, "house", "bad", w.agent)
	if err := w.run(bad); err != nil {
		t.Fatal(err)
	}
	if ex := w.execution(t, bad); ex.Status != workflow.Waiting {
		t.Errorf("a missing group: %+v", ex)
	}
	if st := w.steps(t, bad); len(st) != 1 || !strings.Contains(string(st[0].Detail), `no group named \"Nobody\"`) {
		t.Errorf("the step does not say why the task has no team: %s", st[0].Detail)
	}
	if n := w.count(t, `SELECT count(*) FROM tasks WHERE type = 'work_task' AND assignment_group_id IS NULL`); n != 2 {
		t.Errorf("%d unrouted tasks", n)
	}
}

// A workflow acts as its app, which restricted groups do not hold back: it files a task into HR, a restricted group
// its app is not in, keeps track of it, and carries on when HR finishes it.
func TestATaskStepFilesIntoARestrictedGroup(t *testing.T) {
	w := setup(t)
	w.group(t, "HR", w.agent) // the agent who later finishes the task works in HR; the workflow's app is in no group
	if _, err := w.pool.Exec(context.Background(), `UPDATE groups SET restricted = true WHERE name = 'HR'`); err != nil {
		t.Fatal(err)
	}
	req := w.request(t)
	w.installWith(t, "house", taskPerms, map[string]string{"hire": "kind: workflow\nname: hire\nrecord_type: request\nstart: contract\nsteps:\n" +
		"  contract:\n    type: task.create\n    title: Prepare the contract\n    group: HR\n    next: say\n" +
		"  say:\n    type: comment.add\n    body: '=\"contract: \" + vars.contract_outcome'\n    next: done\n" +
		"  done:\n    type: end\n"}, nil)
	id := w.start(t, "house", "hire", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if ex := w.execution(t, id); ex.Status != workflow.Waiting || vars(t, ex)["contract_task"] != "TASK1" {
		t.Fatalf("should file TASK1 into HR and wait: %+v", ex)
	}
	var task uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `SELECT id FROM tasks WHERE parent_id = $1`, req).Scan(&task); err != nil {
		t.Fatal(err)
	}
	w.move(t, task, "done")
	w.wake(t, id)
	if ex := w.execution(t, id); ex.Status != workflow.Done {
		t.Fatalf("should finish once HR is done: %+v", ex)
	}
}
