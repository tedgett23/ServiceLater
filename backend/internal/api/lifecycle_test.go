package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

type lifecycleResp struct {
	RecordType string `json:"record_type"`
	Initial    string `json:"initial"`
	App        string `json:"app"`
	Version    string `json:"version"`
	States     []struct {
		ID       string `json:"id"`
		Label    string `json:"label"`
		Category string `json:"category"`
	} `json:"states"`
}

type move struct {
	To      string `json:"to"`
	Verb    string `json:"verb"`
	Primary bool   `json:"primary"`
	Blocked string `json:"blocked"`
}

type withMoves struct {
	ticket
	Moves []move `json:"moves"`
}

func TestLifecycleEndpoint(t *testing.T) {
	h, pool := setup(t)
	lena := newActor(t, h, pool, "lena@x.test", "end_user")

	expect(t, do(h, "GET", "/api/v1/lifecycles/incident", "", nil), 401, "anonymous")
	expect(t, lena.do(h, "GET", "/api/v1/lifecycles/widget", ""), 404, "unknown record type")

	w := lena.do(h, "GET", "/api/v1/lifecycles/incident", "") // any signed-in user may read it: the UI needs the labels
	expect(t, w, 200, "requester")
	lc := decodeBody[lifecycleResp](t, w)
	if lc.RecordType != "incident" || lc.Initial != "new" || lc.App != "incident-management" || lc.Version != "0.3.0" {
		t.Errorf("lifecycle: %+v", lc)
	}
	if len(lc.States) != 5 || lc.States[0].ID != "new" || lc.States[2].Category != "waiting" {
		t.Errorf("states (in the author's order): %+v", lc.States)
	}
	// The rules are the server's business: guards are not published, and neither are transitions.
	body := w.Body.String()
	for _, leak := range []string{"transitions", "when", "verb"} {
		if strings.Contains(body, `"`+leak+`"`) {
			t.Errorf("the lifecycle endpoint should not publish %q: %s", leak, body)
		}
	}
}

func TestMovesAreOfferedToStaffOnly(t *testing.T) {
	h, pool := setup(t)
	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, alice.do(h, "POST", "/api/v1/tickets", `{"title":"t"}`))

	got := decodeBody[withMoves](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID, ""))
	if len(got.Moves) != 4 || got.Moves[0].To != "in_progress" || !got.Moves[0].Primary || got.Moves[0].Verb != "Start work" {
		t.Errorf("moves for a new ticket, primary first: %+v", got.Moves)
	}
	for _, m := range got.Moves {
		if m.Blocked != "" {
			t.Errorf("the shipped lifecycle has no guards: %+v", m)
		}
	}
	if own := decodeBody[withMoves](t, alice.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")); len(own.Moves) != 0 {
		t.Errorf("a requester must not be offered moves: %+v", own.Moves)
	}
	// Lists do not carry moves.
	if strings.Contains(agent.do(h, "GET", "/api/v1/tickets", "").Body.String(), `"moves"`) {
		t.Error("list responses should not include moves")
	}
}

func TestTicketsCarryTheirStateCategory(t *testing.T) {
	h, pool := setup(t)
	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	type withCategory struct {
		ticket
		Category string `json:"state_category"`
	}
	tk := decodeBody[withCategory](t, alice.do(h, "POST", "/api/v1/tickets", `{"title":"t"}`))
	if tk.Status != "new" || tk.Category != "open" {
		t.Fatalf("a new ticket starts at the lifecycle's initial state: %+v", tk)
	}
	move := func(to string) withCategory {
		w := agent.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/transitions", fmt.Sprintf(`{"to":%q}`, to))
		expect(t, w, 200, "move to "+to)
		return decodeBody[withCategory](t, w)
	}
	// Walk the flow, checking the stored category after every move.
	for _, step := range []struct{ to, category string }{
		{"on_hold", "waiting"}, {"in_progress", "open"}, {"resolved", "done"}, {"closed", "closed"},
	} {
		if got := move(step.to); got.Category != step.category {
			t.Errorf("after moving to %s: category %q, want %q", step.to, got.Category, step.category)
		}
	}
}

