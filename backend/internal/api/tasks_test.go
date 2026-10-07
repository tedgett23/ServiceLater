package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

type typed struct {
	withMoves
	Type       string `json:"type"`
	Reference  string `json:"reference"`
	Resolution string `json:"resolution"`
}

func TestTicketsHaveATypeAndAReference(t *testing.T) {
	h, pool := setup(t)
	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[typed](t, alice.do(h, "POST", "/api/v1/tickets", `{"title":"printer"}`))
	if tk.Type != "incident" || tk.Reference != fmt.Sprintf("INC%d", tk.Number) {
		t.Fatalf("type %q reference %q number %d", tk.Type, tk.Reference, tk.Number)
	}

	// Every way of naming it reaches the same ticket: id, reference (any case), and a bare number.
	for _, name := range []string{tk.ID, tk.Reference, strings.ToLower(tk.Reference), fmt.Sprint(tk.Number)} {
		got := decodeBody[typed](t, alice.do(h, "GET", "/api/v1/tickets/"+name, ""))
		if got.ID != tk.ID {
			t.Errorf("GET %s reached %s", name, got.ID)
		}
	}
	// A reference names a type: the same number under another type is a different (here: missing) ticket.
	expect(t, alice.do(h, "GET", fmt.Sprintf("/api/v1/tickets/CHG%d", tk.Number), ""), 404, "wrong type prefix")
	expect(t, alice.do(h, "GET", "/api/v1/tickets/INC0", ""), 404, "number zero")
	expect(t, alice.do(h, "GET", "/api/v1/tickets/ZZZ1", ""), 404, "unknown prefix")

	// Lists carry them too, and can be limited to a type.
	list := decodeBody[struct {
		Items []typed `json:"items"`
	}](t, agent.do(h, "GET", "/api/v1/tickets?type=incident", ""))
	if len(list.Items) != 1 || list.Items[0].Reference != tk.Reference {
		t.Errorf("list: %+v", list.Items)
	}
	expect(t, agent.do(h, "GET", "/api/v1/tickets?type=change", ""), 200, "a real type with no tickets")
	expect(t, agent.do(h, "GET", "/api/v1/tickets?type=widget", ""), 400, "unknown type")
	expect(t, agent.do(h, "GET", "/api/v1/tickets?type=task", ""), 400, "task is the base, not a type of ticket")
}

func TestResolutionLivesInTheIncidentRow(t *testing.T) {
	h, pool := setup(t)
	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[typed](t, alice.do(h, "POST", "/api/v1/tickets", `{"title":"vpn"}`))
	path := "/api/v1/tickets/" + tk.ID

	if tk.Resolution != "" {
		t.Errorf("a new incident has no resolution, got %q", tk.Resolution)
	}
	w := agent.do(h, "PATCH", path, `{"resolution":"Replaced the gateway certificate."}`)
	expect(t, w, 200, "staff write the resolution")
	if got := decodeBody[typed](t, w); got.Resolution != "Replaced the gateway certificate." {
		t.Errorf("response: %q", got.Resolution)
	}
	// The requester reads it but cannot write it.
	if got := decodeBody[typed](t, alice.do(h, "GET", path, "")); got.Resolution != "Replaced the gateway certificate." {
		t.Errorf("the requester should see the resolution: %q", got.Resolution)
	}
	expect(t, alice.do(h, "PATCH", path, `{"resolution":"nope"}`), 403, "a requester writing it")
	expect(t, agent.do(h, "PATCH", path, fmt.Sprintf(`{"resolution":%q}`, strings.Repeat("x", 10001))), 422, "too long")

}

