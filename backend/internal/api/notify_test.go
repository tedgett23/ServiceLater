package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

type sentNotification struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	TaskID    string `json:"task_id"`
	Reference string `json:"reference"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Link      string `json:"link"`
	ReadAt    string `json:"read_at"`
}

type sentList struct {
	Items       []sentNotification `json:"items"`
	UnreadCount int                `json:"unread_count"`
}

func inbox(t *testing.T, h http.Handler, a actor) sentList {
	t.Helper()
	return decodeBody[sentList](t, a.do(h, "GET", "/api/v1/notifications", ""))
}

// about lists the template notifications a person has about one ticket.
func about(t *testing.T, h http.Handler, a actor, taskID string) []sentNotification {
	t.Helper()
	var out []sentNotification
	for _, n := range inbox(t, h, a).Items {
		if n.TaskID == taskID {
			out = append(out, n)
		}
	}
	return out
}

// installNotifyApp installs an app with its own templates and rules on top of the first-party apps.
func installNotifyApp(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry, files map[string]string) {
	t.Helper()
	bundle := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: house-notify\nversion: 1.0.0\nauthor: t\ndescription: \"Notify.\"\npermissions: [records:read:*, records:update:*, comments:internal]\n")}
	for name, src := range files {
		bundle["default/"+name] = []byte(src)
	}
	c, err := (&apps.Bundle{Files: bundle}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Install(context.Background(), pool, c, apps.InstallOptions{ApprovePermissions: true}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// core-itsm's notify-assignee: the new assignee hears about it, with the text filled in from the ticket; whoever
// made the change does not, and taking a ticket yourself is silent.
func TestAssigningSomeoneNotifiesThem(t *testing.T) {
	h, pool := setup(t)
	priya := newActor(t, h, pool, "priya@x.test", "agent")
	tomas := newActor(t, h, pool, "tomas@x.test", "agent")

	tk := decodeBody[ticket](t, priya.do(h, "POST", "/api/v1/tickets", `{"title":"VPN drops"}`))
	expect(t, priya.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignee_id":"`+tomas.id+`"}`), 200, "assigning Tomás")

	got := about(t, h, tomas, tk.ID)
	if len(got) != 1 {
		t.Fatalf("Tomás's notifications: %+v", inbox(t, h, tomas))
	}
	n := got[0]
	if n.Kind != "template" || !strings.HasSuffix(n.Title, " was assigned to you") || n.Body != "VPN drops" ||
		n.Link != "/tickets/"+n.Reference || !strings.HasPrefix(n.Title, n.Reference) {
		t.Errorf("rendered: %+v", n)
	}
	if len(about(t, h, priya, tk.ID)) != 0 {
		t.Error("the person who assigned it is told about their own change")
	}

	// Saving again without changing the assignee says nothing new.
	expect(t, priya.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"priority":2}`), 200, "another edit")
	if len(about(t, h, tomas, tk.ID)) != 1 {
		t.Error("an unrelated edit notified the assignee again")
	}

	mine := decodeBody[ticket](t, priya.do(h, "POST", "/api/v1/tickets", `{"title":"Mine"}`))
	expect(t, priya.do(h, "PATCH", "/api/v1/tickets/"+mine.ID, `{"assignee_id":"`+priya.id+`"}`), 200, "taking it")
	if len(about(t, h, priya, mine.ID)) != 0 {
		t.Error("taking a ticket yourself notified you")
	}
}

// core-itsm's notify-group: a team hears about a ticket that lands in its queue with nobody on it. The platform
// leaves out end users in the group (they cannot see the ticket), deactivated members and the person routing it.
// Routed with an assignee, only the assignee is told.
func TestRoutingToAGroupNotifiesTheTeam(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	priya := newActor(t, h, pool, "priya@x.test", "agent")
	tomas := newActor(t, h, pool, "tomas@x.test", "agent")
	gone := newActor(t, h, pool, "gone@x.test", "agent")
	approver := newActor(t, h, pool, "approver@x.test", "end_user")
	g := decodeBody[group](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Network"}`))
	for _, a := range []actor{priya, tomas, gone, approver} {
		expect(t, admin.do(h, "PUT", "/api/v1/groups/"+g.ID+"/members/"+a.id, ""), 204, "adding a member")
	}
	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+gone.id, `{"deactivated":true}`), 200, "deactivating one")

	tk := decodeBody[ticket](t, priya.do(h, "POST", "/api/v1/tickets", `{"title":"Switch down"}`))
	expect(t, priya.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignment_group_id":"`+g.ID+`"}`), 200, "routing it")

	got := about(t, h, tomas, tk.ID)
	if len(got) != 1 || !strings.HasSuffix(got[0].Title, " is waiting for Network") {
		t.Fatalf("Tomás: %+v", got)
	}
	if len(about(t, h, priya, tk.ID)) != 0 {
		t.Error("the person routing it was told")
	}
	if l := inbox(t, h, approver); len(l.Items) != 0 {
		t.Errorf("an end user in the group heard about a ticket they cannot see: %+v", l)
	}
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM notifications WHERE user_id = $1", gone.id).Scan(&n); err != nil || n != 0 {
		t.Errorf("a deactivated member was notified: %d %v", n, err)
	}

	// Routed straight to someone, only that person hears about it.
	direct := decodeBody[ticket](t, priya.do(h, "POST", "/api/v1/tickets", `{"title":"Fiber cut"}`))
	expect(t, priya.do(h, "PATCH", "/api/v1/tickets/"+direct.ID, `{"assignment_group_id":"`+g.ID+`","assignee_id":"`+tomas.id+`"}`), 200, "routing to Tomás")
	if got := about(t, h, tomas, direct.ID); len(got) != 1 || !strings.HasSuffix(got[0].Title, "was assigned to you") {
		t.Errorf("Tomás, routed to directly: %+v", got)
	}
}