func TestOpenFilterUsesCategories(t *testing.T) {
	h, pool := setup(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	mk := func(title, to string) {
		tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", fmt.Sprintf(`{"title":%q}`, title)))
		if to != "" {
			expect(t, agent.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/transitions", fmt.Sprintf(`{"to":%q}`, to)), 200, "move")
		}
	}
	mk("new", "")
	mk("held", "on_hold")    // waiting: still open work
	mk("solved", "resolved") // done
	mk("shut", "closed")     // closed
	got := decodeBody[list](t, agent.do(h, "GET", "/api/v1/tickets?open=true", ""))
	if len(got.Items) != 2 {
		t.Errorf("open=true returned %d tickets, want 2 (open and waiting)", len(got.Items))
	}
}

// The API must follow whatever lifecycle is installed, and follow a change the moment the registry reloads,
// which is what an install on another instance triggers through NOTIFY.
func TestAPIFollowsTheInstalledLifecycle(t *testing.T) {
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	ctx := context.Background()

	if _, err := apps.Uninstall(ctx, pool, "incident-management"); err != nil {
		t.Fatal(err)
	}
	custom, err := (&apps.Bundle{Files: map[string][]byte{
		"app.yaml": []byte("apiVersion: 1\nname: my-flow\nversion: 1.0.0\nauthor: t\ndescription: \"Mine.\"\ndependencies:\n  - core-itsm: \">=0.1.0\"\n"),
		"default/lifecycles/incident.yaml": []byte(`kind: lifecycle
record_type: incident
initial: triage
states:
  triage: { label: Triage, category: open }
  fixing: { label: Fixing, category: waiting }
  fixed:  { label: Fixed,  category: closed }
transitions:
  - { from: triage, to: fixing, verb: Take it, primary: true }
  - { from: fixing, to: fixed,  verb: Done }
`),
	}}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Install(ctx, pool, custom, apps.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(ctx); err != nil { // what the NOTIFY listener does on a live instance
		t.Fatal(err)
	}

	type withCategory struct {
		ticket
		Category string `json:"state_category"`
	}
	tk := decodeBody[withCategory](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"custom"}`))
	if tk.Status != "triage" || tk.Category != "open" {
		t.Fatalf("new tickets must start at the installed lifecycle's initial state: %+v", tk)
	}
	path := "/api/v1/tickets/" + tk.ID + "/transitions"
	expect(t, agent.do(h, "POST", path, `{"to":"in_progress"}`), 422, "a state the installed lifecycle does not have")
	expect(t, agent.do(h, "POST", path, `{"to":"fixed"}`), 409, "a move the installed lifecycle does not allow")
	w := agent.do(h, "POST", path, `{"to":"fixing"}`)
	expect(t, w, 200, "an allowed move")
	if got := decodeBody[withCategory](t, w); got.Category != "waiting" {
		t.Errorf("category %q, want waiting", got.Category)
	}
	expect(t, agent.do(h, "GET", "/api/v1/tickets?status=resolved", ""), 400, "filtering by a state that no installed lifecycle has")
	expect(t, agent.do(h, "GET", "/api/v1/tickets?status=fixing", ""), 200, "filtering by an installed state")
}

func TestTicketsStrandedByAnUninstallCanStillBeClosed(t *testing.T) {
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"stranded"}`))
	path := "/api/v1/tickets/" + tk.ID + "/transitions"
	expect(t, agent.do(h, "POST", path, `{"to":"in_progress"}`), 200, "work it")

	if _, err := apps.Uninstall(context.Background(), pool, "incident-management"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The ticket is still there, still readable, and can be closed but not pushed anywhere else.
	got := decodeBody[withMoves](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID, ""))
	if got.Status != "in_progress" {
		t.Fatalf("uninstall changed a ticket: %+v", got)
	}
	if len(got.Moves) != 1 || got.Moves[0].To != "closed" || got.Moves[0].Verb != "Close" {
		t.Errorf("a stranded ticket is offered exactly one move, closing it: %+v", got.Moves)
	}
	expect(t, agent.do(h, "POST", path, `{"to":"open"}`), 409, "a stranded ticket may not move into the fallback's open state")
	expect(t, agent.do(h, "POST", path, `{"to":"closed"}`), 200, "but it can be closed")
}

const guardedApp = `apiVersion: 1
name: guarded
version: 1.0.0
author: t
description: "Guards."
dependencies:
  - core-itsm: ">=0.1.0"
`

const guardedLifecycle = `kind: lifecycle
record_type: incident
initial: new
states:
  new:    { label: New,    category: open }
  active: { label: Active, category: open }
  done:   { label: Done,   category: closed }
transitions:
  - { from: new,    to: active, verb: Start, primary: true, when: 'has(record.assignee_id)', error: Assign the incident before starting work. }
  - { from: active, to: done,   verb: Finish, when: 'user.role == "admin" || record.priority == 4', error: Only an admin can finish anything above low priority. }
`

// installApp replaces incident-management with a test app and reloads the registry, as a live install would.
func installApp(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry, lifecycle string) {
	t.Helper()
	ctx := context.Background()
	if _, err := apps.Uninstall(ctx, pool, "incident-management"); err != nil {
		t.Fatal(err)
	}
	c, err := (&apps.Bundle{Files: map[string][]byte{
		"app.yaml":                         []byte(guardedApp),
		"default/lifecycles/incident.yaml": []byte(lifecycle),
	}}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Install(ctx, pool, c, apps.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
}

func moveTo(ms []move, to string) *move {
	for i := range ms {
		if ms[i].To == to {
			return &ms[i]
		}
	}
	return nil
}

func TestGuardsBlockMovesAndSayWhy(t *testing.T) {
	h, pool, reg := setupApps(t)
	installApp(t, pool, reg, guardedLifecycle)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	boss := newActor(t, h, pool, "boss@x.test", "admin")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"guarded"}`))
	path := "/api/v1/tickets/" + tk.ID

	// Unassigned: the move is listed, marked blocked, and says what to do about it.
	got := decodeBody[withMoves](t, agent.do(h, "GET", path, ""))
	start := moveTo(got.Moves, "active")
	if start == nil || start.Blocked != "Assign the incident before starting work." || !start.Primary {
		t.Fatalf("moves: %+v", got.Moves)
	}
	// Trying anyway is refused with the same message, and nothing changes.
	w := agent.do(h, "POST", path+"/transitions", `{"to":"active"}`)
	expect(t, w, 422, "a blocked move")
	if !strings.Contains(w.Body.String(), "Assign the incident before starting work.") {
		t.Errorf("the refusal should carry the message: %s", w.Body)
	}
	if now := decodeBody[ticket](t, agent.do(h, "GET", path, "")); now.Status != "new" {
		t.Errorf("a blocked move changed the ticket: %s", now.Status)
	}

	// Assigning changes the answer, and the response to the edit already says so.
	w = agent.do(h, "PATCH", path, fmt.Sprintf(`{"assignee_id":%q}`, agent.id))
	expect(t, w, 200, "assign")
	if m := moveTo(decodeBody[withMoves](t, w).Moves, "active"); m == nil || m.Blocked != "" {
		t.Errorf("after assigning, the move should be open: %+v", m)
	}
	expect(t, agent.do(h, "POST", path+"/transitions", `{"to":"active"}`), 200, "the move is now allowed")

	// A guard can depend on who is acting: an agent cannot finish this ticket, an admin can.
	agentView := decodeBody[withMoves](t, agent.do(h, "GET", path, ""))
	if m := moveTo(agentView.Moves, "done"); m == nil || m.Blocked != "Only an admin can finish anything above low priority." {
		t.Errorf("agent's view of Finish: %+v", m)
	}
	expect(t, agent.do(h, "POST", path+"/transitions", `{"to":"done"}`), 422, "an agent finishing")
	if m := moveTo(decodeBody[withMoves](t, boss.do(h, "GET", path, "")).Moves, "done"); m == nil || m.Blocked != "" {
		t.Errorf("the admin's view of Finish: %+v", m)
	}
	expect(t, boss.do(h, "POST", path+"/transitions", `{"to":"done"}`), 200, "an admin finishing")
}

// A guard that can no longer be checked must block its move, never wave it through.
func TestAGuardThatStoppedCompilingFailsClosed(t *testing.T) {
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t"}`))

	// Simulate a platform upgrade that removed a field an installed app's guard still mentions. The installer would
	// reject this guard, so it is put in the database directly.
	ctx := context.Background()
	if _, err := apps.Uninstall(ctx, pool, "incident-management"); err != nil {
		t.Fatal(err)
	}
	body := `{"record_type":"incident","initial":"new","states":[{"id":"new","label":"New","category":"open"},{"id":"active","label":"Active","category":"open"}],
	  "transitions":[{"from":"new","to":"active","verb":"Start","primary":true,"when":"record.a_field_that_was_removed > 1","error":"unused"}]}`
	if _, err := pool.Exec(ctx, `INSERT INTO apps (name, version, manifest) VALUES ('legacy', '1.0.0', '{"name":"legacy","dependencies":[]}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_definitions (app_name, kind, name, version, checksum, body) VALUES ('legacy', 'lifecycle', 'incident', 1, 'x', $1::jsonb)`, body); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}

	got := decodeBody[withMoves](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID, ""))
	if m := moveTo(got.Moves, "active"); m == nil || !strings.Contains(m.Blocked, "could not be checked") {
		t.Fatalf("the move must be listed as blocked: %+v", got.Moves)
	}
	w := agent.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/transitions", `{"to":"active"}`)
	expect(t, w, 422, "the broken guard blocks the move")
	if !strings.Contains(w.Body.String(), "could not be checked") {
		t.Errorf("message: %s", w.Body)
	}
}
