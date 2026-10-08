package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/servicelater/servicelater/backend/internal/realtime"
	"github.com/servicelater/servicelater/backend/internal/slatimer"
)

// wsClient reads on a goroutine: coder/websocket closes the connection when a Read's context expires,
// so "nothing arrived" has to be checked on a channel rather than with a read timeout.
type wsClient struct {
	conn   *websocket.Conn
	events chan map[string]any
	err    chan error // receives the read error once, when the connection ends
}

func startHub(t *testing.T) *realtime.Hub {
	t.Helper()
	dsn := os.Getenv("SL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SL_TEST_DATABASE_URL not set")
	}
	hub := realtime.NewHub()
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx, dsn)
	t.Cleanup(cancel)
	select {
	case <-hub.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("hub never started listening")
	}
	return hub
}

func cookieHeader(a actor) http.Header {
	var parts []string
	for _, c := range a.cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return http.Header{"Cookie": {strings.Join(parts, "; ")}}
}

// dial returns the HTTP status of the handshake (0 if there was none) instead of the response itself.
func dial(t *testing.T, srv *httptest.Server, a actor, extra ...string) (*wsClient, int, error) {
	t.Helper()
	hdr := cookieHeader(a)
	for i := 0; i+1 < len(extra); i += 2 {
		hdr.Set(extra[i], extra[i+1])
	}
	conn, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/v1/subscribe",
		&websocket.DialOptions{HTTPHeader: hdr})
	status := 0
	if resp != nil {
		status = resp.StatusCode
		if resp.Body != nil { // nil after a successful upgrade
			_ = resp.Body.Close()
		}
	}
	if err != nil {
		return nil, status, err
	}
	c := &wsClient{conn: conn, events: make(chan map[string]any, 64), err: make(chan error, 1)}
	t.Cleanup(func() { _ = conn.CloseNow() })
	go func() {
		for {
			_, b, err := conn.Read(context.Background())
			if err != nil {
				c.err <- err
				return
			}
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			c.events <- m
		}
	}()
	return c, status, nil
}

