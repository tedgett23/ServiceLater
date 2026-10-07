package api_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db"
)

// Every access check in SQL names a permission. A policy or function that still asks "is this person staff" would
// ignore custom roles and app principals.
func TestNothingChecksRolesInSQL(t *testing.T) {
	_, pool := setup(t)
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT tablename || '.' || policyname FROM pg_policies
		WHERE coalesce(qual, '') ~ 'app_is_|role IN|''agent''' OR coalesce(with_check, '') ~ 'app_is_|role IN|''agent'''`)
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		bad = append(bad, s)
	}
	if len(bad) > 0 {
		t.Errorf("policies that check a role instead of a permission: %v", bad)
	}
	rows, err = pool.Query(ctx, `SELECT proname FROM pg_proc WHERE pronamespace = 'public'::regnamespace
		AND proname NOT IN ('app_is_staff', 'app_is_admin', 'app_seed_builtin_roles')
		AND (prosrc ~ 'app_is_(staff|admin)' OR prosrc ~ 'role IN \(''admin'', ''agent''\)')`)
	if err != nil {
		t.Fatal(err)
	}
	bad = nil
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		bad = append(bad, s)
	}
	if len(bad) > 0 {
		t.Errorf("functions that check a role instead of a permission: %v", bad)
	}
}

// The built-in roles use only permissions the platform knows, and admin has all of them.
func TestBuiltInRolesMatchTheCatalog(t *testing.T) {
	_, pool := setup(t)
	rows, err := pool.Query(context.Background(), "SELECT name, permissions FROM roles WHERE app_name IS NULL")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for rows.Next() {
		var name string
		var perms []string
		if err := rows.Scan(&name, &perms); err != nil {
			t.Fatal(err)
		}
		got[name] = perms
		for _, p := range perms {
			if !authz.Known(p) {
				t.Errorf("role %s has %q, which is not in authz.Catalog", name, p)
			}
		}
	}
	if len(got) != 3 || len(got["end_user"]) != 0 {
		t.Errorf("built-in roles: %v", got)
	}
	for _, p := range authz.Catalog {
		if !slices.Contains(got["admin"], p.Name) {
			t.Errorf("admin lacks %s", p.Name)
		}
	}
}

// /auth/me lists what each base role may do, which is what the web shows and hides things by.
func TestMeListsPermissions(t *testing.T) {
	h, pool := setup(t)
	for role, want := range map[string]string{"admin": authz.UsersManage, "agent": authz.TicketsUpdate} {
		a := newActor(t, h, pool, role+"@x.test", role)
		me := decodeBody[struct{ Permissions []string }](t, a.do(h, "GET", "/api/v1/auth/me", ""))
		if !slices.Contains(me.Permissions, want) {
			t.Errorf("%s: %v", role, me.Permissions)
		}
	}
	u := newActor(t, h, pool, "user@x.test", "end_user")
	if me := decodeBody[struct{ Permissions []string }](t, u.do(h, "GET", "/api/v1/auth/me", "")); len(me.Permissions) != 0 {
		t.Errorf("an end user: %v", me.Permissions)
	}
}

// A role granted to a group gives its members the permissions, in Go and in RLS alike, and leaving the group takes
// them away. This is the path custom roles and app roles take.
func TestARoleGrantedToAGroupGivesItsMembersAccess(t *testing.T) {
	h, pool := setup(t)
	ctx := context.Background()
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	marcus := newActor(t, h, pool, "marcus@x.test", "end_user")
	theirs := decodeBody[ticket](t, marcus.do(h, "POST", "/api/v1/tickets", `{"title":"Marcus's laptop"}`))

	if _, err := pool.Exec(ctx, `INSERT INTO roles (name, label, permissions) VALUES ('triage', 'Triage', '{tickets:read_all,tickets:update}')`); err != nil {
		t.Fatal(err)
	}
	g := decodeBody[group](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Triage desk"}`))
	if _, err := pool.Exec(ctx, `INSERT INTO role_grants (role, group_id) VALUES ('triage', $1)`, g.ID); err != nil {
		t.Fatal(err)
	}

	expect(t, lena.do(h, "GET", "/api/v1/tickets/"+theirs.ID, ""), 404, "before joining the group")
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+g.ID+"/members/"+lena.id, ""), 204, "joining the group")
	expect(t, lena.do(h, "GET", "/api/v1/tickets/"+theirs.ID, ""), 200, "seeing someone else's ticket through the role")
	expect(t, lena.do(h, "PATCH", "/api/v1/tickets/"+theirs.ID, `{"priority":2}`), 200, "editing it through the role")
	me := decodeBody[struct{ Permissions []string }](t, lena.do(h, "GET", "/api/v1/auth/me", ""))
	if !slices.Contains(me.Permissions, authz.TicketsUpdate) {
		t.Errorf("me: %v", me.Permissions)
	}
	// The role can be assigned tickets now, like any agent.
	staff := decodeBody[personList](t, admin.do(h, "GET", "/api/v1/users/staff", ""))
	if !listed(staff, lena.id) {
		t.Error("someone who may work tickets is not offered as an assignee")
	}
	// It gives what it says and nothing more: no internal notes.
	if w := lena.do(h, "POST", "/api/v1/tickets/"+theirs.ID+"/comments", `{"body":"psst","internal":true}`); w.Code != 403 {
		t.Errorf("internal note without comments:internal: %d %s", w.Code, w.Body)
	}

	expect(t, admin.do(h, "DELETE", "/api/v1/groups/"+g.ID+"/members/"+lena.id, ""), 204, "leaving the group")
	expect(t, lena.do(h, "GET", "/api/v1/tickets/"+theirs.ID, ""), 404, "after leaving the group")
	if w := lena.do(h, "PATCH", "/api/v1/tickets/"+theirs.ID, `{"priority":1}`); w.Code != 403 {
		t.Errorf("editing after leaving: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
}

type role struct {
	Name        string   `json:"name"`
	App         string   `json:"app"`
	BuiltIn     bool     `json:"built_in"`
	Permissions []string `json:"permissions"`
	Grants      int      `json:"grants"`
	GrantedTo   []struct {
		ID      string `json:"id"`
		UserID  string `json:"user_id"`
		GroupID string `json:"group_id"`
	} `json:"granted_to"`
}

// An admin grants a shipped role (core-itsm's ticket-viewer) to a person, which gives exactly what it says; revoking
// it takes it back. Only roles:manage may grant, and base roles are not grantable.
func TestGrantingAndRevokingARole(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	marcus := newActor(t, h, pool, "marcus@x.test", "end_user")
	theirs := decodeBody[ticket](t, marcus.do(h, "POST", "/api/v1/tickets", `{"title":"Marcus's"}`))

	roles := decodeBody[struct{ Items []role }](t, admin.do(h, "GET", "/api/v1/roles", ""))
	var names []string
	for _, r := range roles.Items {
		names = append(names, r.Name)
		if (r.Name == "admin" && !r.BuiltIn) || (r.Name == "ticket-viewer" && (r.BuiltIn || r.App != "core-itsm")) {
			t.Errorf("role: %+v", r)
		}
	}
	if !slices.Contains(names, "ticket-viewer") || !slices.Contains(names, "agent") {
		t.Fatalf("roles: %v", names)
	}
	expect(t, agent.do(h, "GET", "/api/v1/roles", ""), 403, "an agent listing roles")
	expect(t, agent.do(h, "POST", "/api/v1/roles/ticket-viewer/grants", `{"user_id":"`+lena.id+`"}`), 403, "an agent granting a role")
	expect(t, admin.do(h, "POST", "/api/v1/roles/agent/grants", `{"user_id":"`+lena.id+`"}`), 422, "granting a base role")
	expect(t, admin.do(h, "POST", "/api/v1/roles/ticket-viewer/grants", `{}`), 422, "granting to nobody")
	expect(t, admin.do(h, "POST", "/api/v1/roles/nope/grants", `{"user_id":"`+lena.id+`"}`), 404, "an unknown role")

	w := admin.do(h, "POST", "/api/v1/roles/ticket-viewer/grants", `{"user_id":"`+lena.id+`"}`)
	expect(t, w, 201, "granting ticket-viewer")
	grant := decodeBody[struct{ ID string }](t, w)
	expect(t, admin.do(h, "POST", "/api/v1/roles/ticket-viewer/grants", `{"user_id":"`+lena.id+`"}`), 409, "granting it twice")

	expect(t, lena.do(h, "GET", "/api/v1/tickets/"+theirs.ID, ""), 200, "a viewer reading someone else's ticket")
	expect(t, lena.do(h, "PATCH", "/api/v1/tickets/"+theirs.ID, `{"priority":1}`), 403, "a viewer editing it")
	held := decodeBody[struct {
		Roles []struct{ Role, GroupName string } `json:"roles"`
	}](t, admin.do(h, "GET", "/api/v1/users/"+lena.id, ""))
	if len(held.Roles) != 1 || held.Roles[0].Role != "ticket-viewer" {
		t.Errorf("user detail roles: %+v", held.Roles)
	}
	if r := decodeBody[role](t, admin.do(h, "GET", "/api/v1/roles/ticket-viewer", "")); len(r.GrantedTo) != 1 || r.GrantedTo[0].UserID != lena.id {
		t.Errorf("role detail: %+v", r)
	}

	expect(t, admin.do(h, "DELETE", "/api/v1/roles/ticket-viewer/grants/"+grant.ID, ""), 204, "revoking")
	expect(t, lena.do(h, "GET", "/api/v1/tickets/"+theirs.ID, ""), 404, "after revoking")
	expect(t, admin.do(h, "DELETE", "/api/v1/roles/ticket-viewer/grants/"+grant.ID, ""), 404, "revoking twice")
}

// group-manager runs groups, but cannot change who is in a group that holds a role (its members get the role), and
// a people manager cannot make anyone, themselves included, an admin.
func TestManagersCannotEscalate(t *testing.T) {
	h, pool, reg := setupApps(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	gm := newActor(t, h, pool, "gm@x.test", "end_user")
	pm := newActor(t, h, pool, "pm@x.test", "end_user")
	installNotifyApp(t, pool, reg, map[string]string{"roles/people.yaml": "kind: role\nname: people-manager\nlabel: People\npermissions: [users:read, users:manage]\n"})
	expect(t, admin.do(h, "POST", "/api/v1/roles/group-manager/grants", `{"user_id":"`+gm.id+`"}`), 201, "granting group-manager")
	expect(t, admin.do(h, "POST", "/api/v1/roles/people-manager/grants", `{"user_id":"`+pm.id+`"}`), 201, "granting people-manager")

	plain := decodeBody[group](t, gm.do(h, "POST", "/api/v1/groups", `{"name":"Floor 2"}`))
	expect(t, gm.do(h, "PUT", "/api/v1/groups/"+plain.ID+"/members/"+gm.id, ""), 204, "joining a plain group")
	powerful := decodeBody[group](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Viewers"}`))
	expect(t, admin.do(h, "POST", "/api/v1/roles/ticket-viewer/grants", `{"group_id":"`+powerful.ID+`"}`), 201, "granting a role to a group")
	w := gm.do(h, "PUT", "/api/v1/groups/"+powerful.ID+"/members/"+gm.id, "")
	expect(t, w, 403, "a group manager joining a group that holds a role")
	if !strings.Contains(w.Body.String(), "roles:manage") {
		t.Errorf("says why: %s", w.Body)
	}
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+powerful.ID+"/members/"+pm.id, ""), 204, "an admin adding someone to it")
	expect(t, gm.do(h, "DELETE", "/api/v1/groups/"+powerful.ID+"/members/"+pm.id, ""), 403, "a group manager removing someone from it")

	expect(t, pm.do(h, "PATCH", "/api/v1/users/"+pm.id, `{"role":"admin"}`), 403, "a people manager making themselves an admin")
	expect(t, pm.do(h, "POST", "/api/v1/users", `{"email":"new@x.test","name":"New","role":"admin","password":"long-enough"}`), 403, "a people manager creating an admin")
	expect(t, pm.do(h, "PATCH", "/api/v1/users/"+gm.id, `{"role":"agent"}`), 200, "a people manager making someone an agent")
	// And RLS says the same if Go is bypassed.
	err := db.WithUser(context.Background(), pool, uuid.MustParse(pm.id), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "UPDATE users SET role = 'admin' WHERE id = app_current_user_id()")
		return err
	})
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "42501" {
		t.Errorf("RLS let a people manager become an admin: %v", err)
	}
}

