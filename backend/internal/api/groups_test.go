package api_test

import (
	"net/url"
	"strings"
	"testing"
)

type group struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	MemberCount int      `json:"member_count"`
	Members     []member `json:"members"`
}

type member struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

type groupList struct {
	Items []group `json:"items"`
}

type memberList struct {
	Items []member `json:"items"`
}

type groupTicket struct {
	ticket
	AssignmentGroupID   *string `json:"assignment_group_id"`
	AssignmentGroupName *string `json:"assignment_group_name"`
}

type groupTicketList struct {
	Items []groupTicket `json:"items"`
}

func TestGroupsRequireAuth(t *testing.T) {
	h, _ := setup(t)
	expect(t, do(h, "GET", "/api/v1/groups", "", nil), 401, "GET groups")
	expect(t, do(h, "POST", "/api/v1/groups", `{"name":"x"}`, nil), 401, "POST groups")
	expect(t, do(h, "GET", "/api/v1/users", "", nil), 401, "GET users")
}

// Everyone signed in reads groups (a workflow run as an end user must resolve group()); only admins change them.
func TestGroupsAreReadByAllAndChangedByAdmins(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	user := newActor(t, h, pool, "user@x.test", "end_user")

	for _, a := range []actor{agent, user} {
		expect(t, a.do(h, "POST", "/api/v1/groups", `{"name":"Network"}`), 403, "non-admin creating a group")
	}
	w := admin.do(h, "POST", "/api/v1/groups", `{"name":"  Network ","description":"Routers and links"}`)
	expect(t, w, 201, "admin creating a group")
	net := decodeBody[group](t, w)
	if net.Name != "Network" || net.Description != "Routers and links" || net.MemberCount != 0 {
		t.Errorf("created: %+v", net)
	}
	if loc := w.Header().Get("Location"); loc != "/api/v1/groups/"+net.ID {
		t.Errorf("Location = %q", loc)
	}
	expect(t, admin.do(h, "POST", "/api/v1/groups", `{"name":"network"}`), 409, "the same name in another case")
	expect(t, admin.do(h, "POST", "/api/v1/groups", `{"name":"   "}`), 422, "a blank name")
	expect(t, admin.do(h, "POST", "/api/v1/groups", `{"name":"`+strings.Repeat("x", 101)+`"}`), 422, "a long name")

	// Membership is open to anyone, not only staff: an approver need not be an agent.
	for _, id := range []string{agent.id, user.id} {
		expect(t, admin.do(h, "PUT", "/api/v1/groups/"+net.ID+"/members/"+id, ""), 204, "adding a member")
	}
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+net.ID+"/members/"+agent.id, ""), 204, "adding a member twice")
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+net.ID+"/members/00000000-0000-0000-0000-000000000000", ""), 404, "adding nobody")
	expect(t, agent.do(h, "PUT", "/api/v1/groups/"+net.ID+"/members/"+agent.id, ""), 403, "an agent adding themselves")

	for _, a := range []actor{admin, agent, user} {
		l := decodeBody[groupList](t, a.do(h, "GET", "/api/v1/groups", ""))
		if len(l.Items) != 1 || l.Items[0].MemberCount != 2 {
			t.Errorf("groups as seen by %s: %+v", a.id, l.Items)
		}
		g := decodeBody[group](t, a.do(h, "GET", "/api/v1/groups/"+net.ID, ""))
		if len(g.Members) != 2 {
			t.Errorf("members as seen by %s: %+v", a.id, g.Members)
		}
	}
	expect(t, user.do(h, "GET", "/api/v1/groups/not-an-id", ""), 404, "a malformed id")

	expect(t, user.do(h, "PATCH", "/api/v1/groups/"+net.ID, `{"name":"Mine now"}`), 403, "an end user renaming")
	renamed := decodeBody[group](t, admin.do(h, "PATCH", "/api/v1/groups/"+net.ID, `{"name":"Networks"}`))
	if renamed.Name != "Networks" || renamed.Description != "Routers and links" || renamed.MemberCount != 2 {
		t.Errorf("renamed: %+v", renamed)
	}

	expect(t, agent.do(h, "DELETE", "/api/v1/groups/"+net.ID+"/members/"+user.id, ""), 403, "an agent removing a member")
	expect(t, admin.do(h, "DELETE", "/api/v1/groups/"+net.ID+"/members/"+user.id, ""), 204, "removing a member")
	expect(t, admin.do(h, "DELETE", "/api/v1/groups/"+net.ID+"/members/"+user.id, ""), 404, "removing them twice")
	if l := decodeBody[memberList](t, user.do(h, "GET", "/api/v1/groups/"+net.ID+"/members", "")); len(l.Items) != 1 || l.Items[0].ID != agent.id {
		t.Errorf("members after removal: %+v", l.Items)
	}

	// A group with tickets cannot go: those tickets would change without passing through the save path.
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"link down"}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignment_group_id":"`+net.ID+`"}`), 200, "routing a ticket")
	expect(t, admin.do(h, "DELETE", "/api/v1/groups/"+net.ID, ""), 409, "deleting a group with tickets")
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignment_group_id":null}`), 200, "taking it out of the group")
	expect(t, agent.do(h, "DELETE", "/api/v1/groups/"+net.ID, ""), 403, "an agent deleting a group")
	expect(t, admin.do(h, "DELETE", "/api/v1/groups/"+net.ID, ""), 204, "deleting an empty group")
	expect(t, admin.do(h, "DELETE", "/api/v1/groups/"+net.ID, ""), 404, "deleting it twice")
}