// The point of the flat record: a guard can use a field that lives in the incident's own table, and an edit to that
// field changes which moves are available.
func TestAGuardCanUseAnIncidentColumn(t *testing.T) {
	h, pool, reg := setupApps(t)
	installApp(t, pool, reg, strings.Replace(guardedLifecycle,
		`when: 'has(record.assignee_id)', error: Assign the incident before starting work.`,
		`when: 'size(record.resolution) > 0', error: Write the resolution notes first.`, 1))
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[typed](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"guarded"}`))
	path := "/api/v1/tickets/" + tk.ID

	got := decodeBody[typed](t, agent.do(h, "GET", path, ""))
	if m := moveTo(got.Moves, "active"); m == nil || m.Blocked != "Write the resolution notes first." {
		t.Fatalf("moves: %+v", got.Moves)
	}
	expect(t, agent.do(h, "POST", path+"/transitions", `{"to":"active"}`), 422, "blocked until the notes exist")

	w := agent.do(h, "PATCH", path, `{"resolution":"Rebooted the switch."}`)
	expect(t, w, 200, "write the notes")
	if m := moveTo(decodeBody[typed](t, w).Moves, "active"); m == nil || m.Blocked != "" {
		t.Errorf("after writing the notes the move should be open: %+v", m)
	}
	expect(t, agent.do(h, "POST", path+"/transitions", `{"to":"active"}`), 200, "now allowed")
}

const baseFlow = `kind: lifecycle
record_type: task
initial: todo
states:
  todo:  { label: To do, category: open }
  doing: { label: Doing,  category: open }
  done:  { label: Done,   category: closed }
transitions:
  - { from: todo,  to: doing, verb: Begin, primary: true }
  - { from: doing, to: done,  verb: Finish, primary: true }
`

func installBaseFlow(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry) {
	t.Helper()
	c, err := (&apps.Bundle{Files: map[string][]byte{
		"app.yaml":                     []byte("apiVersion: 1\nname: base-flow\nversion: 1.0.0\nauthor: t\ndescription: \"A default flow for every type.\"\n"),
		"default/lifecycles/task.yaml": []byte(baseFlow),
	}}).Compile()
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

// A type with no lifecycle of its own uses the `task` one, like anything extending a base; a type that has its own
// keeps it. Tickets of a type with no extension table work everywhere: list, read, comment and move.
func TestATypeInheritsTheTaskLifecycle(t *testing.T) {
	h, pool, reg := setupApps(t)
	installBaseFlow(t, pool, reg)
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	// A "problem" ticket: a base row only, as an app-defined type would be.
	var problemID string
	if err := pool.QueryRow(context.Background(), `INSERT INTO tasks (type, title, requester_id, status, state_category)
		VALUES ('problem', 'Recurring firewall drops', $1, 'todo', 'open') RETURNING id`, agent.id).Scan(&problemID); err != nil {
		t.Fatal(err)
	}
	incident := decodeBody[typed](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"an incident"}`))

	problem := decodeBody[typed](t, agent.do(h, "GET", "/api/v1/tickets/"+problemID, ""))
	if problem.Type != "problem" || problem.Reference != "PRB1" || problem.Resolution != "" {
		t.Errorf("problem: %+v", problem)
	}
	// It takes its moves from the task lifecycle...
	if m := moveTo(problem.Moves, "doing"); m == nil || m.Verb != "Begin" || len(problem.Moves) != 1 {
		t.Errorf("a problem is offered the task lifecycle's moves: %+v", problem.Moves)
	}
	// ...while the incident keeps its own.
	if m := moveTo(incident.Moves, "in_progress"); m == nil || m.Verb != "Start work" {
		t.Errorf("an incident keeps the incident lifecycle: %+v", incident.Moves)
	}

	w := agent.do(h, "POST", "/api/v1/tickets/PRB1/transitions", `{"to":"doing"}`)
	expect(t, w, 200, "move a problem")
	expect(t, agent.do(h, "POST", "/api/v1/tickets/PRB1/transitions", `{"to":"in_progress"}`), 422, "an incident status is unknown to a problem")
	expect(t, agent.do(h, "POST", "/api/v1/tickets/PRB1/comments", `{"body":"Window booked."}`), 201, "comment on a problem")
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/PRB1", `{"resolution":"x"}`), 422, "a problem has no resolution notes")
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/PRB1", `{"priority":1}`), 200, "but its base fields edit fine")

	// The queue spans types and can be narrowed to one.
	all := decodeBody[struct {
		Items []typed `json:"items"`
	}](t, agent.do(h, "GET", "/api/v1/tickets", ""))
	if len(all.Items) != 2 {
		t.Errorf("the queue should hold both: %+v", all.Items)
	}
	only := decodeBody[struct {
		Items []typed `json:"items"`
	}](t, agent.do(h, "GET", "/api/v1/tickets?type=problem", ""))
	if len(only.Items) != 1 || only.Items[0].Reference != "PRB1" {
		t.Errorf("type filter: %+v", only.Items)
	}
	// A status is valid in the queue if any lifecycle has it.
	expect(t, agent.do(h, "GET", "/api/v1/tickets?status=doing", ""), 200, "a status only the task lifecycle has")

	// The lifecycle endpoint reports the type asked for, whichever lifecycle answers.
	lc := decodeBody[lifecycleResp](t, agent.do(h, "GET", "/api/v1/lifecycles/problem", ""))
	if lc.RecordType != "problem" || lc.App != "base-flow" || lc.Initial != "todo" {
		t.Errorf("lifecycle for problem: %+v", lc)
	}
	if inc := decodeBody[lifecycleResp](t, agent.do(h, "GET", "/api/v1/lifecycles/incident", "")); inc.App != "incident-management" {
		t.Errorf("incident lifecycle: %+v", inc)
	}
}

func TestRequestersDoNotSeeOtherTypesOfTicketsThatAreNotTheirs(t *testing.T) {
	h, pool := setup(t)
	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	bob := newActor(t, h, pool, "bob@x.test", "end_user")
	if _, err := pool.Exec(context.Background(), `INSERT INTO tasks (type, title, requester_id, status, state_category)
		VALUES ('change', 'Bob change', $1, 'new', 'open')`, bob.id); err != nil {
		t.Fatal(err)
	}
	expect(t, alice.do(h, "GET", "/api/v1/tickets/CHG1", ""), 404, "another requester's change")
	expect(t, bob.do(h, "GET", "/api/v1/tickets/CHG1", ""), 200, "own change")
}
