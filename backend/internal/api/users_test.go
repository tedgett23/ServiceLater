package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
)

type person struct {
	ID            string  `json:"id"`
	Email         string  `json:"email"`
	Name          string  `json:"name"`
	Role          string  `json:"role"`
	DeactivatedAt *string `json:"deactivated_at"`
	Groups        []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"groups"`
}

type personList struct {
	Items []person `json:"items"`
}

func login(h http.Handler, email, password string) *httptest.ResponseRecorder {
	return do(h, "POST", "/api/v1/auth/login", fmt.Sprintf(`{"email":%q,"password":%q}`, email, password), nil)
}

func listed(l personList, id string) bool {
	for _, p := range l.Items {
		if p.ID == id {
			return true
		}
	}
	return false
}

// Only admins manage people; agents read the directory, end users not even that.
func TestOnlyAdminsManageUsers(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	user := newActor(t, h, pool, "user@x.test", "end_user")

	for _, a := range []actor{agent, user} {
		expect(t, a.do(h, "POST", "/api/v1/users", `{"email":"n@x.test","name":"N","password":"long-enough"}`), 403, "creating a user")
		expect(t, a.do(h, "GET", "/api/v1/users/"+user.id, ""), 403, "reading a user")
		expect(t, a.do(h, "PATCH", "/api/v1/users/"+user.id, `{"role":"admin"}`), 403, "promoting someone")
		expect(t, a.do(h, "PUT", "/api/v1/users/"+user.id+"/password", `{"password":"long-enough"}`), 403, "setting a password")
		expect(t, a.do(h, "DELETE", "/api/v1/users/"+user.id, ""), 403, "deleting someone")
	}
	expect(t, admin.do(h, "GET", "/api/v1/users/"+user.id, ""), 200, "an admin reading a user")
	expect(t, admin.do(h, "GET", "/api/v1/users/00000000-0000-0000-0000-000000000000", ""), 404, "nobody")
	expect(t, admin.do(h, "GET", "/api/v1/users/staff", ""), 200, "the staff route is not an id")
}

func TestAdminCreatesAndEditsUsers(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")

	w := admin.do(h, "POST", "/api/v1/users", `{"email":"  Nia@X.test ","name":" Nia Obi ","password":"first-password"}`)
	expect(t, w, 201, "creating a user")
	nia := decodeBody[person](t, w)
	if nia.Email != "nia@x.test" || nia.Name != "Nia Obi" || nia.Role != "end_user" || nia.DeactivatedAt != nil {
		t.Errorf("created: %+v", nia)
	}
	if loc := w.Header().Get("Location"); loc != "/api/v1/users/"+nia.ID {
		t.Errorf("Location = %q", loc)
	}
	if r := login(h, "nia@x.test", "first-password"); r.Code != 200 {
		t.Fatalf("the new user signs in: %d", r.Code)
	}

	for body, what := range map[string]string{
		`{"email":"NIA@x.test","name":"Again","password":"first-password"}`: "the same email in another case",
	} {
		expect(t, admin.do(h, "POST", "/api/v1/users", body), 409, what)
	}
	for body, what := range map[string]string{
		`{"email":"nope","name":"N","password":"first-password"}`:                "not an email",
		`{"email":"a@x.test","name":"  ","password":"first-password"}`:           "a blank name",
		`{"email":"a@x.test","name":"N","password":"short"}`:                     "a short password",
		`{"email":"a@x.test","name":"N","password":"first-password","role":"x"}`: "an unknown role",
	} {
		expect(t, admin.do(h, "POST", "/api/v1/users", body), 422, what)
	}

	// Making her an agent puts her in the assignee picker.
	w = admin.do(h, "PATCH", "/api/v1/users/"+nia.ID, `{"name":"Nia Obi-Ross","role":"agent","email":"nia.or@x.test"}`)
	expect(t, w, 200, "editing")
	if got := decodeBody[person](t, w); got.Name != "Nia Obi-Ross" || got.Role != "agent" || got.Email != "nia.or@x.test" {
		t.Errorf("edited: %+v", got)
	}
	if !listed(decodeBody[personList](t, admin.do(h, "GET", "/api/v1/users/staff", "")), nia.ID) {
		t.Error("a new agent is in the staff list")
	}
	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+nia.ID, `{"email":"admin@x.test"}`), 409, "taking someone else's email")

	// The detail lists her groups, so membership can be managed from either side.
	g := decodeBody[group](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Network"}`))
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+g.ID+"/members/"+nia.ID, ""), 204, "adding her to a group")
	if got := decodeBody[person](t, admin.do(h, "GET", "/api/v1/users/"+nia.ID, "")); len(got.Groups) != 1 || got.Groups[0].Name != "Network" {
		t.Errorf("groups: %+v", got.Groups)
	}

	// A new password works at once and signs her out of the sessions she had.
	r := login(h, "nia.or@x.test", "first-password")
	expect(t, admin.do(h, "PUT", "/api/v1/users/"+nia.ID+"/password", `{"password":"short"}`), 422, "a short password")
	expect(t, admin.do(h, "PUT", "/api/v1/users/"+nia.ID+"/password", `{"password":"second-password"}`), 204, "resetting her password")
	if login(h, "nia.or@x.test", "first-password").Code != 401 || login(h, "nia.or@x.test", "second-password").Code != 200 {
		t.Error("the old password stops working and the new one works")
	}
	if w := do(h, "POST", "/api/v1/auth/refresh", "", r.Result().Cookies()); w.Code != 401 {
		t.Errorf("a session from before the reset is revoked: %d", w.Code)
	}
}

