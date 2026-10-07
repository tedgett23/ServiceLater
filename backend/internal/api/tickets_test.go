package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

type actor struct {
	id      string
	cookies []*http.Cookie
}

// newActor creates a user with a password and logs them in through the real login endpoint.
func newActor(t *testing.T, h http.Handler, pool *pgxpool.Pool, email, role string) actor {
	t.Helper()
	ctx := context.Background()
	hash, err := auth.HashPassword("password-" + email)
	if err != nil {
		t.Fatal(err)
	}
	var u dbgen.User
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var err error
		if u, err = q.CreateUser(ctx, dbgen.CreateUserParams{Email: email, Name: email, Role: role}); err != nil {
			return err
		}
		return q.UpsertCredentials(ctx, dbgen.UpsertCredentialsParams{UserID: u.ID, PasswordHash: hash})
	}); err != nil {
		t.Fatal(err)
	}
	w := do(h, "POST", "/api/v1/auth/login", fmt.Sprintf(`{"email":%q,"password":%q}`, email, "password-"+email), nil)
	if w.Code != 200 {
		t.Fatalf("login %s: %d %s", email, w.Code, w.Body)
	}
	return actor{id: u.ID.String(), cookies: w.Result().Cookies()}
}

func (a actor) do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	return do(h, method, path, body, a.cookies)
}

func decodeBody[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return v
}

type ticket struct {
	ID            string  `json:"id"`
	Number        int64   `json:"number"`
	Title         string  `json:"title"`
	Status        string  `json:"status"`
	Priority      int     `json:"priority"`
	Requester     string  `json:"requester_id"`
	AssigneeID    *string `json:"assignee_id"`
	RequesterName string  `json:"requester_name"`
	AssigneeName  *string `json:"assignee_name"`
}

type list struct {
	Items   []ticket `json:"items"`
	HasMore bool     `json:"has_more"`
}

func expect(t *testing.T, w *httptest.ResponseRecorder, code int, what string) {
	t.Helper()
	if w.Code != code {
		t.Errorf("%s: want %d, got %d %s", what, code, w.Code, w.Body)
	}
}

func TestTicketsRequireAuth(t *testing.T) {
	h, _ := setup(t)
	for _, path := range []string{"/api/v1/tickets", "/api/v1/tickets/1", "/api/v1/tickets/1/comments"} {
		expect(t, do(h, "GET", path, "", nil), 401, "GET "+path)
	}
	expect(t, do(h, "POST", "/api/v1/tickets", `{"title":"x"}`, nil), 401, "POST tickets")
}

func TestCreateAndVisibility(t *testing.T) {
	h, pool := setup(t)
	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	bob := newActor(t, h, pool, "bob@x.test", "end_user")
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	w := alice.do(h, "POST", "/api/v1/tickets", `{"title":"  Laptop won't boot ","description":"black screen"}`)
	expect(t, w, 201, "alice creates")
	tk := decodeBody[ticket](t, w)
	if tk.Title != "Laptop won't boot" || tk.Status != "new" || tk.Priority != 3 || tk.Requester != alice.id || tk.Number < 1 {
		t.Errorf("unexpected ticket %+v", tk)
	}
	if w.Header().Get("Location") == "" {
		t.Error("missing Location header")
	}

	// Read by uuid and by number.
	expect(t, alice.do(h, "GET", "/api/v1/tickets/"+tk.ID, ""), 200, "alice GET by id")
	expect(t, alice.do(h, "GET", fmt.Sprintf("/api/v1/tickets/%d", tk.Number), ""), 200, "alice GET by number")
	// Bob can't see it, and can't tell it exists.
	expect(t, bob.do(h, "GET", "/api/v1/tickets/"+tk.ID, ""), 404, "bob GET")
	expect(t, bob.do(h, "GET", fmt.Sprintf("/api/v1/tickets/%d", tk.Number), ""), 404, "bob GET by number")
	expect(t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID, ""), 200, "agent GET")
	expect(t, alice.do(h, "GET", "/api/v1/tickets/nonsense", ""), 404, "bad id")

	if l := decodeBody[list](t, bob.do(h, "GET", "/api/v1/tickets", "")); len(l.Items) != 0 {
		t.Errorf("bob sees %d tickets", len(l.Items))
	}
	if l := decodeBody[list](t, agent.do(h, "GET", "/api/v1/tickets", "")); len(l.Items) != 1 {
		t.Errorf("agent sees %d tickets", len(l.Items))
	}
}

