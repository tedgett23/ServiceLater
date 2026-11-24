package api_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type groupRow struct {
	ID         string `json:"id"`
	Restricted bool   `json:"restricted"`
}

type routedTicket struct {
	ID         string `json:"id"`
	Reference  string `json:"reference"`
	RoutedAway bool   `json:"routed_away"`
}

// A restricted group keeps the records routed to it to its members. Other staff stop seeing them, everywhere they
// could have looked: the record itself, the queue, the counts, search and its comments. The requester still sees
// their own, and so does anyone who holds restricted:read. Records in other groups are untouched.
func TestARestrictedGroupKeepsItsRecords(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	desk := newActor(t, h, pool, "desk@x.test", "agent")
	hannah := newActor(t, h, pool, "hannah@x.test", "agent")
	auditor := newActor(t, h, pool, "auditor@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	customRole(t, h, pool, admin, "auditor", auditor, "restricted:read")

	hr := decodeBody[groupRow](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"HR","restricted":true}`))
	if !hr.Restricted {
		t.Fatalf("created restricted: %+v", hr)
	}
	network := decodeBody[groupRow](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Network"}`))
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+hr.ID+"/members/"+hannah.id, ""), 204, "hannah joins HR")

	salary := decodeBody[routedTicket](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"My salary is wrong this month"}`))
	wifi := decodeBody[routedTicket](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Wi-Fi keeps dropping"}`))
	expect(t, desk.do(h, "PATCH", "/api/v1/tickets/"+wifi.ID, `{"assignment_group_id":"`+network.ID+`"}`), 200, "routing to Network")

	// The service desk routes the misfiled ticket to HR: the save works, and says the ticket has left their sight.
	w := desk.do(h, "PATCH", "/api/v1/tickets/"+salary.ID, `{"assignment_group_id":"`+hr.ID+`"}`)
	expect(t, w, 200, "routing to HR")
	if got := decodeBody[routedTicket](t, w); !got.RoutedAway || got.Reference != salary.Reference {
		t.Errorf("the response says it is routed away: %+v", got)
	}

	// Now only HR, the requester and the auditor see it.
	for _, c := range []struct {
		who  actor
		name string
		sees bool
	}{{desk, "the service desk", false}, {hannah, "an HR member", true}, {lena, "the requester", true},
		{auditor, "someone with restricted:read", true}, {admin, "an admin", true}} {
		want := 404
		if c.sees {
			want = 200
		}
		expect(t, c.who.do(h, "GET", "/api/v1/tickets/"+salary.ID, ""), want, c.name+" opening it")
		listed := false
		for _, it := range decodeBody[struct{ Items []routedTicket }](t, c.who.do(h, "GET", "/api/v1/tickets", "")).Items {
			listed = listed || it.ID == salary.ID
		}
		if listed != c.sees {
			t.Errorf("%s: in the queue %v, want %v", c.name, listed, c.sees)
		}
		found := false
		for _, it := range searchFor(t, h, c.who, "salary").Items {
			found = found || it.Reference == salary.Reference
		}
		if found != c.sees {
			t.Errorf("%s: found by search %v, want %v", c.name, found, c.sees)
		}
	}
	expect(t, desk.do(h, "POST", "/api/v1/tickets/"+salary.ID+"/comments", `{"body":"any news?"}`), 404, "the desk commenting on it")
	expect(t, desk.do(h, "PATCH", "/api/v1/tickets/"+salary.ID, `{"priority":1}`), 404, "the desk editing it")
	expect(t, desk.do(h, "GET", "/api/v1/tickets/"+wifi.ID, ""), 200, "a ticket in an ordinary group is still everyone's")
	counts := decodeBody[struct{ Open, Unassigned int64 }](t, desk.do(h, "GET", "/api/v1/tickets/counts", ""))
	if counts.Open != 1 {
		t.Errorf("the desk's counts leave it out: %+v", counts)
	}

	// HR works it as usual; the assignee keeps it even outside the group.
	expect(t, hannah.do(h, "PATCH", "/api/v1/tickets/"+salary.ID, `{"assignee_id":"`+hannah.id+`"}`), 200, "hannah takes it")
	expect(t, hannah.do(h, "POST", "/api/v1/tickets/"+salary.ID+"/comments", `{"body":"Looking into it.","internal":true}`), 201, "hannah's note")

	// Lifting the restriction gives it back to everyone.
	expect(t, admin.do(h, "PATCH", "/api/v1/groups/"+hr.ID, `{"restricted":false}`), 200, "lifting it")
	expect(t, desk.do(h, "GET", "/api/v1/tickets/"+salary.ID, ""), 200, "the desk once HR is no longer restricted")
}