// Deactivating someone stops them signing in, ends their sessions, takes away their power under RLS at once (even
// with an access token still in hand), and drops them from pickers. Reactivating undoes it.
func TestDeactivatedUsersAreLockedOut(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	other := newActor(t, h, pool, "other@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+other.id, `{"deactivated":true}`), 200, "deactivating an admin")
	if r := login(h, "other@x.test", "password-other@x.test"); r.Code != 401 {
		t.Errorf("a deactivated person signs in: %d", r.Code)
	}
	expect(t, do(h, "POST", "/api/v1/auth/refresh", "", other.cookies), 401, "refreshing a deactivated session")
	expect(t, other.do(h, "GET", "/api/v1/auth/me", ""), 401, "me, deactivated")
	// Their access token still passes the role check in Go, but RLS no longer counts them as an admin.
	expect(t, other.do(h, "POST", "/api/v1/groups", `{"name":"Sneaky"}`), 403, "a deactivated admin's leftover token")

	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+agent.id, `{"deactivated":true}`), 200, "deactivating an agent")
	if listed(decodeBody[personList](t, admin.do(h, "GET", "/api/v1/users/staff", "")), agent.id) {
		t.Error("a deactivated agent is offered as an assignee")
	}
	if listed(decodeBody[personList](t, admin.do(h, "GET", "/api/v1/users", "")), agent.id) {
		t.Error("the directory leaves deactivated people out by default")
	}
	all := decodeBody[personList](t, admin.do(h, "GET", "/api/v1/users?include_deactivated=true", ""))
	if !listed(all, agent.id) {
		t.Error("include_deactivated lists them")
	}
	tk := decodeBody[ticket](t, admin.do(h, "POST", "/api/v1/tickets", `{"title":"Printer"}`))
	expect(t, admin.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignee_id":"`+agent.id+`"}`), 422, "assigning a deactivated agent")

	w := admin.do(h, "PATCH", "/api/v1/users/"+agent.id, `{"deactivated":false}`)
	if got := decodeBody[person](t, w); got.DeactivatedAt != nil {
		t.Errorf("reactivated: %+v", got)
	}
	if r := login(h, "agent@x.test", "password-agent@x.test"); r.Code != 200 {
		t.Errorf("a reactivated person signs in: %d", r.Code)
	}
}

func TestAdminsCannotLockThemselvesOut(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+admin.id, `{"role":"agent"}`), 422, "demoting yourself")
	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+admin.id, `{"deactivated":true}`), 422, "deactivating yourself")
	expect(t, admin.do(h, "DELETE", "/api/v1/users/"+admin.id, ""), 422, "deleting yourself")
	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+admin.id, `{"name":"Still Admin","role":"admin"}`), 200, "editing yourself otherwise")
}

// The database keeps one active admin whoever asks, the CLI included; two admins demoting each other at once
// cannot both succeed.
func TestThereIsAlwaysAnActiveAdmin(t *testing.T) {
	_, pool := setup(t)
	err := exec(pool, "UPDATE users SET role = 'agent' WHERE role = 'admin'")
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.ConstraintName != "users_last_admin" {
		t.Errorf("demoting every admin: %v", err)
	}
	if err := exec(pool, "DELETE FROM users WHERE role = 'admin'"); !errors.As(err, &pe) || pe.ConstraintName != "users_last_admin" {
		t.Errorf("deleting every admin: %v", err)
	}
}

// RLS lets people update their own row, but never their own role or deactivation.
func TestNobodyPromotesThemselves(t *testing.T) {
	h, pool := setup(t)
	user := newActor(t, h, pool, "user@x.test", "end_user")
	err := db.WithUser(context.Background(), pool, uuid.MustParse(user.id), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "UPDATE users SET role = 'admin' WHERE id = app_current_user_id()")
		return err
	})
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "42501" {
		t.Errorf("an end user making themselves an admin: %v", err)
	}
	err = db.WithUser(context.Background(), pool, uuid.MustParse(user.id), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "UPDATE users SET name = 'U' WHERE id = app_current_user_id()")
		return err
	})
	if err != nil {
		t.Errorf("their own name is still theirs to change: %v", err)
	}
}

func TestDeletingSomeoneWithHistoryIsRefused(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	fresh := newActor(t, h, pool, "fresh@x.test", "end_user")
	busy := newActor(t, h, pool, "busy@x.test", "end_user")
	expect(t, busy.do(h, "POST", "/api/v1/tickets", `{"title":"Mine"}`), 201, "filing a ticket")
	g := decodeBody[group](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Desk"}`))
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+g.ID+"/members/"+fresh.id, ""), 204, "a membership does not count as history")

	w := admin.do(h, "DELETE", "/api/v1/users/"+busy.id, "")
	expect(t, w, 409, "deleting someone with a ticket")
	if !strings.Contains(w.Body.String(), "deactivate them instead") {
		t.Errorf("says what to do instead: %s", w.Body)
	}
	expect(t, admin.do(h, "DELETE", "/api/v1/users/"+fresh.id, ""), 204, "deleting someone with no history")
	expect(t, admin.do(h, "DELETE", "/api/v1/users/"+fresh.id, ""), 404, "deleting them again")
	if r := login(h, "fresh@x.test", "password-fresh@x.test"); r.Code != 401 {
		t.Errorf("a deleted person signs in: %d", r.Code)
	}
}

func exec(pool *pgxpool.Pool, sql string) error {
	_, err := pool.Exec(context.Background(), sql)
	return err
}
