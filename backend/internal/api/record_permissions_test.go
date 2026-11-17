package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// customRole defines a role with these permissions and grants it to someone through the API, as an admin would.
func customRole(t *testing.T, h http.Handler, pool *pgxpool.Pool, admin actor, name string, to actor, perms ...string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO roles (name, label, permissions) VALUES ($1, $1, $2)
		ON CONFLICT (name) DO NOTHING`, name, perms)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, admin.do(h, "POST", "/api/v1/roles/"+name+"/grants", `{"user_id":"`+to.id+`"}`), 201, "granting "+name)
}

type typedTicket struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Reference string `json:"reference"`
	Status    string `json:"status"`
	Moves     []move `json:"moves"`
}

func refusal(t *testing.T, w *httptest.ResponseRecorder, code int, want, what string) {
	t.Helper()
	expect(t, w, code, what)
	if !strings.Contains(w.Body.String(), want) {
		t.Errorf("%s: want %q in %s", what, want, w.Body)
	}
}

// Record permissions name a type: someone who works changes sees, files, edits and moves changes, and nothing about
// incidents beyond their own. RLS and the Go checks agree, and the refusals name the permission that is missing.
func TestRecordPermissionsAreScopedByType(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	chris := newActor(t, h, pool, "chris@x.test", "end_user")
	customRole(t, h, pool, admin, "change-desk", chris, "records:read:change", "records:create:change", "records:update:change")

	incident := decodeBody[typedTicket](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Laptop is slow"}`))
	if incident.Type != "incident" || !strings.HasPrefix(incident.Reference, "INC") {
		t.Fatalf("a ticket with no type is an incident: %+v", incident)
	}
	refusal(t, lena.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Patch the mail server"}`), 403,
		"records:create:change", "an end user filing a change")

	change := decodeBody[typedTicket](t, chris.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Patch the mail server",
		"change":{"implementation_plan":"Patch it.","backout_plan":"Roll it back."}}`))
	if change.Type != "change" || !strings.HasPrefix(change.Reference, "CHG") || change.Status != "draft" {
		t.Fatalf("the change: %+v", change)
	}
	if moveTo(change.Moves, "assess") == nil {
		t.Errorf("chris works changes, so he is offered their moves: %+v", change.Moves)
	}

	// Seeing: the change desk sees changes, not other people's incidents; the requester sees neither of theirs.
	expect(t, chris.do(h, "GET", "/api/v1/tickets/"+incident.ID, ""), 404, "the change desk reading an incident")
	expect(t, lena.do(h, "GET", "/api/v1/tickets/"+change.ID, ""), 404, "an end user reading someone's change")
	expect(t, agent.do(h, "GET", "/api/v1/tickets/"+change.ID, ""), 200, "an agent reading a change")
	for _, tk := range decodeBody[struct{ Items []typedTicket }](t, chris.do(h, "GET", "/api/v1/tickets", "")).Items {
		if tk.Type != "change" {
			t.Errorf("the change desk's queue has a %s", tk.Type)
		}
	}

	// Working: changes yes, incidents no.
	expect(t, chris.do(h, "PATCH", "/api/v1/tickets/"+change.ID, `{"priority":2}`), 200, "editing a change")
	expect(t, chris.do(h, "POST", "/api/v1/tickets/"+change.ID+"/transitions", `{"to":"assess"}`), 200, "moving a change")
	customRole(t, h, pool, admin, "reader", chris, "records:read:*")
	seen := decodeBody[typedTicket](t, chris.do(h, "GET", "/api/v1/tickets/"+incident.ID, ""))
	if len(seen.Moves) != 0 {
		t.Errorf("chris may read incidents but not work them, so no moves: %+v", seen.Moves)
	}
	refusal(t, chris.do(h, "PATCH", "/api/v1/tickets/"+incident.ID, `{"priority":2}`), 403,
		"records:update:incident", "the change desk editing an incident it can read")
	refusal(t, chris.do(h, "POST", "/api/v1/tickets/"+incident.ID+"/transitions", `{"to":"in_progress"}`), 403,
		"records:update:incident", "the change desk moving an incident")
	refusal(t, lena.do(h, "PATCH", "/api/v1/tickets/"+incident.ID, `{"priority":2}`), 403, "forbidden",
		"an end user editing their own ticket")

	// Assigning: only someone who works the type.
	refusal(t, agent.do(h, "PATCH", "/api/v1/tickets/"+incident.ID, `{"assignee_id":"`+chris.id+`"}`), 422,
		"works tickets of this type", "assigning an incident to the change desk")
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+change.ID, `{"assignee_id":"`+chris.id+`"}`), 200, "assigning a change to the change desk")
	if !listed(decodeBody[personList](t, agent.do(h, "GET", "/api/v1/users/staff?type=change", "")), chris.id) {
		t.Error("chris is not offered as an assignee for changes")
	}
	if listed(decodeBody[personList](t, agent.do(h, "GET", "/api/v1/users/staff?type=incident", "")), chris.id) {
		t.Error("chris is offered as an assignee for incidents")
	}
	expect(t, agent.do(h, "GET", "/api/v1/users/staff?type=task", ""), 400, "staff for the base type")

	// Filing for someone else needs update on the type too; the requester then sees it.
	forLena := decodeBody[typedTicket](t, chris.do(h, "POST", "/api/v1/tickets",
		`{"type":"change","title":"Move Lena's mailbox","requester_id":"`+lena.id+`"}`))
	expect(t, lena.do(h, "GET", "/api/v1/tickets/"+forLena.ID, ""), 200, "the requester reading a change filed for them")
	refusal(t, chris.do(h, "POST", "/api/v1/tickets", `{"title":"For Lena","requester_id":"`+lena.id+`"}`), 403,
		"records:update:incident", "filing an incident for someone else without working incidents")
}

// Only types in use can be filed: registered, and given a lifecycle by an installed app.
func TestOnlyTypesInUseCanBeFiled(t *testing.T) {
	h, pool, reg := setupApps(t)
	withoutProblemManagement(t, pool, reg)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	refusal(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"problem","title":"Recurring outage"}`), 422,
		"is not a record type in use", "a registered type no app provides")
	refusal(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"spaceship","title":"x"}`), 422,
		"is not a record type in use", "an unknown type")
	refusal(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"task","title":"x"}`), 422,
		"is not a record type in use", "the base type")
}