func TestCreateValidationAndOnBehalf(t *testing.T) {
	h, pool := setup(t)
	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	bob := newActor(t, h, pool, "bob@x.test", "end_user")
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	for name, body := range map[string]string{
		"empty title":   `{"title":"   "}`,
		"long title":    fmt.Sprintf(`{"title":%q}`, strings.Repeat("a", 201)),
		"priority zero": `{"title":"x","priority":0}`,
		"priority high": `{"title":"x","priority":9}`,
		"unknown user":  `{"title":"x","requester_id":"00000000-0000-0000-0000-000000000000"}`,
	} {
		expect(t, agent.do(h, "POST", "/api/v1/tickets", body), 422, name)
	}
	expect(t, alice.do(h, "POST", "/api/v1/tickets", `{"title":"x","bogus":1}`), 400, "unknown field")

	// End users cannot file on behalf of someone else; staff can.
	expect(t, alice.do(h, "POST", "/api/v1/tickets", fmt.Sprintf(`{"title":"spoof","requester_id":%q}`, bob.id)), 403, "spoof")
	w := agent.do(h, "POST", "/api/v1/tickets", fmt.Sprintf(`{"title":"phoned in","requester_id":%q}`, bob.id))
	expect(t, w, 201, "agent on behalf")
	if tk := decodeBody[ticket](t, w); tk.Requester != bob.id {
		t.Errorf("requester = %s, want bob", tk.Requester)
	}
	// Bob now sees the ticket filed for him.
	if l := decodeBody[list](t, bob.do(h, "GET", "/api/v1/tickets", "")); len(l.Items) != 1 {
		t.Errorf("bob sees %d tickets", len(l.Items))
	}
}

func TestListFiltersAndPaging(t *testing.T) {
	h, pool := setup(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	for i, title := range []string{"VPN down", "Printer 100% jammed", "printer_offline"} {
		w := agent.do(h, "POST", "/api/v1/tickets", fmt.Sprintf(`{"title":%q,"priority":%d}`, title, i+1))
		expect(t, w, 201, "seed")
	}
	get := func(qs string) list {
		w := agent.do(h, "GET", "/api/v1/tickets"+qs, "")
		expect(t, w, 200, "GET "+qs)
		return decodeBody[list](t, w)
	}

	first := get("?limit=2")
	if len(first.Items) != 2 || !first.HasMore {
		t.Errorf("page 1: %d items, has_more=%v", len(first.Items), first.HasMore)
	}
	second := get("?limit=2&offset=2")
	if len(second.Items) != 1 || second.HasMore {
		t.Errorf("page 2: %d items, has_more=%v", len(second.Items), second.HasMore)
	}
	if first.Items[0].Number <= first.Items[1].Number {
		t.Error("expected newest first")
	}
	if n := len(get("?priority=2").Items); n != 1 {
		t.Errorf("priority filter: %d", n)
	}
	if n := len(get("?q=printer").Items); n != 2 {
		t.Errorf("search: %d", n)
	}
	// LIKE wildcards in the search term are literal.
	if n := len(get("?q=100%25").Items); n != 1 {
		t.Errorf("percent search: %d", n)
	}
	if n := len(get("?q=%25").Items); n != 1 {
		t.Errorf("bare percent should match only the ticket containing it: %d", n)
	}
	if n := len(get("?q=printer_").Items); n != 1 {
		t.Errorf("underscore search: %d", n)
	}
	if n := len(get("?status=closed").Items); n != 0 {
		t.Errorf("status filter: %d", n)
	}
	if n := len(get("?requester=me").Items); n != 3 {
		t.Errorf("requester=me: %d", n)
	}
	if n := len(get("?assignee=me").Items); n != 0 {
		t.Errorf("assignee=me: %d", n)
	}
	if n := len(get("?assignee=none").Items); n != 3 {
		t.Errorf("assignee=none: %d", n)
	}
	if n := len(get("?open=true").Items); n != 3 {
		t.Errorf("open=true: %d", n)
	}
	all := get("")
	expect(t, agent.do(h, "POST", "/api/v1/tickets/"+all.Items[0].ID+"/transitions", `{"to":"resolved"}`), 200, "resolve one")
	if n := len(get("?open=true").Items); n != 2 {
		t.Errorf("open=true after resolving one: %d", n)
	}
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+all.Items[1].ID, fmt.Sprintf(`{"assignee_id":%q}`, agent.id)), 200, "assign one")
	if n := len(get("?assignee=none").Items); n != 2 {
		t.Errorf("assignee=none after assigning one: %d", n)
	}
	for _, qs := range []string{"?limit=0", "?limit=101", "?offset=-1", "?offset=4294967296", "?status=bogus", "?priority=7", "?assignee=xyz"} {
		expect(t, agent.do(h, "GET", "/api/v1/tickets"+qs, ""), 400, qs)
	}
}