// change-management's cab-member role, asked through role(): whoever holds it, directly or through a group, is asked;
// a deactivated holder is not.
func TestApprovalAsksARolesHolders(t *testing.T) {
	h, pool, reg := setupApps(t)
	ctx := context.Background()
	b, err := apps.LoadBundled("change-management")
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Install(ctx, pool, c, apps.InstallOptions{ApprovePermissions: true}); err != nil {
		t.Fatal(err)
	}
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	ann := newActor(t, h, pool, "ann@x.test", "end_user")
	bob := newActor(t, h, pool, "bob@x.test", "end_user")
	gone := newActor(t, h, pool, "gone@x.test", "end_user")
	board := decodeBody[group](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Board"}`))
	expect(t, admin.do(h, "POST", "/api/v1/roles/cab-member/grants", `{"group_id":"`+board.ID+`"}`), 201, "granting cab-member to a group")
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+board.ID+"/members/"+ann.id, ""), 204, "ann joins")
	expect(t, admin.do(h, "POST", "/api/v1/roles/cab-member/grants", `{"user_id":"`+bob.id+`"}`), 201, "granting it to bob")
	expect(t, admin.do(h, "POST", "/api/v1/roles/cab-member/grants", `{"user_id":"`+gone.id+`"}`), 201, "granting it to gone")
	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+gone.id, `{"deactivated":true}`), 200, "deactivating gone")

	installFlows(t, pool, reg, map[string]string{"cab": wfSrc("cab", "ask",
		"  ask:\n    type: approval\n    approvers: \"role('cab-member')\"\n    rule: all\n    next: done\n  done:\n    type: end\n")}, nil)
	startFlowWorker(t, pool, reg)
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"swap the core switch"}`))
	agent.do(h, "POST", "/api/v1/workflow-runs", `{"workflow":"house-flows/cab","ticket":"`+tk.ID+`"}`)
	for _, a := range []actor{ann, bob} {
		waitFor(t, "the request to reach "+a.id, func() bool {
			return len(decodeBody[approvalList](t, a.do(h, "GET", "/api/v1/approvals", "")).Items) == 1
		})
	}
	var asked int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM workflow_approvals").Scan(&asked); err != nil || asked != 2 {
		t.Errorf("asked %d people (want ann and bob, not the deactivated one): %v", asked, err)
	}
}