// Only someone who holds roles:manage may restrict a group, lift it, or change who is in a restricted one: each changes
// who sees its records. A group manager may still run ordinary groups.
func TestRestrictingAGroupNeedsRolesManage(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	manager := newActor(t, h, pool, "manager@x.test", "agent")
	other := newActor(t, h, pool, "other@x.test", "agent")
	customRole(t, h, pool, admin, "group-boss", manager, "groups:manage")

	refusal(t, manager.do(h, "POST", "/api/v1/groups", `{"name":"Secret","restricted":true}`), 403, "roles:manage", "creating a restricted group")
	plain := decodeBody[groupRow](t, manager.do(h, "POST", "/api/v1/groups", `{"name":"Plain"}`))
	refusal(t, manager.do(h, "PATCH", "/api/v1/groups/"+plain.ID, `{"restricted":true}`), 403, "roles:manage", "restricting it")
	expect(t, manager.do(h, "PATCH", "/api/v1/groups/"+plain.ID, `{"restricted":false,"description":"still plain"}`), 200, "saving it unchanged")
	expect(t, manager.do(h, "PUT", "/api/v1/groups/"+plain.ID+"/members/"+other.id, ""), 204, "joining an ordinary group")

	hr := decodeBody[groupRow](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"HR","restricted":true}`))
	refusal(t, manager.do(h, "PUT", "/api/v1/groups/"+hr.ID+"/members/"+manager.id, ""), 403, "restricted", "joining HR")
	refusal(t, manager.do(h, "PATCH", "/api/v1/groups/"+hr.ID, `{"restricted":false}`), 403, "roles:manage", "lifting it")
	if !strings.Contains(manager.do(h, "GET", "/api/v1/groups/"+hr.ID, "").Body.String(), `"restricted":true`) {
		t.Error("everyone may see that a group is restricted")
	}
}

// Live updates follow the same rule: someone who cannot see a restricted record does not even hear its id.
func TestRealtimeKeepsRestrictedRecordsQuiet(t *testing.T) {
	hub := startHub(t)
	h, pool, _ := setupWith(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	desk := newActor(t, h, pool, "desk@x.test", "agent")
	hannah := newActor(t, h, pool, "hannah@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	hr := decodeBody[groupRow](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"HR","restricted":true}`))
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+hr.ID+"/members/"+hannah.id, ""), 204, "hannah joins HR")
	tk := decodeBody[routedTicket](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Parental leave dates"}`))
	expect(t, admin.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignment_group_id":"`+hr.ID+`"}`), 200, "routing to HR")

	d := mustDial(t, srv, desk)
	hn := mustDial(t, srv, hannah)
	expect(t, lena.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/comments", `{"body":"Starting in March."}`), 201, "lena comments")
	if e := hn.next(t, "comment.added"); e["ticket_id"] != tk.ID {
		t.Errorf("HR hears of it: %v", e)
	}
	hn.next(t, "notification.changed") // and is told: nobody has the ticket yet (core-itsm's comment-to-group)
	d.none(t, "the service desk must not hear of a restricted record")

	expect(t, hannah.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"priority":2}`), 200, "hannah edits it")
	if e := hn.next(t, "ticket.changed"); e["id"] != tk.ID {
		t.Errorf("HR hears of the change: %v", e)
	}
	d.none(t, "nor of its changes")
}