func mustDial(t *testing.T, srv *httptest.Server, a actor) *wsClient {
	t.Helper()
	c, status, err := dial(t, srv, a)
	if err != nil {
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	return c
}

// next waits for the next event of the given type, skipping none: unexpected events fail the test.
func (c *wsClient) next(t *testing.T, want string) map[string]any {
	t.Helper()
	select {
	case e := <-c.events:
		if e["type"] != want {
			t.Fatalf("got event %v, want type %q", e, want)
		}
		return e
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
		return nil
	}
}

func (c *wsClient) none(t *testing.T, why string) {
	t.Helper()
	select {
	case e := <-c.events:
		t.Errorf("%s: unexpected event %v", why, e)
	case <-time.After(400 * time.Millisecond):
	}
}

func TestRealtimeRoutingAndPrivacy(t *testing.T) {
	hub := startHub(t)
	h, pool, _ := setupWith(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()

	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	bob := newActor(t, h, pool, "bob@x.test", "end_user")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	ca, cb, cg := mustDial(t, srv, alice), mustDial(t, srv, bob), mustDial(t, srv, agent)

	// Alice files a ticket: she and staff hear about it, Bob does not.
	tk := decodeBody[ticket](t, alice.do(h, "POST", "/api/v1/tickets", `{"title":"printer"}`))
	for name, c := range map[string]*wsClient{"alice": ca, "agent": cg} {
		e := c.next(t, "ticket.changed")
		if e["id"] != tk.ID || int64(e["number"].(float64)) != tk.Number {
			t.Errorf("%s got %v, want ticket %s", name, e, tk.ID)
		}
	}
	cb.none(t, "bob must not learn about alice's ticket")

	// A transition by staff reaches the requester.
	expect(t, agent.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/transitions", `{"to":"in_progress"}`), 200, "transition")
	ca.next(t, "ticket.changed")
	cg.next(t, "ticket.changed")
	cb.none(t, "bob after transition")

	// A public comment reaches the requester and staff, not Bob.
	expect(t, agent.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/comments", `{"body":"on it"}`), 201, "public comment")
	for _, c := range []*wsClient{ca, cg} {
		if e := c.next(t, "comment.added"); e["ticket_id"] != tk.ID {
			t.Errorf("comment event %v", e)
		}
	}
	cb.none(t, "bob after public comment")

	// An internal note is staff-only: the requester must not even learn it exists.
	expect(t, agent.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/comments", `{"body":"secret","internal":true}`), 201, "internal note")
	cg.next(t, "comment.added")
	ca.none(t, "alice must not hear about an internal note")
	cb.none(t, "bob after internal note")
}

// A pending approval reaches only the person it was asked of, and deciding it tells them again (this time to drop
// the badge), never the stranger watching the same server.
func TestRealtimeNotifiesTheApprover(t *testing.T) {
	hub := startHub(t)
	h, pool, _, reg := build(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()

	agent := newActor(t, h, pool, "agent@x.test", "agent")
	reviewer := newActor(t, h, pool, "review@x.test", "end_user")
	stranger := newActor(t, h, pool, "stranger@x.test", "end_user")
	cr, cs := mustDial(t, srv, reviewer), mustDial(t, srv, stranger)

	installFlows(t, pool, reg, map[string]string{"ask": wfSrc("ask", "ask", askFlow(reviewer.id))}, nil)
	startFlowWorker(t, pool, reg)

	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"needs a yes"}`))
	agent.do(h, "POST", "/api/v1/workflow-runs", `{"workflow":"house-flows/ask","ticket":"`+tk.ID+`"}`)

	cr.next(t, "notification.changed")
	cs.none(t, "a stranger must not learn about someone else's approval")

	l := decodeBody[notificationList](t, reviewer.do(h, "GET", "/api/v1/notifications", ""))
	if len(l.Items) != 1 {
		t.Fatalf("reviewer's notifications: %+v", l)
	}
	expect(t, reviewer.do(h, "POST", "/api/v1/approvals/"+l.Items[0].ApprovalID+"/decide", `{"decision":"approved"}`), 200, "decide")

	cr.next(t, "notification.changed")
	cs.none(t, "a stranger must not learn about someone else's decision")
}

// An app's notify rule pings the person it tells, and nobody else, on the same channel as approvals.
func TestRealtimeNotifiesTheAssignee(t *testing.T) {
	hub := startHub(t)
	h, pool, _ := setupWith(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()

	priya := newActor(t, h, pool, "priya@x.test", "agent")
	tomas := newActor(t, h, pool, "tomas@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	ct, cl := mustDial(t, srv, tomas), mustDial(t, srv, lena)

	tk := decodeBody[ticket](t, priya.do(h, "POST", "/api/v1/tickets", `{"title":"Wi-Fi"}`))
	expect(t, priya.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignee_id":"`+tomas.id+`"}`), 200, "assigning")
	// Tomás also sees the ticket itself change; the notification is among those events.
	deadline := time.After(3 * time.Second)
	for got := false; !got; {
		select {
		case e := <-ct.events:
			got = e["type"] == "notification.changed"
		case <-deadline:
			t.Fatal("Tomás was not pinged")
		}
	}
	cl.none(t, "Lena must not hear about Tomás's notification or a ticket that is not hers")
}

// Everyone may read freezes, so declaring one tells everyone connected, end users included, with only its id.
func TestRealtimeTellsEveryoneAboutFreezes(t *testing.T) {
	hub := startHub(t)
	h, pool, _ := setupWith(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	cl := mustDial(t, srv, lena)
	f := decodeBody[struct{ ID string }](t, admin.do(h, "POST", "/api/v1/freeze-windows",
		`{"name":"Year end","starts_at":"2026-12-20T00:00:00Z","ends_at":"2027-01-03T00:00:00Z"}`))
	if e := cl.next(t, "freeze.changed"); e["id"] != f.ID || len(e) != 2 {
		t.Errorf("the event carries the id and nothing else: %v", e)
	}
	expect(t, admin.do(h, "DELETE", "/api/v1/freeze-windows/"+f.ID, ""), 204, "lifting it")
	cl.next(t, "freeze.changed")
}

func TestRealtimeHandshakeRules(t *testing.T) {
	hub := startHub(t)
	h, pool, _ := setupWith(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	if _, status, err := dial(t, srv, actor{}); err == nil || status != 401 {
		t.Errorf("anonymous dial: err=%v status=%d", err, status)
	}
	if _, status, err := dial(t, srv, agent, "Origin", "https://evil.example"); err == nil || status != 403 {
		t.Errorf("cross-origin dial: err=%v status=%d", err, status)
	}
	if _, _, err := dial(t, srv, agent, "Origin", srv.URL); err != nil {
		t.Errorf("same-origin dial: %v", err)
	}
}

func TestRealtimeConnectionCap(t *testing.T) {
	hub := startHub(t)
	h, pool, _ := setupWith(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	for i := 0; i < 10; i++ {
		if _, _, err := dial(t, srv, agent); err != nil {
			t.Fatalf("connection %d: %v", i+1, err)
		}
	}
	if _, status, err := dial(t, srv, agent); err == nil || status != 429 {
		t.Errorf("11th connection should be refused with 429: err=%v status=%d", err, status)
	}
}

// The cap is a setting (SL_MAX_LIVE_CONNS), not a constant: an office of people with many tabs may want more.
func TestRealtimeConnectionCapIsConfigurable(t *testing.T) {
	hub := startHub(t)
	hub.MaxPerUser = 2
	h, pool, _ := setupWith(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	for i := 0; i < 2; i++ {
		if _, _, err := dial(t, srv, agent); err != nil {
			t.Fatalf("connection %d: %v", i+1, err)
		}
	}
	if _, status, err := dial(t, srv, agent); err == nil || status != 429 {
		t.Errorf("3rd connection should be refused with 429: err=%v status=%d", err, status)
	}
}

func TestRealtimeClosesWhenTokenExpires(t *testing.T) {
	hub := startHub(t)
	h, pool, _ := setupWith(t, 2*time.Second, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	c := mustDial(t, srv, agent)

	select {
	case err := <-c.err:
		if got := websocket.CloseStatus(err); got != realtime.CloseSessionExpired {
			t.Errorf("close status %d, want %d (%v)", got, realtime.CloseSessionExpired, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("socket stayed open past token expiry")
	}
}

func TestRealtimeResyncAfterListenerLoss(t *testing.T) {
	hub := startHub(t)
	h, pool, _ := setupWith(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	cg, ca := mustDial(t, srv, agent), mustDial(t, srv, alice)

	// Kill the hub's LISTEN connection, as a database restart or network blip would.
	tag, err := pool.Exec(context.Background(),
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE 'LISTEN %' AND pid <> pg_backend_pid()")
	if err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("could not terminate listener: %v rows=%d", err, tag.RowsAffected())
	}

	// After it reconnects, every client is told to refetch, because notifications may have been missed.
	cg.next(t, "resync")
	ca.next(t, "resync")

	// And events flow again afterwards.
	expect(t, alice.do(h, "POST", "/api/v1/tickets", `{"title":"after reconnect"}`), 201, "create")
	cg.next(t, "ticket.changed")
}

func TestRealtimeClosesOnShutdown(t *testing.T) {
	dsn := os.Getenv("SL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SL_TEST_DATABASE_URL not set")
	}
	hub := realtime.NewHub()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { hub.Run(ctx, dsn); close(done) }()
	<-hub.Ready()

	h, pool, _ := setupWith(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := mustDial(t, srv, newActor(t, h, pool, "agent@x.test", "agent"))

	cancel()
	select {
	case err := <-c.err:
		if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
			t.Errorf("close status %d, want going away (%v)", got, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("socket not closed on shutdown")
	}
	<-done
}

// A breach the SLA timer finds, with nobody saving, still reaches the ticket's watchers at once, routed like any change
// to the ticket: the requester hears of it, another end user does not.
func TestRealtimeTellsWatchersOfABreach(t *testing.T) {
	hub := startHub(t)
	h, pool, _ := setupWith(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	omar := newActor(t, h, pool, "omar@x.test", "end_user")
	everyHour(t, h, admin)
	mine, theirs := mustDial(t, srv, lena), mustDial(t, srv, omar)
	tk := decodeBody[slaTicket](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Nobody came","priority":1}`))
	mine.next(t, "ticket.changed")

	backdate(t, pool, tk.ID, "respond", time.Hour)
	if _, err := slatimer.Tick(context.Background(), pool, time.Now()); err != nil {
		t.Fatal(err)
	}
	if e := mine.next(t, "ticket.changed"); e["id"] != tk.ID {
		t.Errorf("the breach event: %v", e)
	}
	theirs.none(t, "another end user's ticket breached")
}

// Only those who read the CMDB are told an item changed, with its id only; linking an item to a record tells the
// record's watchers, the requester included, that it changed (they refetch, and see no items).
func TestRealtimeTellsCMDBReadersAboutItems(t *testing.T) {
	hub := startHub(t)
	h, pool, _ := setupWith(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	reader, requester := mustDial(t, srv, agent), mustDial(t, srv, lena)
	ci := decodeBody[struct{ ID string }](t, admin.do(h, "POST", "/api/v1/cis", `{"class":"other","name":"Badge reader"}`))
	if e := reader.next(t, "ci.changed"); e["id"] != ci.ID || len(e) != 2 {
		t.Errorf("the event carries the id and nothing else: %v", e)
	}
	requester.none(t, "an end user cannot read the CMDB")

	tk := decodeBody[struct{ ID string }](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Door will not open"}`))
	requester.next(t, "ticket.changed")
	reader.next(t, "ticket.changed")
	expect(t, agent.do(h, "PUT", "/api/v1/tickets/"+tk.ID+"/cis/"+ci.ID, ""), 200, "linking it")
	if e := requester.next(t, "ticket.changed"); e["id"] != tk.ID {
		t.Errorf("linking an item: %v", e)
	}
}

// Those who see every run hear when one starts or moves, so the builder can show it; others do not.
func TestRealtimeTellsStaffAboutRuns(t *testing.T) {
	hub := startHub(t)
	h, pool, _ := setupWith(t, time.Minute, hub)
	srv := httptest.NewServer(h)
	defer srv.Close()
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	staff, requester := mustDial(t, srv, agent), mustDial(t, srv, lena)
	ch := decodeBody[struct{ ID string }](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Patch"}`))
	staff.next(t, "ticket.changed")
	expect(t, agent.do(h, "POST", "/api/v1/workflow-runs", `{"workflow":"change-management/cab-approval","ticket":"`+ch.ID+`"}`), 201, "starting a run")
	if e := staff.next(t, "run.changed"); e["app"] != "change-management" || e["workflow"] != "cab-approval" {
		t.Errorf("the event names the workflow: %v", e)
	}
	requester.none(t, "an end user does not see runs")
}
