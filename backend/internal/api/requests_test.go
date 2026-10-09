package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/db"
)

type requestBody struct {
	ID              string  `json:"id"`
	Reference       string  `json:"reference"`
	Status          string  `json:"status"`
	ParentID        *string `json:"parent_id"`
	ParentReference *string `json:"parent_reference"`
	OpenTasks       int     `json:"open_tasks"`
	Tasks           []struct {
		Reference           string  `json:"reference"`
		Title               string  `json:"title"`
		StateCategory       string  `json:"state_category"`
		AssignmentGroupName *string `json:"assignment_group_name"`
	} `json:"tasks"`
}

// A request is fulfilled by tasks filed under it: its page lists them, each says what it is part of, its requester
// follows them, and it cannot be fulfilled while one is still open.
func TestTasksUnderARequest(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	omar := newActor(t, h, pool, "omar@x.test", "end_user")
	g := decodeBody[struct {
		ID string `json:"id"`
	}](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Procurement"}`))

	req := decodeBody[requestBody](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"request","title":"New laptop","requester_id":"`+lena.id+`"}`))
	if req.Reference != "REQ1" || req.Status != "submitted" {
		t.Fatalf("request: %+v", req)
	}
	expect(t, agent.do(h, "POST", "/api/v1/tickets/"+req.ID+"/transitions", `{"to":"in_progress"}`), 200, "starting work")
	w := agent.do(h, "POST", "/api/v1/tickets", `{"type":"work_task","title":"Order it","parent_id":"`+req.ID+`","requester_id":"`+lena.id+
		`","assignment_group_id":"`+g.ID+`"}`)
	expect(t, w, 201, "filing a task under it")
	task := decodeBody[requestBody](t, w)
	if task.Reference != "TASK1" || task.ParentID == nil || *task.ParentID != req.ID {
		t.Fatalf("task: %+v", task)
	}
	refusal(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"work_task","title":"Deeper","parent_id":"`+task.ID+`"}`), 422,
		"cannot have records of its own", "a task under a task")
	refusal(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"work_task","title":"Nowhere","parent_id":"`+uuid.NewString()+`"}`), 422,
		"no record to put it under", "a parent that does not exist")

	got := decodeBody[requestBody](t, agent.do(h, "GET", "/api/v1/tickets/"+req.ID, ""))
	if len(got.Tasks) != 1 || got.Tasks[0].Reference != "TASK1" || *got.Tasks[0].AssignmentGroupName != "Procurement" || got.OpenTasks != 1 {
		t.Fatalf("the request's tasks: %+v", got)
	}
	if got := decodeBody[requestBody](t, agent.do(h, "GET", "/api/v1/tickets/TASK1", "")); got.ParentReference == nil || *got.ParentReference != "REQ1" {
		t.Errorf("the task's parent: %+v", got)
	}
	// Lena follows her request's tasks; someone else sees neither.
	if got := decodeBody[requestBody](t, lena.do(h, "GET", "/api/v1/tickets/REQ1", "")); len(got.Tasks) != 1 {
		t.Errorf("the requester sees %d tasks", len(got.Tasks))
	}
	expect(t, lena.do(h, "GET", "/api/v1/tickets/TASK1", ""), 200, "the requester reading her task")
	expect(t, omar.do(h, "GET", "/api/v1/tickets/TASK1", ""), 404, "someone else reading it")
	expect(t, lena.do(h, "POST", "/api/v1/tickets/TASK1/transitions", `{"to":"done"}`), 403, "the requester finishing a task")

	refusal(t, agent.do(h, "POST", "/api/v1/tickets/"+req.ID+"/transitions", `{"to":"fulfilled"}`), 422,
		"Its tasks are not all finished yet", "fulfilling with a task open")
	expect(t, agent.do(h, "POST", "/api/v1/tickets/TASK1/transitions", `{"to":"done"}`), 200, "finishing the task")
	expect(t, agent.do(h, "POST", "/api/v1/tickets/"+req.ID+"/transitions", `{"to":"fulfilled"}`), 200, "fulfilling once it is done")
	// Approval is the workflow's: nobody moves a request into or out of it by hand.
	r2 := decodeBody[requestBody](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"request","title":"Monitor"}`))
	expect(t, admin.do(h, "POST", "/api/v1/tickets/"+r2.ID+"/transitions", `{"to":"approval"}`), 409, "asking for approval by hand")
}

// Each person may have a manager, set by those who manage people and nobody else: otherwise anyone could choose who
// approves their own requests.
func TestOnlyPeopleManagersSetAManager(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	mia := newActor(t, h, pool, "mia@x.test", "end_user")

	w := admin.do(h, "PATCH", "/api/v1/users/"+lena.id, `{"manager_id":"`+mia.id+`"}`)
	expect(t, w, 200, "an admin setting a manager")
	if !strings.Contains(w.Body.String(), `"manager_id":"`+mia.id+`"`) {
		t.Errorf("not saved: %s", w.Body)
	}
	if got := decodeBody[struct {
		ManagerName *string `json:"manager_name"`
	}](t, admin.do(h, "GET", "/api/v1/users/"+lena.id, "")); got.ManagerName == nil || *got.ManagerName != "mia@x.test" {
		t.Errorf("manager name: %+v", got)
	}
	refusal(t, admin.do(h, "PATCH", "/api/v1/users/"+lena.id, `{"manager_id":"`+lena.id+`"}`), 422, "nobody is their own manager", "oneself")
	refusal(t, admin.do(h, "PATCH", "/api/v1/users/"+lena.id, `{"manager_id":"`+uuid.NewString()+`"}`), 422, "no person with that id", "nobody")
	expect(t, agent.do(h, "PATCH", "/api/v1/users/"+lena.id, `{"manager_id":null}`), 403, "an agent")
	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+lena.id, `{"name":"Lena F"}`), 200, "another change keeps the manager")
	if !strings.Contains(admin.do(h, "GET", "/api/v1/users/"+lena.id, "").Body.String(), mia.id) {
		t.Error("an unrelated change cleared the manager")
	}

	// Lena may edit her own row (her theme, say), but not choose her manager, even straight in the database.
	err := db.WithUser(context.Background(), pool, uuid.MustParse(lena.id), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE users SET manager_id = NULL WHERE id = $1`, lena.id)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "only those who manage people") {
		t.Errorf("a person clearing their own manager: %v", err)
	}
	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+lena.id, `{"manager_id":null}`), 200, "removing the manager")
}