func TestUpdateTicket(t *testing.T) {
	h, pool := setup(t)
	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, alice.do(h, "POST", "/api/v1/tickets", `{"title":"orig"}`))
	path := "/api/v1/tickets/" + tk.ID

	expect(t, alice.do(h, "PATCH", path, `{"title":"hacked"}`), 403, "end user PATCH")

	w := agent.do(h, "PATCH", path, fmt.Sprintf(`{"priority":1,"assignee_id":%q}`, agent.id))
	expect(t, w, 200, "agent PATCH")
	got := decodeBody[ticket](t, w)
	if got.Priority != 1 || got.AssigneeID == nil || *got.AssigneeID != agent.id || got.Title != "orig" {
		t.Errorf("after patch: %+v", got)
	}
	if n := len(decodeBody[list](t, agent.do(h, "GET", "/api/v1/tickets?assignee=me", "")).Items); n != 1 {
		t.Errorf("assignee=me after assign: %d", n)
	}

	// Absent assignee_id leaves it alone; null clears it.
	got = decodeBody[ticket](t, agent.do(h, "PATCH", path, `{"title":"renamed"}`))
	if got.AssigneeID == nil || got.Title != "renamed" {
		t.Errorf("assignee should survive unrelated patch: %+v", got)
	}
	got = decodeBody[ticket](t, agent.do(h, "PATCH", path, `{"assignee_id":null}`))
	if got.AssigneeID != nil {
		t.Errorf("assignee not cleared: %+v", got)
	}

	expect(t, agent.do(h, "PATCH", path, fmt.Sprintf(`{"assignee_id":%q}`, alice.id)), 422, "assign to end user")
	expect(t, agent.do(h, "PATCH", path, `{"assignee_id":"00000000-0000-0000-0000-000000000000"}`), 422, "assign to nobody")
	expect(t, agent.do(h, "PATCH", path, `{"assignee_id":"nope"}`), 400, "malformed assignee")
	expect(t, agent.do(h, "PATCH", path, `{"priority":5}`), 422, "bad priority")
	expect(t, agent.do(h, "PATCH", path, `{"title":""}`), 422, "empty title")
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/00000000-0000-0000-0000-000000000000", `{"title":"x"}`), 404, "missing ticket")
}

func TestTransitions(t *testing.T) {
	h, pool := setup(t)
	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, alice.do(h, "POST", "/api/v1/tickets", `{"title":"t"}`))
	path := "/api/v1/tickets/" + tk.ID + "/transitions"
	move := func(to string) *httptest.ResponseRecorder {
		return agent.do(h, "POST", path, fmt.Sprintf(`{"to":%q}`, to))
	}

	expect(t, alice.do(h, "POST", path, `{"to":"closed"}`), 403, "end user transition")
	expect(t, move("bogus"), 422, "unknown status")
	expect(t, move("new"), 409, "new -> new")

	w := move("in_progress")
	expect(t, w, 200, "new -> in_progress")
	if decodeBody[ticket](t, w).Status != "in_progress" {
		t.Error("status not updated")
	}
	expect(t, move("closed"), 409, "in_progress -> closed skips resolved")
	expect(t, move("resolved"), 200, "-> resolved")
	expect(t, move("in_progress"), 200, "reopen")
	expect(t, move("resolved"), 200, "-> resolved again")
	expect(t, move("closed"), 200, "-> closed")
	for _, to := range []string{"new", "in_progress", "on_hold", "resolved", "closed"} {
		expect(t, move(to), 409, "closed -> "+to)
	}
	// The end user still sees the final state.
	if got := decodeBody[ticket](t, alice.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")); got.Status != "closed" {
		t.Errorf("status = %s", got.Status)
	}
}

