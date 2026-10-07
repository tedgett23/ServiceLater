package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// changeProcess is the world the change-management app runs in: its first-party content installed, a worker for
// rules and workflows, an agent who works changes, a requester, and whoever sits on the boards.
type changeProcess struct {
	h       http.Handler
	agent   actor
	lena    actor
	admin   actor
	cab     []actor
	ecab    actor
	planned string
}

func newChangeProcess(t *testing.T) *changeProcess {
	h, pool, reg := setupApps(t)
	startFlowWorker(t, pool, reg)
	w := &changeProcess{h: h, agent: newActor(t, h, pool, "agent@x.test", "agent"),
		lena: newActor(t, h, pool, "lena@x.test", "end_user"), admin: newActor(t, h, pool, "boss@x.test", "admin")}
	for _, email := range []string{"cab1@x.test", "cab2@x.test", "cab3@x.test"} {
		a := newActor(t, h, pool, email, "end_user")
		expect(t, w.admin.do(h, "POST", "/api/v1/roles/cab-member/grants", `{"user_id":"`+a.id+`"}`), 201, "a CAB member")
		w.cab = append(w.cab, a)
	}
	w.ecab = newActor(t, h, pool, "ecab@x.test", "end_user")
	expect(t, w.admin.do(h, "POST", "/api/v1/roles/ecab-member/grants", `{"user_id":"`+w.ecab.id+`"}`), 201, "an ECAB member")
	start := time.Now().Add(-time.Minute).UTC()
	w.planned = `"planned_start":"` + start.Format(time.RFC3339) + `","planned_end":"` + start.Add(2*time.Hour).Format(time.RFC3339) + `"`
	return w
}

// file files a change for Lena, of the given kind, with its plans written, and submits it to Assess.
func (w *changeProcess) file(t *testing.T, kind string) typedTicket {
	t.Helper()
	c := decodeBody[typedTicket](t, w.agent.do(w.h, "POST", "/api/v1/tickets", `{"type":"change","title":"Swap the core switch",
		"requester_id":"`+w.lena.id+`","change":{"change_type":"`+kind+`","implementation_plan":"Swap it.","backout_plan":"Swap it back."}}`))
	expect(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+c.ID+"/transitions", `{"to":"assess"}`), 200, "submitting")
	return c
}

func (w *changeProcess) assess(t *testing.T, c typedTicket) {
	t.Helper()
	expect(t, w.agent.do(w.h, "PATCH", "/api/v1/tickets/"+c.ID, `{"change":{"risk":"medium","impact":"low",`+w.planned+`}}`), 200, "assessing")
}

func (w *changeProcess) get(t *testing.T, c typedTicket) typedTicket {
	t.Helper()
	return decodeBody[typedTicket](t, w.agent.do(w.h, "GET", "/api/v1/tickets/"+c.ID, ""))
}

func (w *changeProcess) waitStatus(t *testing.T, c typedTicket, status string) {
	t.Helper()
	waitFor(t, c.Reference+" in "+status, func() bool { return w.get(t, c).Status == status })
}

// pending is the one approval someone has been asked for on this change, once it arrives.
func (w *changeProcess) pending(t *testing.T, who actor, c typedTicket) approval {
	t.Helper()
	var got approval
	waitFor(t, "an approval request on "+c.Reference, func() bool {
		for _, a := range decodeBody[approvalList](t, who.do(w.h, "GET", "/api/v1/approvals", "")).Items {
			if a.Reference == c.Reference {
				got = a
				return true
			}
		}
		return false
	})
	return got
}

func (w *changeProcess) comments(t *testing.T, c typedTicket) string {
	t.Helper()
	var out []string
	for _, x := range decodeBody[commentList](t, w.agent.do(w.h, "GET", "/api/v1/tickets/"+c.ID+"/comments", "")).Items {
		out = append(out, x.Body)
	}
	return strings.Join(out, "\n")
}

// The lifecycle's guards say what each step needs before the change can move on.
func TestTheChangeLifecycleAsksForWhatEachStepNeeds(t *testing.T) {
	w := newChangeProcess(t)
	bare := decodeBody[typedTicket](t, w.agent.do(w.h, "POST", "/api/v1/tickets", `{"type":"change","title":"Patch it"}`))
	if m := moveTo(bare.Moves, "assess"); m == nil || !strings.Contains(m.Blocked, "implementation and back-out plans") {
		t.Errorf("submitting needs the plans: %+v", bare.Moves)
	}
	refusal(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+bare.ID+"/transitions", `{"to":"assess"}`), 422,
		"implementation and back-out plans", "submitting without plans")

	c := w.file(t, "normal")
	got := w.get(t, c)
	if m := moveTo(got.Moves, "authorize"); m == nil || !strings.Contains(m.Blocked, "Rate the risk and impact") {
		t.Errorf("requesting authorization needs an assessment: %+v", got.Moves)
	}
	w.assess(t, c)
	if m := moveTo(w.get(t, c).Moves, "authorize"); m == nil || m.Blocked != "" {
		t.Errorf("assessed, it can go to the board: %+v", m)
	}
	if m := moveTo(w.get(t, c).Moves, "scheduled"); m == nil || !strings.Contains(m.Blocked, "Only a standard change") {
		t.Errorf("a normal change cannot skip the board: %+v", m)
	}
}