func TestTicketAssignmentGroup(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	ann := newActor(t, h, pool, "ann@x.test", "agent")
	bob := newActor(t, h, pool, "bob@x.test", "agent")
	user := newActor(t, h, pool, "user@x.test", "end_user")
	desk := decodeBody[group](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Service Desk"}`))
	net := decodeBody[group](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Network"}`))
	admin.do(h, "PUT", "/api/v1/groups/"+desk.ID+"/members/"+ann.id, "")
	admin.do(h, "PUT", "/api/v1/groups/"+desk.ID+"/members/"+bob.id, "")
	admin.do(h, "PUT", "/api/v1/groups/"+net.ID+"/members/"+bob.id, "")

	tk := decodeBody[groupTicket](t, user.do(h, "POST", "/api/v1/tickets", `{"title":"no wifi"}`))
	if tk.AssignmentGroupID != nil {
		t.Fatalf("a new ticket has no group: %+v", tk)
	}
	path := "/api/v1/tickets/" + tk.ID
	expect(t, user.do(h, "PATCH", path, `{"assignment_group_id":"`+desk.ID+`"}`), 403, "an end user routing")
	expect(t, ann.do(h, "PATCH", path, `{"assignment_group_id":"00000000-0000-0000-0000-000000000000"}`), 422, "a group that does not exist")
	expect(t, ann.do(h, "PATCH", path, `{"assignment_group_id":"desk"}`), 400, "not an id")

	got := decodeBody[groupTicket](t, ann.do(h, "PATCH", path, `{"assignment_group_id":"`+desk.ID+`","assignee_id":"`+ann.id+`"}`))
	if got.AssignmentGroupID == nil || *got.AssignmentGroupID != desk.ID || got.AssignmentGroupName == nil || *got.AssignmentGroupName != "Service Desk" ||
		got.AssigneeID == nil || *got.AssigneeID != ann.id {
		t.Fatalf("routed and assigned: %+v", got)
	}
	// The requester sees which team has it.
	if seen := decodeBody[groupTicket](t, user.do(h, "GET", path, "")); seen.AssignmentGroupName == nil || *seen.AssignmentGroupName != "Service Desk" {
		t.Errorf("the requester's view: %+v", seen)
	}

	// An assignee must be in the group.
	expect(t, ann.do(h, "PATCH", path, `{"assignee_id":"`+admin.id+`"}`), 422, "an assignee outside the group")
	// Handing it to another team lets go of an assignee who is not in it ...
	got = decodeBody[groupTicket](t, ann.do(h, "PATCH", path, `{"assignment_group_id":"`+net.ID+`"}`))
	if got.AssigneeID != nil || *got.AssignmentGroupName != "Network" {
		t.Errorf("moved to Network: %+v", got)
	}
	// ... and keeps one who is.
	ann.do(h, "PATCH", path, `{"assignee_id":"`+bob.id+`"}`)
	got = decodeBody[groupTicket](t, ann.do(h, "PATCH", path, `{"assignment_group_id":"`+desk.ID+`"}`))
	if got.AssigneeID == nil || *got.AssigneeID != bob.id {
		t.Errorf("moved back with bob: %+v", got)
	}
	// Naming both at once is checked against the new group.
	expect(t, ann.do(h, "PATCH", path, `{"assignment_group_id":"`+net.ID+`","assignee_id":"`+ann.id+`"}`), 422, "ann is not in Network")
	// Leaving the group clears the group only.
	got = decodeBody[groupTicket](t, ann.do(h, "PATCH", path, `{"assignment_group_id":null}`))
	if got.AssignmentGroupID != nil || got.AssignmentGroupName != nil || got.AssigneeID == nil {
		t.Errorf("out of the group: %+v", got)
	}

	// The queue filters by group: an id, the caller's own groups, or none.
	other := decodeBody[groupTicket](t, user.do(h, "POST", "/api/v1/tickets", `{"title":"router on fire"}`))
	ann.do(h, "PATCH", "/api/v1/tickets/"+other.ID, `{"assignment_group_id":"`+net.ID+`"}`)
	ann.do(h, "PATCH", path, `{"assignment_group_id":"`+desk.ID+`"}`)
	titles := func(a actor, query string) []string {
		l := decodeBody[groupTicketList](t, a.do(h, "GET", "/api/v1/tickets?"+query, ""))
		var out []string
		for _, it := range l.Items {
			out = append(out, it.Title)
		}
		return out
	}
	if got := titles(ann, "assignment_group="+net.ID); len(got) != 1 || got[0] != "router on fire" {
		t.Errorf("by id: %v", got)
	}
	if got := titles(ann, "assignment_group=mine"); len(got) != 1 || got[0] != "no wifi" {
		t.Errorf("ann's groups: %v", got)
	}
	if got := titles(bob, "assignment_group=mine"); len(got) != 2 {
		t.Errorf("bob's groups: %v", got)
	}
	third := decodeBody[groupTicket](t, user.do(h, "POST", "/api/v1/tickets", `{"title":"new one"}`))
	if got := titles(ann, "assignment_group=none"); len(got) != 1 || got[0] != third.Title {
		t.Errorf("no group: %v", got)
	}
	expect(t, ann.do(h, "GET", "/api/v1/tickets?assignment_group=nope", ""), 400, "a bad filter")
	if l := decodeBody[groupTicketList](t, ann.do(h, "GET", "/api/v1/tickets?assignment_group="+net.ID, "")); l.Items[0].AssignmentGroupName == nil {
		t.Errorf("the queue carries the group's name: %+v", l.Items[0])
	}
}

// Rules route tickets by a group's name, because an app cannot know the ids of the install it lands in.
func TestRuleRoutesToAGroupByName(t *testing.T) {
	h, pool, reg := setupApps(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	user := newActor(t, h, pool, "user@x.test", "end_user")
	net := decodeBody[group](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Network"}`))
	admin.do(h, "PUT", "/api/v1/groups/"+net.ID+"/members/"+agent.id, "")
	installRules(t, pool, reg, map[string]string{
		"route": rule("route-network", "before", "create", "when: 'record.title.contains(\"router\")'\nrun:\n  - action: set\n    field: assignment_group\n    value: network\n"),
		"ghost": rule("route-nowhere", "before", "create", "when: 'record.title.contains(\"ghost\")'\nrun:\n  - action: set\n    field: assignment_group\n    value: Nobody Here\n"),
	})

	// An end user's ticket is routed by the rule; reading the group needs no more rights than filing the ticket.
	w := user.do(h, "POST", "/api/v1/tickets", `{"title":"router down"}`)
	expect(t, w, 201, "filing a ticket the rule routes")
	tk := decodeBody[groupTicket](t, w)
	if tk.AssignmentGroupID == nil || *tk.AssignmentGroupID != net.ID || *tk.AssignmentGroupName != "Network" {
		t.Errorf("routed by the rule: %+v", tk)
	}
	w = user.do(h, "POST", "/api/v1/tickets", `{"title":"ghost in the machine"}`)
	expect(t, w, 422, "a rule naming a group that does not exist")
	if !strings.Contains(w.Body.String(), "Nobody Here") {
		t.Errorf("the error names the group: %s", w.Body)
	}
}

// group('name') asks everyone in a team, as of when the step runs.
func TestApprovalAsksAGroup(t *testing.T) {
	h, pool, reg := setupApps(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	ann := newActor(t, h, pool, "ann@x.test", "end_user")
	bob := newActor(t, h, pool, "bob@x.test", "end_user")
	cab := decodeBody[group](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"CAB"}`))
	admin.do(h, "PUT", "/api/v1/groups/"+cab.ID+"/members/"+ann.id, "")
	admin.do(h, "PUT", "/api/v1/groups/"+cab.ID+"/members/"+bob.id, "")
	groupFlow := func(name string) string {
		return "  ask:\n    type: approval\n    approvers: \"group('" + name + "')\"\n    rule: all\n    next: done\n  done:\n    type: end\n"
	}
	installFlows(t, pool, reg, map[string]string{
		"cab":   wfSrc("cab", "ask", groupFlow("cab")),
		"ghost": wfSrc("ghost", "ask", groupFlow("Ghosts")),
	}, nil)
	startFlowWorker(t, pool, reg)

	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"change the core switch"}`))
	started := decodeBody[run](t, agent.do(h, "POST", "/api/v1/workflow-runs", `{"workflow":"house-flows/cab","ticket":"`+tk.ID+`"}`))
	for _, a := range []actor{ann, bob} {
		waitFor(t, "the request to reach "+a.id, func() bool {
			return len(decodeBody[approvalList](t, a.do(h, "GET", "/api/v1/approvals", "")).Items) == 1
		})
	}
	if l := decodeBody[approvalList](t, agent.do(h, "GET", "/api/v1/approvals", "")); len(l.Items) != 0 {
		t.Errorf("someone outside the group was asked: %+v", l.Items)
	}
	for _, a := range []actor{ann, bob} {
		id := decodeBody[approvalList](t, a.do(h, "GET", "/api/v1/approvals", "")).Items[0].ID
		expect(t, a.do(h, "POST", "/api/v1/approvals/"+id+"/decide", `{"decision":"approved"}`), 200, "deciding")
	}
	waitFor(t, "the run to finish", func() bool {
		return decodeBody[run](t, agent.do(h, "GET", "/api/v1/workflow-runs/"+started.ID, "")).Status == "done"
	})

	// A group that does not exist fails the step and says which, rather than asking nobody.
	ghost := decodeBody[run](t, agent.do(h, "POST", "/api/v1/workflow-runs", `{"workflow":"house-flows/ghost","ticket":"`+tk.ID+`"}`))
	waitFor(t, "the ghost run to fail", func() bool {
		return decodeBody[run](t, agent.do(h, "GET", "/api/v1/workflow-runs/"+ghost.ID, "")).Status == "failed"
	})
	if r := decodeBody[run](t, agent.do(h, "GET", "/api/v1/workflow-runs/"+ghost.ID, "")); !strings.Contains(r.Error, `no group named "Ghosts"`) {
		t.Errorf("error: %q", r.Error)
	}
}

func TestUserDirectory(t *testing.T) {
	h, pool := setup(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	user := newActor(t, h, pool, "ursula@x.test", "end_user")
	newActor(t, h, pool, "zed@x.test", "end_user")

	expect(t, user.do(h, "GET", "/api/v1/users", ""), 403, "an end user listing users")
	all := decodeBody[memberList](t, agent.do(h, "GET", "/api/v1/users", ""))
	roles := map[string]int{}
	for _, u := range all.Items {
		roles[u.Role]++
	}
	if roles["end_user"] != 2 || roles["agent"] != 1 {
		t.Errorf("everyone, not only staff: %+v", all.Items)
	}
	found := decodeBody[memberList](t, agent.do(h, "GET", "/api/v1/users?q="+url.QueryEscape("URS"), ""))
	if len(found.Items) != 1 || found.Items[0].ID != user.id {
		t.Errorf("search: %+v", found.Items)
	}
	if l := decodeBody[memberList](t, agent.do(h, "GET", "/api/v1/users?q=%25", "")); len(l.Items) != 0 {
		t.Errorf("a wildcard is a plain character: %+v", l.Items)
	}
	if l := decodeBody[memberList](t, agent.do(h, "GET", "/api/v1/users?limit=1", "")); len(l.Items) != 1 {
		t.Errorf("limit: %+v", l.Items)
	}
	expect(t, agent.do(h, "GET", "/api/v1/users?limit=0", ""), 400, "a bad limit")
}