type comment struct {
	ID       string `json:"id"`
	AuthorID string `json:"author_id"`
	Body     string `json:"body"`
	Internal bool   `json:"internal"`
	Author   string `json:"author_name"`
	Behalf   string `json:"on_behalf_of_name"`
}

func TestComments(t *testing.T) {
	h, pool := setup(t)
	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	bob := newActor(t, h, pool, "bob@x.test", "end_user")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, alice.do(h, "POST", "/api/v1/tickets", `{"title":"t"}`))
	path := "/api/v1/tickets/" + tk.ID + "/comments"

	w := alice.do(h, "POST", path, `{"body":"still broken"}`)
	expect(t, w, 201, "requester comments")
	if c := decodeBody[comment](t, w); c.AuthorID != alice.id || c.Internal {
		t.Errorf("comment %+v", c)
	}
	expect(t, agent.do(h, "POST", path, `{"body":"looking into it"}`), 201, "agent public")
	expect(t, agent.do(h, "POST", path, `{"body":"user is on the old firmware","internal":true}`), 201, "agent internal")

	expect(t, alice.do(h, "POST", path, `{"body":"psst","internal":true}`), 403, "end user internal")
	expect(t, alice.do(h, "POST", path, `{"body":"   "}`), 422, "blank body")
	expect(t, alice.do(h, "POST", path, `{"body":"x","author_id":"a"}`), 400, "cannot set author")
	// Bob can neither read nor write on a ticket he cannot see.
	expect(t, bob.do(h, "GET", path, ""), 404, "bob list")
	expect(t, bob.do(h, "POST", path, `{"body":"hi"}`), 404, "bob post")

	type comments struct {
		Items []comment `json:"items"`
	}
	if got := decodeBody[comments](t, alice.do(h, "GET", path, "")); len(got.Items) != 2 {
		t.Errorf("requester sees %d comments, want 2 (internal note hidden)", len(got.Items))
	}
	got := decodeBody[comments](t, agent.do(h, "GET", path, ""))
	if len(got.Items) != 3 || got.Items[0].Body != "still broken" || !got.Items[2].Internal {
		t.Errorf("agent view: %+v", got.Items)
	}
}

func TestNamesAreIncluded(t *testing.T) {
	h, pool := setup(t)
	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, alice.do(h, "POST", "/api/v1/tickets", `{"title":"t"}`))
	if tk.RequesterName != "alice@x.test" || tk.AssigneeName != nil {
		t.Errorf("create: %+v", tk)
	}
	got := decodeBody[ticket](t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, fmt.Sprintf(`{"assignee_id":%q}`, agent.id)))
	if got.AssigneeName == nil || *got.AssigneeName != "agent@x.test" {
		t.Errorf("assignee name: %+v", got)
	}
	// The requester sees the agent's name on their own ticket, and on the list.
	if l := decodeBody[list](t, alice.do(h, "GET", "/api/v1/tickets", "")); len(l.Items) != 1 || l.Items[0].AssigneeName == nil {
		t.Errorf("list names: %+v", l.Items)
	}
	c := decodeBody[comment](t, agent.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/comments", `{"body":"hi"}`))
	if c.Author != "agent@x.test" {
		t.Errorf("comment author: %+v", c)
	}
}

func TestStaffList(t *testing.T) {
	h, pool := setup(t)
	alice := newActor(t, h, pool, "alice@x.test", "end_user")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	expect(t, alice.do(h, "GET", "/api/v1/users/staff", ""), 403, "end user")
	expect(t, do(h, "GET", "/api/v1/users/staff", "", nil), 401, "anonymous")
	w := agent.do(h, "GET", "/api/v1/users/staff", "")
	expect(t, w, 200, "agent")
	var got struct {
		Items []struct{ Name, Role string } `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// The bootstrap admin plus the agent; not the end user.
	if len(got.Items) != 2 {
		t.Errorf("staff = %+v", got.Items)
	}
}
