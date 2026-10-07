package api_test

import (
	"context"
	"strings"
	"testing"
)

// An app's principal is not a person: it cannot sign in, is in no list or picker, and admins cannot edit it, give it
// a password, a group or a role. Its permissions come from its manifest only.
func TestAnAppsPrincipalIsNotAPerson(t *testing.T) {
	h, pool, reg := setupApps(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	installNotifyApp(t, pool, reg, map[string]string{})
	var id, email string
	if err := pool.QueryRow(context.Background(), "SELECT id, email FROM users WHERE app_name = 'house-notify'").Scan(&id, &email); err != nil {
		t.Fatal(err)
	}
	if r := login(h, email, "anything-at-all"); r.Code != 401 {
		t.Errorf("an app signed in: %d", r.Code)
	}
	for _, path := range []string{"/api/v1/users?include_deactivated=true&limit=200", "/api/v1/users/staff"} {
		if listed(decodeBody[personList](t, admin.do(h, "GET", path, "")), id) {
			t.Errorf("%s lists an app", path)
		}
	}
	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+id, `{"name":"Mine now"}`), 422, "editing an app's principal")
	expect(t, admin.do(h, "PUT", "/api/v1/users/"+id+"/password", `{"password":"long-enough"}`), 422, "giving it a password")
	expect(t, admin.do(h, "DELETE", "/api/v1/users/"+id, ""), 422, "deleting it")
	g := decodeBody[group](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Apps"}`))
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+g.ID+"/members/"+id, ""), 422, "adding it to a group")
	w := admin.do(h, "POST", "/api/v1/roles/ticket-viewer/grants", `{"user_id":"`+id+`"}`)
	expect(t, w, 422, "granting it a role")
	if !strings.Contains(w.Body.String(), "manifest") {
		t.Errorf("says why: %s", w.Body)
	}
}

// run_as: app lets an after rule do what the app may rather than what the person saving may: a requester's own save
// gets an internal note written, by the app, for them. Without it the same rule fails that save.
func TestAnAfterRuleCanRunAsTheApp(t *testing.T) {
	h, pool, reg := setupApps(t)
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	note := func(runAs string) string {
		return "kind: rule\nname: triage-note\nrecord_type: task\ntiming: after\non: [create]\n" + runAs +
			"run:\n  - action: comment.add\n    body: '=\"Filed by \" + user.name'\n    internal: true\n"
	}
	installNotifyApp(t, pool, reg, map[string]string{"rules/note.yaml": note("")})
	expect(t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"As myself"}`), 403, "a requester's save whose rule writes an internal note as her")

	installNotifyApp(t, pool, reg, map[string]string{"rules/note.yaml": note("run_as: app\n")})
	w := lena.do(h, "POST", "/api/v1/tickets", `{"title":"As the app"}`)
	expect(t, w, 201, "the same rule, run as the app")
	tk := decodeBody[ticket](t, w)
	cs := decodeBody[commentList](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/comments", "")).Items
	if len(cs) != 1 || !cs[0].Internal || cs[0].Author != "house-notify" || cs[0].Behalf != "lena@x.test" || cs[0].Body != "Filed by lena@x.test" {
		t.Fatalf("comments: %+v", cs)
	}
	// She still cannot see it, and the save went on as her: she is the requester.
	if l := decodeBody[commentList](t, lena.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/comments", "")).Items; len(l) != 0 {
		t.Errorf("the requester sees the internal note: %+v", l)
	}
	if tk.Requester != lena.id {
		t.Errorf("requester: %s", tk.Requester)
	}
}