// GET /record-types tells the New ticket dialog what each person may file.
func TestRecordTypesSayWhoMayFileWhat(t *testing.T) {
	h, pool, reg := setupApps(t)
	withoutProblemManagement(t, pool, reg)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	chris := newActor(t, h, pool, "chris@x.test", "end_user")
	customRole(t, h, pool, admin, "change-filer", chris, "records:create:change")

	type rt struct {
		Type             string
		Prefix           string
		CanFile          bool `json:"can_file"`
		CanFileForOthers bool `json:"can_file_for_others"`
	}
	types := func(a actor) map[string]rt {
		out := map[string]rt{}
		for _, x := range decodeBody[struct{ Items []rt }](t, a.do(h, "GET", "/api/v1/record-types", "")).Items {
			out[x.Type] = x
		}
		return out
	}
	if got := types(lena); len(got) != 4 || !got["incident"].CanFile || got["incident"].CanFileForOthers || got["change"].CanFile || got["request"].CanFile || got["work_task"].CanFile {
		t.Errorf("an end user: %+v", got)
	}
	if got := types(chris); !got["change"].CanFile || got["change"].CanFileForOthers || got["change"].Prefix != "CHG" {
		t.Errorf("someone who may create changes: %+v", got)
	}
	if got := types(admin); !got["change"].CanFileForOthers || !got["incident"].CanFileForOthers {
		t.Errorf("an admin: %+v", got)
	}
	if _, ok := types(admin)["problem"]; ok {
		t.Error("a type no app provides is listed")
	}
}

// Live events follow the same rule as reading: the change desk hears about changes and not about other people's
// incidents, including their comments.
func TestRealtimeRoutesByRecordType(t *testing.T) {
	hub := startHub(t)
	h, pool, _ := setupWith(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	chris := newActor(t, h, pool, "chris@x.test", "end_user")
	customRole(t, h, pool, admin, "change-desk", chris, "records:read:change", "records:create:change", "records:update:change")
	c := mustDial(t, srv, chris)

	incident := decodeBody[typedTicket](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Laptop is slow"}`))
	expect(t, lena.do(h, "POST", "/api/v1/tickets/"+incident.ID+"/comments", `{"body":"still slow"}`), 201, "comment")
	c.none(t, "the change desk must not hear about an incident")

	change := decodeBody[typedTicket](t, admin.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Patch"}`))
	if e := c.next(t, "ticket.changed"); e["id"] != change.ID {
		t.Errorf("event %v", e)
	}
	expect(t, admin.do(h, "POST", "/api/v1/tickets/"+change.ID+"/comments", `{"body":"tonight"}`), 201, "comment")
	if e := c.next(t, "comment.added"); e["ticket_id"] != change.ID {
		t.Errorf("event %v", e)
	}
}
