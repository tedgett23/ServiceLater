package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// withKey calls the API with an API key and no session cookie, as an integration does.
func withKey(h http.Handler, key, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

type keyBody struct {
	ID     string   `json:"id"`
	Key    string   `json:"key"`
	Prefix string   `json:"prefix"`
	Scopes []string `json:"scopes"`
}

// An integration calls the API as a service account, with a key: it files and reads records as that account, under
// the same row-level security as a person; it cannot sign in or be given a password; and a revoked or expired key,
// or a deactivated account, stops working at once.
func TestAServiceAccountCallsTheAPIWithAKey(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	w := admin.do(h, "POST", "/api/v1/service-accounts", `{"name":"Monitoring","description":"Files incidents from alerts. Owner: Ops.","role":"agent"}`)
	expect(t, w, 201, "making a service account")
	sa := decodeBody[struct {
		ID             string `json:"id"`
		Email          string `json:"email"`
		ServiceAccount bool   `json:"service_account"`
	}](t, w)
	if !sa.ServiceAccount || !strings.HasSuffix(sa.Email, "@service.servicelater.invalid") {
		t.Fatalf("the account: %+v", sa)
	}
	refusal(t, agent.do(h, "POST", "/api/v1/service-accounts", `{"name":"Mine"}`), 403, "", "an agent making one")
	refusal(t, admin.do(h, "PUT", "/api/v1/users/"+sa.ID+"/password", `{"password":"hunter2hunter2"}`), 422, "API keys, not a password", "giving it a password")
	if staff := agent.do(h, "GET", "/api/v1/users/staff", "").Body.String(); strings.Contains(staff, sa.ID) {
		t.Error("a service account is offered as an assignee")
	}
	if list := admin.do(h, "GET", "/api/v1/users?q=monitoring", "").Body.String(); !strings.Contains(list, `"service_account":true`) {
		t.Errorf("the user list does not mark it: %s", list)
	}
	refusal(t, admin.do(h, "POST", "/api/v1/users/"+agent.id+"/api-keys", `{"name":"x"}`), 422, "belong to service accounts", "a key for a person")

	w = admin.do(h, "POST", "/api/v1/users/"+sa.ID+"/api-keys", `{"name":"Alertmanager"}`)
	expect(t, w, 201, "making a key")
	key := decodeBody[keyBody](t, w)
	if !strings.HasPrefix(key.Key, "slk_") || !strings.HasPrefix(key.Key, key.Prefix) || len(key.Key) < 50 {
		t.Fatalf("the key: %+v", key)
	}
	if listed := admin.do(h, "GET", "/api/v1/users/"+sa.ID+"/api-keys", "").Body.String(); strings.Contains(listed, key.Key) || !strings.Contains(listed, key.Prefix) {
		t.Errorf("the key list: %s", listed)
	}

	me := decodeBody[struct {
		ID string `json:"id"`
	}](t, withKey(h, key.Key, "GET", "/api/v1/auth/me", ""))
	if me.ID != sa.ID {
		t.Errorf("the key acts as %s", me.ID)
	}
	w = withKey(h, key.Key, "POST", "/api/v1/tickets", `{"title":"Disk 95% full on db-1"}`)
	expect(t, w, 201, "filing with the key")
	inc := decodeBody[problemBody](t, w)
	if got := decodeBody[struct {
		RequesterName string `json:"requester_name"`
	}](t, agent.do(h, "GET", "/api/v1/tickets/"+inc.ID, "")); got.RequesterName != "Monitoring" {
		t.Errorf("filed as %q", got.RequesterName)
	}
	expect(t, withKey(h, key.Key, "GET", "/api/v1/tickets", ""), 200, "reading the queue with the key")
	refusal(t, withKey(h, key.Key, "GET", "/api/v1/users/"+agent.id, ""), 403, "", "doing what its account may not")
	if n := countRows(t, pool, "SELECT count(*) FROM api_keys WHERE id = $1 AND last_used_at IS NOT NULL", key.ID); n != 1 {
		t.Error("the key's use was not recorded")
	}
	expect(t, withKey(h, "slk_nonsense", "GET", "/api/v1/tickets", ""), 401, "a made-up key")
	expect(t, withKey(h, "not-a-key", "GET", "/api/v1/tickets", ""), 401, "something else as a bearer")

	// Revoked, expired, deactivated: each stops it at once.
	k2 := decodeBody[keyBody](t, admin.do(h, "POST", "/api/v1/users/"+sa.ID+"/api-keys", `{"name":"Spare"}`))
	expect(t, admin.do(h, "DELETE", "/api/v1/users/"+sa.ID+"/api-keys/"+k2.ID, ""), 204, "revoking")
	expect(t, withKey(h, k2.Key, "GET", "/api/v1/tickets", ""), 401, "a revoked key")
	refusal(t, admin.do(h, "POST", "/api/v1/users/"+sa.ID+"/api-keys", `{"name":"Old","expires_at":"2020-01-01T00:00:00Z"}`), 422, "in the future", "a key that has already expired")
	k3 := decodeBody[keyBody](t, admin.do(h, "POST", "/api/v1/users/"+sa.ID+"/api-keys",
		`{"name":"Short","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`))
	expect(t, withKey(h, k3.Key, "GET", "/api/v1/tickets", ""), 200, "a key before it expires")
	if _, err := pool.Exec(context.Background(), "UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1", k3.ID); err != nil {
		t.Fatal(err)
	}
	expect(t, withKey(h, k3.Key, "GET", "/api/v1/tickets", ""), 401, "an expired key")
	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+sa.ID, `{"deactivated":true}`), 200, "deactivating the account")
	expect(t, withKey(h, key.Key, "GET", "/api/v1/tickets", ""), 401, "a key of a deactivated account")
}