// A normal change goes to the change advisory board. A majority authorizes it, which moves it to Scheduled; that
// move is the board's, so nobody can make it by hand. The requester hears about it.
func TestTheBoardAuthorizesANormalChange(t *testing.T) {
	w := newChangeProcess(t)
	c := w.file(t, "normal")
	w.assess(t, c)
	expect(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+c.ID+"/transitions", `{"to":"authorize"}`), 200, "requesting authorization")

	waiting := w.get(t, c)
	if waiting.Status != "authorize" || moveTo(waiting.Moves, "scheduled") != nil {
		t.Errorf("authorizing is not offered to people: %+v", waiting.Moves)
	}
	refusal(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+c.ID+"/transitions", `{"to":"scheduled"}`), 409,
		"made by the process", "authorizing by hand")
	refusal(t, w.admin.do(w.h, "POST", "/api/v1/tickets/"+c.ID+"/transitions", `{"to":"scheduled"}`), 409,
		"made by the process", "even an admin")

	// Two of three is a majority.
	for _, m := range w.cab[:2] {
		a := w.pending(t, m, c)
		if a.Workflow != "change-management/cab-approval" {
			t.Errorf("asked by %s", a.Workflow)
		}
		expect(t, m.do(w.h, "POST", "/api/v1/approvals/"+a.ID+"/decide", `{"decision":"approved","comment":"Fine by me."}`), 200, "approving")
	}
	w.waitStatus(t, c, "scheduled")
	waitFor(t, "the board's comment", func() bool { return strings.Contains(w.comments(t, c), "Authorized by the change advisory board.") })
	waitFor(t, "the requester is told", func() bool {
		for _, n := range inbox(t, w.h, w.lena).Items {
			if n.Reference == c.Reference && strings.Contains(n.Title, "is scheduled") {
				return true
			}
		}
		return false
	})

	// The window is open, so work can start.
	expect(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+c.ID+"/transitions", `{"to":"implement"}`), 200, "starting work")
}

// When the board says no, the change goes back to Assess with the board's reason.
func TestTheBoardSendsARejectedChangeBack(t *testing.T) {
	w := newChangeProcess(t)
	c := w.file(t, "normal")
	w.assess(t, c)
	expect(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+c.ID+"/transitions", `{"to":"authorize"}`), 200, "requesting authorization")
	for _, m := range w.cab[:2] {
		a := w.pending(t, m, c)
		expect(t, m.do(w.h, "POST", "/api/v1/approvals/"+a.ID+"/decide", `{"decision":"rejected","comment":"Not during month end."}`), 200, "rejecting")
	}
	w.waitStatus(t, c, "assess")
	waitFor(t, "the board's reason", func() bool {
		return strings.Contains(w.comments(t, c), "did not authorize this change. Not during month end.")
	})
}

// An emergency change goes to the emergency board, where one yes is enough.
func TestTheEmergencyBoardAuthorizesAnEmergencyChange(t *testing.T) {
	w := newChangeProcess(t)
	c := w.file(t, "emergency")
	w.assess(t, c)
	expect(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+c.ID+"/transitions", `{"to":"authorize"}`), 200, "requesting authorization")
	a := w.pending(t, w.ecab, c)
	if a.Workflow != "change-management/ecab-approval" {
		t.Errorf("asked by %s", a.Workflow)
	}
	for _, m := range w.cab {
		if l := decodeBody[approvalList](t, m.do(w.h, "GET", "/api/v1/approvals", "")); len(l.Items) != 0 {
			t.Errorf("the normal board is not asked about an emergency change: %+v", l.Items)
		}
	}
	expect(t, w.ecab.do(w.h, "POST", "/api/v1/approvals/"+a.ID+"/decide", `{"decision":"approved"}`), 200, "approving")
	w.waitStatus(t, c, "scheduled")
}

// A standard change is pre-approved: it is scheduled straight from Assess and never goes to a board.
func TestAStandardChangeSkipsTheBoard(t *testing.T) {
	w := newChangeProcess(t)
	c := w.file(t, "standard")
	w.assess(t, c)
	got := w.get(t, c)
	if m := moveTo(got.Moves, "authorize"); m == nil || m.Blocked == "" {
		t.Errorf("a standard change is not sent to a board: %+v", m)
	}
	expect(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+c.ID+"/transitions", `{"to":"scheduled"}`), 200, "scheduling it")
}

// With nobody on the board, the change is not left waiting forever: it goes back with a note saying why.
func TestNobodyOnTheBoardSendsTheChangeBack(t *testing.T) {
	w := newChangeProcess(t)
	for _, m := range w.cab {
		role := decodeBody[struct {
			GrantedTo []struct {
				ID     string `json:"id"`
				UserID string `json:"user_id"`
			} `json:"granted_to"`
		}](t, w.admin.do(w.h, "GET", "/api/v1/roles/cab-member", ""))
		for _, g := range role.GrantedTo {
			if g.UserID == m.id {
				expect(t, w.admin.do(w.h, "DELETE", "/api/v1/roles/cab-member/grants/"+g.ID, ""), 204, "revoking")
			}
		}
	}
	c := w.file(t, "normal")
	w.assess(t, c)
	expect(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+c.ID+"/transitions", `{"to":"authorize"}`), 200, "requesting authorization")
	w.waitStatus(t, c, "assess")
	waitFor(t, "the note", func() bool { return strings.Contains(w.comments(t, c), "Nobody holds the CAB member role") })
}
