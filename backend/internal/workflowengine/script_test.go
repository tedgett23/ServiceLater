package workflowengine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// task files a task under the request, in a status.
func (w *world) task(t *testing.T, parent uuid.UUID, title, status, category string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) error {
		r, err := dbgen.New(tx).CreateTask(context.Background(), dbgen.CreateTaskParams{Type: "work_task", Title: title, Priority: 3,
			RequesterID: w.requester, Status: status, StateCategory: category, ParentID: uuid.NullUUID{UUID: parent, Valid: true}})
		id = r.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

const cancelScript = `
closed = []
for task in children(record, open=True):
    update(task, {"status": "cancelled", "priority": 2}, comment="The request was withdrawn.")
    closed.append(task.reference)
print("cancelled", len(closed))
if not closed:
    return {"next": "nothing"}
return {"vars": {"closed": closed, "count": len(closed)}}
`

func scriptFlow(src string) string {
	var b strings.Builder
	b.WriteString("kind: workflow\nname: withdraw\nrecord_type: request\nstart: cancel\nsteps:\n  cancel:\n    type: script\n    script: |\n")
	for _, line := range strings.Split(strings.Trim(src, "\n"), "\n") {
		b.WriteString("      " + line + "\n")
	}
	b.WriteString("    next: tell\n    branches:\n      nothing: quiet\n" +
		"  tell:\n    type: comment.add\n    body: '=\"Cancelled \" + string(vars.count) + \" tasks\"'\n    next: quiet\n" +
		"  quiet:\n    type: end\n")
	return b.String()
}

// A script step loops over a request's open tasks and cancels each with a note, through the save path (the lifecycle
// and history apply); its vars reach later steps, and it picks a branch when there was nothing to do.
func TestAScriptStepChangesRecordsThroughTheSavePath(t *testing.T) {
	w := setup(t)
	req := w.request(t)
	t1 := w.task(t, req, "Order it", "open", "open")
	t2 := w.task(t, req, "Image it", "in_progress", "open")
	done := w.task(t, req, "Check budget", "done", "done")
	w.installWith(t, "house", taskPerms, map[string]string{"withdraw": scriptFlow(cancelScript)}, nil)

	id := w.start(t, "house", "withdraw", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if ex := w.execution(t, id); ex.Status != "done" {
		t.Fatalf("execution: %s %s", ex.Status, ex.Error)
	}
	for _, tid := range []uuid.UUID{t1, t2} {
		if n := w.count(t, "SELECT count(*) FROM tasks WHERE id = $1 AND status = 'cancelled' AND priority = 2", tid); n != 1 {
			t.Errorf("task %s was not cancelled", tid)
		}
		if n := w.count(t, "SELECT count(*) FROM task_comments WHERE task_id = $1 AND body = 'The request was withdrawn.'", tid); n != 1 {
			t.Errorf("task %s has no comment", tid)
		}
		if n := w.count(t, "SELECT count(*) FROM record_history WHERE subject = 'task' AND subject_id = $1 AND field = 'status'", tid); n < 1 {
			t.Errorf("task %s has no history of the move", tid)
		}
	}
	if n := w.count(t, "SELECT count(*) FROM tasks WHERE id = $1 AND status = 'done'", done); n != 1 {
		t.Error("a finished task was touched")
	}
	if n := w.count(t, "SELECT count(*) FROM task_comments WHERE task_id = $1 AND body = 'Cancelled 2 tasks'", req); n != 1 {
		t.Error("the next step did not see the script's vars")
	}
	steps := w.steps(t, id)
	if !strings.Contains(string(steps[0].Detail), `"printed": ["cancelled 2"]`) {
		t.Errorf("the step's detail: %s", steps[0].Detail)
	}

	// Run again: nothing is open, so the script takes its branch.
	id = w.start(t, "house", "withdraw", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if got := stepIDs(w.steps(t, id)); got != "cancel:ok quiet:ok" {
		t.Errorf("steps: %s", got)
	}
}

// A script whose change is refused fails the step and undoes its other changes; a mistake names its line.
func TestAFailingScriptUndoesItsChanges(t *testing.T) {
	w := setup(t)
	req := w.request(t)
	t1 := w.task(t, req, "Order it", "open", "open")
	w.task(t, req, "Image it", "on_hold", "waiting")
	w.installWith(t, "house", taskPerms, map[string]string{"withdraw": scriptFlow(`
for task in children(record, open=True):
    update(task, {"status": "done", "title": "Closed by script"})
return {"vars": {"count": 0}}
`)}, nil)
	id := w.start(t, "house", "withdraw", w.agent)
	_ = w.run(id)
	ex := w.execution(t, id)
	if ex.Status != "failed" || !strings.Contains(ex.Error, "was refused") {
		t.Fatalf("execution: %s %s", ex.Status, ex.Error)
	}
	if n := w.count(t, "SELECT count(*) FROM tasks WHERE id = $1 AND status = 'open' AND title = 'Order it'", t1); n != 1 {
		t.Error("the first task's change was kept although the step failed")
	}

	w.installWith(t, "house", taskPerms, map[string]string{"withdraw": scriptFlow("x = 1\ny = x + 'a'\nreturn {\"vars\": {\"count\": 0}}")}, nil)
	id = w.start(t, "house", "withdraw", w.agent)
	_ = w.run(id)
	if ex := w.execution(t, id); ex.Status != "failed" || !strings.Contains(ex.Error, "line 2") {
		t.Fatalf("execution: %s %s", ex.Status, ex.Error)
	}
}