// A key narrowed to some permissions can do only those, in Go and in the database alike; nobody can make a key
// that could do more than they can.
func TestANarrowedKeyDoesOnlyWhatItIsNarrowedTo(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	sa := decodeBody[struct {
		ID string `json:"id"`
	}](t, admin.do(h, "POST", "/api/v1/service-accounts", `{"name":"Alert bridge","role":"agent"}`))
	change := decodeBody[problemBody](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Patch the firewall"}`))

	refusal(t, admin.do(h, "POST", "/api/v1/users/"+sa.ID+"/api-keys", `{"name":"x","scopes":["roles:manage"]}`), 422, "does not have roles:manage", "a scope the account lacks")
	refusal(t, admin.do(h, "POST", "/api/v1/users/"+sa.ID+"/api-keys", `{"name":"x","scopes":[]}`), 422, "at least one", "no scopes at all")
	w := admin.do(h, "POST", "/api/v1/users/"+sa.ID+"/api-keys", `{"name":"Incidents only","scopes":["records:read:incident","records:create:incident"]}`)
	expect(t, w, 201, "a narrowed key")
	key := decodeBody[keyBody](t, w).Key

	inc := decodeBody[problemBody](t, withKey(h, key, "POST", "/api/v1/tickets", `{"title":"Link down"}`))
	expect(t, withKey(h, key, "GET", "/api/v1/tickets/"+inc.ID, ""), 200, "reading an incident")
	refusal(t, withKey(h, key, "PATCH", "/api/v1/tickets/"+inc.ID, `{"priority":1}`), 403, "", "editing (no records:update)")
	refusal(t, withKey(h, key, "POST", "/api/v1/tickets", `{"type":"change","title":"x"}`), 403, "records:create:change", "filing a change")
	// Row-level security narrows too: the change is not there for the key, though its account can see it.
	expect(t, withKey(h, key, "GET", "/api/v1/tickets/"+change.ID, ""), 404, "reading a change")
	if body := withKey(h, key, "GET", "/api/v1/tickets?type=change", "").Body.String(); strings.Contains(body, change.ID) {
		t.Error("the narrowed key lists changes")
	}
	me := decodeBody[struct {
		Permissions []string `json:"permissions"`
	}](t, withKey(h, key, "GET", "/api/v1/auth/me", ""))
	if !slices.Equal(me.Permissions, []string{"records:create:incident", "records:read:incident"}) {
		t.Errorf("the key's permissions: %v", me.Permissions)
	}

	// Someone who manages people but not roles cannot mint a key for an admin account.
	people := newActor(t, h, pool, "people@x.test", "end_user")
	customRole(t, h, pool, admin, "people-admin", people, "users:manage", "users:read")
	boss := decodeBody[struct {
		ID string `json:"id"`
	}](t, admin.do(h, "POST", "/api/v1/service-accounts", `{"name":"Provisioning","role":"admin"}`))
	refusal(t, people.do(h, "POST", "/api/v1/users/"+boss.ID+"/api-keys", `{"name":"x"}`), 403, "something you cannot", "a key stronger than its maker")
	expect(t, people.do(h, "POST", "/api/v1/users/"+boss.ID+"/api-keys", `{"name":"x","scopes":["users:read"]}`), 201, "a key no stronger than its maker")
}

// authz.Narrow (Go) and app_narrow_permissions (SQL) agree.
func TestNarrowingMatchesTheDatabase(t *testing.T) {
	_, pool, _ := setupApps(t)
	perms := []string{"comments:internal", "records:create:incident", "records:read:*", "records:update:change", "users:read"}
	for _, scopes := range [][]string{
		{"records:read:incident"}, {"records:read:*"}, {"records:update:*"}, {"records:update:incident"},
		{"users:read", "roles:manage"}, {"records:create:*", "comments:internal"}, {"nonsense"}, {"records:delete:change"},
	} {
		var fromSQL []string
		if err := db.WithSystem(context.Background(), pool, func(tx pgx.Tx) error {
			var err error
			fromSQL, err = dbgen.New(tx).NarrowPermissions(context.Background(), dbgen.NarrowPermissionsParams{Perms: perms, Scopes: scopes})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got := authz.Narrow(perms, scopes); !slices.Equal([]string(got), fromSQL) {
			t.Errorf("%v: Go %v, SQL %v", scopes, got, fromSQL)
		}
	}
}
