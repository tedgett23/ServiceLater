package api_test

import (
	"strings"
	"testing"
)

type approval struct {
	ID        string `json:"id"`
	Workflow  string `json:"workflow"`
	Reference string `json:"reference"`
	StepID    string `json:"step_id"`
	Group     string `json:"group"`
}

type approvalList struct {
	Items []approval `json:"items"`
}

func TestApprovalsRequireAuth(t *testing.T) {
	h, _, _ := setupApps(t)
	expect(t, do(h, "GET", "/api/v1/approvals", "", nil), 401, "GET approvals")
	expect(t, do(h, "POST", "/api/v1/approvals/x/decide", `{"decision":"approved"}`, nil), 401, "POST decide")
}

// Approving is not staff-only: anyone the workflow names can be asked, and only they see or answer their own request.
func TestApprovalsInboxAndDecide(t *testing.T) {
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	reviewer := newActor(t, h, pool, "review@x.test", "end_user")
	stranger := newActor(t, h, pool, "stranger@x.test", "end_user")
	installFlows(t, pool, reg, map[string]string{"ask": wfSrc("ask", "ask", askFlow(reviewer.id))}, nil)
	startFlowWorker(t, pool, reg)

	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"needs a yes"}`))
	started := decodeBody[run](t, agent.do(h, "POST", "/api/v1/workflow-runs", `{"workflow":"house-flows/ask","ticket":"`+tk.ID+`"}`))
	waitFor(t, "the run to reach the approval step", func() bool {
		r := decodeBody[run](t, agent.do(h, "GET", "/api/v1/workflow-runs/"+started.ID, ""))
		return r.Status == "waiting"
	})

	// Nobody else sees it, and staff status alone does not grant access to somebody else's request.
	if l := decodeBody[approvalList](t, stranger.do(h, "GET", "/api/v1/approvals", "")); len(l.Items) != 0 {
		t.Errorf("a stranger's inbox should be empty, got %+v", l.Items)
	}
	l := decodeBody[approvalList](t, reviewer.do(h, "GET", "/api/v1/approvals", ""))
	if len(l.Items) != 1 || l.Items[0].Workflow != "house-flows/ask" || l.Items[0].StepID != "ask" || !strings.HasPrefix(l.Items[0].Reference, "INC") {
		t.Fatalf("reviewer's inbox: %+v", l.Items)
	}
	id := l.Items[0].ID

	// Answering someone else's request, or with a value that is not a decision, is refused before anything changes.
	expect(t, stranger.do(h, "POST", "/api/v1/approvals/"+id+"/decide", `{"decision":"approved"}`), 404, "someone else deciding")
	expect(t, reviewer.do(h, "POST", "/api/v1/approvals/"+id+"/decide", `{"decision":"maybe"}`), 422, "not a decision")

	expect(t, reviewer.do(h, "POST", "/api/v1/approvals/"+id+"/decide", `{"decision":"approved","comment":"lgtm"}`), 200, "the real approver")
	if l := decodeBody[approvalList](t, reviewer.do(h, "GET", "/api/v1/approvals", "")); len(l.Items) != 0 {
		t.Errorf("a decided request should leave the inbox, got %+v", l.Items)
	}
	expect(t, reviewer.do(h, "POST", "/api/v1/approvals/"+id+"/decide", `{"decision":"rejected"}`), 404, "deciding twice")

	waitFor(t, "the run to finish", func() bool {
		r := decodeBody[run](t, agent.do(h, "GET", "/api/v1/workflow-runs/"+started.ID, ""))
		return r.Status == "done"
	})
	one := decodeBody[run](t, agent.do(h, "GET", "/api/v1/workflow-runs/"+started.ID, ""))
	if one.Vars["ask_decision"] != "approved" || one.Vars["ask_comment"] != "lgtm" {
		t.Errorf("vars: %v", one.Vars)
	}
}

func askFlow(userID string) string {
	return "  ask:\n    type: approval\n    approvers: '[\"" + userID + "\"]'\n    next: done\n  done:\n    type: end\n"
}