// Opening a ticket answers its notifications, and only the reader's own.
func TestOpeningATicketMarksItsNotificationsRead(t *testing.T) {
	h, pool := setup(t)
	priya := newActor(t, h, pool, "priya@x.test", "agent")
	tomas := newActor(t, h, pool, "tomas@x.test", "agent")
	tk := decodeBody[ticket](t, priya.do(h, "POST", "/api/v1/tickets", `{"title":"Printer"}`))
	expect(t, priya.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignee_id":"`+tomas.id+`"}`), 200, "assigning")
	if l := inbox(t, h, tomas); l.UnreadCount != 1 {
		t.Fatalf("unread: %+v", l)
	}

	w := priya.do(h, "POST", "/api/v1/notifications/read", `{"task_id":"`+tk.ID+`"}`)
	if got := decodeBody[map[string]int](t, w); got["marked"] != 0 {
		t.Errorf("someone else marked Tomás's notification: %v", got)
	}
	w = tomas.do(h, "POST", "/api/v1/notifications/read", `{"task_id":"`+tk.ID+`"}`)
	if got := decodeBody[map[string]int](t, w); got["marked"] != 1 {
		t.Errorf("marked: %v", got)
	}
	if l := inbox(t, h, tomas); l.UnreadCount != 0 || l.Items[0].ReadAt == "" {
		t.Errorf("after reading: %+v", l)
	}
}

// An app's own templates and rules: an =expression names people, the requester hears about their own ticket, and
// someone named who cannot see the ticket is dropped by the platform whatever the rule says.
func TestAppDefinedNotifications(t *testing.T) {
	h, pool, reg := setupApps(t)
	priya := newActor(t, h, pool, "priya@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	marcus := newActor(t, h, pool, "marcus@x.test", "end_user")
	installNotifyApp(t, pool, reg, map[string]string{
		"notifications/urgent.yaml": "kind: notification\nname: urgent\ntitle: \"{{record.reference}} is now priority {{record.priority}}\"\nbody: \"{{actor.name}} raised it ({{record.status}})\"\n",
		"rules/urgent.yaml": "kind: rule\nname: tell-urgent\nrecord_type: task\ntiming: after\non: [update]\n" +
			"when: record.priority == 1 && old.priority != 1\nrun:\n  - action: notify\n    template: urgent\n    to: [requester, \"=['" + marcus.id + "']\"]\n",
	})

	tk := decodeBody[ticket](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Laptop on fire"}`))
	expect(t, priya.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"priority":1}`), 200, "raising it")

	got := about(t, h, lena, tk.ID)
	if len(got) != 1 || got[0].Title != got[0].Reference+" is now priority 1" || got[0].Body != "priya@x.test raised it (New)" {
		t.Fatalf("the requester: %+v", got)
	}
	if l := inbox(t, h, marcus); len(l.Items) != 0 {
		t.Errorf("someone who cannot see the ticket was told about it: %+v", l)
	}

	// The template goes away with its app; the notification still points at the ticket.
	if _, err := apps.Uninstall(context.Background(), pool, "house-notify"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := about(t, h, lena, tk.ID); len(got) != 1 || got[0].Title != "Update on "+got[0].Reference {
		t.Errorf("after the app is gone: %+v", got)
	}
}

// Async rules can notify too; the worker delivers it after the save.
func TestAsyncRulesNotify(t *testing.T) {
	h, pool, reg := setupApps(t)
	priya := newActor(t, h, pool, "priya@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	installNotifyApp(t, pool, reg, map[string]string{
		"notifications/got-it.yaml": "kind: notification\nname: got-it\ntitle: \"We have {{record.reference}}\"\n",
		"rules/got-it.yaml":         "kind: rule\nname: got-it\nrecord_type: task\ntiming: async\non: [create]\nrun:\n  - action: notify\n    template: got-it\n    to: [requester]\n",
	})
	startWorker(t, pool, reg)
	// Filed by an agent for Lena, so Lena is not the one acting.
	tk := decodeBody[ticket](t, priya.do(h, "POST", "/api/v1/tickets", `{"title":"Phone","requester_id":"`+lena.id+`"}`))
	waitFor(t, "the async notification", func() bool { return len(about(t, h, lena, tk.ID)) == 1 })
}
