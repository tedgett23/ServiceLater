package api_test

import "testing"

type notification struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	ApprovalID string `json:"approval_id"`
	ReadAt     string `json:"read_at"`
}

type notificationList struct {
	Items       []notification `json:"items"`
	UnreadCount int            `json:"unread_count"`
}

func TestNotificationsRequireAuth(t *testing.T) {
	h, _, _ := setupApps(t)
	expect(t, do(h, "GET", "/api/v1/notifications", "", nil), 401, "GET notifications")
	expect(t, do(h, "POST", "/api/v1/notifications/x/read", "", nil), 401, "POST read")
}

// A pending approval is also a notification to its approver, and nobody else's: anyone can be asked to approve
// something, so this is not staff-only either.
func TestNotificationsInboxAndMarkRead(t *testing.T) {
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	reviewer := newActor(t, h, pool, "review@x.test", "end_user")
	stranger := newActor(t, h, pool, "stranger@x.test", "end_user")
	installFlows(t, pool, reg, map[string]string{"ask": wfSrc("ask", "ask", askFlow(reviewer.id))}, nil)
	startFlowWorker(t, pool, reg)

	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"needs a yes"}`))
	agent.do(h, "POST", "/api/v1/workflow-runs", `{"workflow":"house-flows/ask","ticket":"`+tk.ID+`"}`)

	waitFor(t, "the approval to be filed", func() bool {
		l := decodeBody[notificationList](t, reviewer.do(h, "GET", "/api/v1/notifications", ""))
		return len(l.Items) == 1
	})

	// A stranger has nothing to see, even though the same workflow is running.
	if l := decodeBody[notificationList](t, stranger.do(h, "GET", "/api/v1/notifications", "")); len(l.Items) != 0 || l.UnreadCount != 0 {
		t.Errorf("a stranger's notifications should be empty, got %+v", l)
	}

	l := decodeBody[notificationList](t, reviewer.do(h, "GET", "/api/v1/notifications", ""))
	if len(l.Items) != 1 || l.Items[0].Kind != "approval_pending" || l.Items[0].ApprovalID == "" || l.Items[0].ReadAt != "" || l.UnreadCount != 1 {
		t.Fatalf("reviewer's notifications: %+v", l)
	}
	id := l.Items[0].ID

	// Answering someone else's notification, or one that does not exist, is refused before anything changes.
	expect(t, stranger.do(h, "POST", "/api/v1/notifications/"+id+"/read", ""), 404, "someone else marking it read")

	got := decodeBody[notification](t, reviewer.do(h, "POST", "/api/v1/notifications/"+id+"/read", ""))
	if got.ReadAt == "" {
		t.Fatalf("marked read: %+v", got)
	}
	if l := decodeBody[notificationList](t, reviewer.do(h, "GET", "/api/v1/notifications", "")); l.UnreadCount != 0 {
		t.Errorf("unread count after marking read: %+v", l)
	}
	expect(t, reviewer.do(h, "POST", "/api/v1/notifications/"+id+"/read", ""), 404, "marking it read twice")
}
