package api_test

import (
	"slices"
	"strings"
	"testing"
)

// core-itsm's comment rules: a comment tells the people it @mentions, the assignee and, when it is public, the
// requester, each once and never the person who wrote it; an internal note reaches only those who may read internal
// notes, and nobody can be mentioned who could not read the comment. The text comes from the comment, as the reader
// may see it. incident-management tells the requester when their incident is put on hold and when it is resolved.
func TestCommentsTellPeople(t *testing.T) {
	h, pool := setup(t)
	priya := newActor(t, h, pool, "priya@x.test", "agent")
	tomas := newActor(t, h, pool, "tomas@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	bob := newActor(t, h, pool, "bob@x.test", "end_user")

	tk := decodeBody[routedTicket](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Laptop will not boot"}`))
	expect(t, priya.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignee_id":"`+priya.id+`"}`), 200, "Priya takes it")
	count := func(a actor, title string) int {
		n := 0
		for _, x := range about(t, h, a, tk.ID) {
			if strings.Contains(x.Title, title) {
				n++
			}
		}
		return n
	}

	// A public comment mentioning the assignee: she hears once, as a mention; the requester hears of the comment.
	c := decodeBody[struct {
		Mentions []struct{ ID, Name string }
	}](t, tomas.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/comments", `{"body":"@priya can you look at the BIOS?","mentions":["`+priya.id+`","`+priya.id+`"]}`))
	if len(c.Mentions) != 1 || c.Mentions[0].ID != priya.id {
		t.Errorf("the comment's mentions: %+v", c.Mentions)
	}
	got := about(t, h, priya, tk.ID)
	if len(got) != 1 || !strings.Contains(got[0].Title, "mentioned you on "+tk.Reference) || got[0].Body != "@priya can you look at the BIOS?" {
		t.Fatalf("Priya's notifications: %+v", got)
	}
	if count(lena, "commented on "+tk.Reference) != 1 {
		t.Errorf("the requester is not told of the comment: %+v", about(t, h, lena, tk.ID))
	}
	if count(tomas, "") != 0 {
		t.Errorf("the writer is told of their own comment: %+v", about(t, h, tomas, tk.ID))
	}

	// The comment box offers only people who may read the comment, and only to those who may read the directory.
	type person struct{ ID, Name string }
	offered := func(q string, internal bool) []string {
		var out []string
		for _, p := range decodeBody[struct{ Items []person }](t, tomas.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/mentionable?q="+q+"&internal="+map[bool]string{true: "true", false: "false"}[internal], "")).Items {
			out = append(out, p.ID)
		}
		return out
	}
	if got := offered("x.test", false); !slices.Contains(got, lena.id) || !slices.Contains(got, priya.id) || slices.Contains(got, bob.id) {
		t.Errorf("offered for a comment: %v (lena %s, priya %s, bob %s)", got, lena.id, priya.id, bob.id)
	}
	if got := offered("x.test", true); slices.Contains(got, lena.id) || !slices.Contains(got, priya.id) {
		t.Errorf("offered for an internal note: %v", got)
	}
	expect(t, lena.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/mentionable?q=x", ""), 403, "the requester looking up people")

	// Nobody may be mentioned who could not read the comment.
	refusal(t, tomas.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/comments", `{"body":"@lena","internal":true,"mentions":["`+lena.id+`"]}`), 422, "cannot read this internal note", "mentioning the requester in an internal note")
	refusal(t, tomas.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/comments", `{"body":"@bob","mentions":["`+bob.id+`"]}`), 422, "cannot see this record", "mentioning a stranger")

	// An internal note tells the assignee, never the requester.
	expect(t, tomas.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/comments", `{"body":"Probably the disk.","internal":true}`), 201, "an internal note")
	if count(priya, "commented on") != 1 {
		t.Errorf("the assignee is not told of the internal note: %+v", about(t, h, priya, tk.ID))
	}
	if count(lena, "commented on") != 1 {
		t.Errorf("the requester hears of an internal note: %+v", about(t, h, lena, tk.ID))
	}
	for _, n := range inbox(t, h, lena).Items {
		if strings.Contains(n.Body, "disk") {
			t.Errorf("the requester can read an internal note in a notification: %+v", n)
		}
	}

	// The requester's reply tells the assignee.
	expect(t, lena.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/comments", `{"body":"It beeps three times."}`), 201, "the requester replies")
	if count(priya, "commented on") != 2 {
		t.Errorf("the assignee is not told of the reply: %+v", about(t, h, priya, tk.ID))
	}
	if count(lena, "commented on") != 1 {
		t.Errorf("the requester is told of her own comment: %+v", about(t, h, lena, tk.ID))
	}

	// On hold, then resolved: the requester hears of both, with the resolution notes.
	expect(t, priya.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/transitions", `{"to":"on_hold"}`), 200, "on hold")
	expect(t, priya.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"resolution":"Replaced the disk."}`), 200, "resolution notes")
	expect(t, priya.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/transitions", `{"to":"resolved"}`), 200, "resolved")
	if count(lena, tk.Reference+" is on hold") != 1 {
		t.Errorf("the requester is not told it is on hold: %+v", about(t, h, lena, tk.ID))
	}
	var resolved *sentNotification
	for _, n := range about(t, h, lena, tk.ID) {
		if n.Title == tk.Reference+" was resolved" {
			resolved = &n
		}
	}
	if resolved == nil || resolved.Body != "Replaced the disk." {
		t.Errorf("the requester is not told it was resolved: %+v", about(t, h, lena, tk.ID))
	}
	if count(priya, "resolved") != 0 {
		t.Errorf("whoever resolves it is told: %+v", about(t, h, priya, tk.ID))
	}
}

// A comment on a ticket nobody has taken yet tells the team it is routed to, when the requester writes it.
func TestARequesterCommentTellsTheTeamWhenNobodyHasIt(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	priya := newActor(t, h, pool, "priya@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	g := decodeBody[groupRow](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Desk"}`))
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+g.ID+"/members/"+priya.id, ""), 204, "Priya joins")
	tk := decodeBody[routedTicket](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"No sound"}`))
	expect(t, admin.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignment_group_id":"`+g.ID+`"}`), 200, "routing it")
	before := len(about(t, h, priya, tk.ID))
	expect(t, lena.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/comments", `{"body":"Still nothing."}`), 201, "Lena comments")
	got := about(t, h, priya, tk.ID)
	if len(got) != before+1 || !strings.Contains(got[0].Title, "commented on "+tk.Reference) || got[0].Body != "Still nothing." {
		t.Errorf("the team is not told: %+v", got)
	}
}
