package apps_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
)

const auditor = "kind: role\nname: auditor\nlabel: Auditor\npermissions: [tickets:read_all, users:read]\n"

func roleApp(t *testing.T, name, version string, roles map[string]string) *apps.Compiled {
	t.Helper()
	files := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: " + name + "\nversion: " + version + "\nauthor: t\ndescription: \"Roles.\"\n")}
	for file, src := range roles {
		files["default/roles/"+file] = []byte(src)
	}
	c, err := (&apps.Bundle{Files: files}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func roleProblems(src string) string {
	files := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: bad\nversion: 1.0.0\nauthor: t\ndescription: \"Roles.\"\n"),
		"default/roles/r.yaml": []byte(src)}
	_, err := (&apps.Bundle{Files: files}).Compile()
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestRoleMistakesFailTheInstall(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"built-in name", "kind: role\nname: admin\nlabel: A\npermissions: [users:read]\n", "built-in role"},
		{"unknown permission", "kind: role\nname: odd\nlabel: Odd\npermissions: [tickets:fly]\n", `"tickets:fly" is not a permission`},
		{"twice", "kind: role\nname: odd\nlabel: Odd\npermissions: [users:read, users:read]\n", "listed twice"},
		{"no label", "kind: role\nname: odd\npermissions: [users:read]\n", "label is required"},
		{"no permissions", "kind: role\nname: odd\nlabel: Odd\n", "permissions is required"},
		{"bad name", "kind: role\nname: Odd\nlabel: Odd\npermissions: [users:read]\n", `name "Odd"`},
	}
	for _, c := range cases {
		if got := roleProblems(c.src); !strings.Contains(got, c.want) {
			t.Errorf("%s: want %q, got %q", c.name, c.want, got)
		}
	}
}

func roleRow(t *testing.T, pool *pgxpool.Pool, name string) (app string, perms []string, found bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), "SELECT coalesce(app_name, ''), permissions FROM roles WHERE name = $1", name).Scan(&app, &perms)
	return app, perms, err == nil
}

// Installing writes the role where RLS and grants can find it; an update changes it in place, keeping its grants;
// removing it from the app, or removing the app, takes it and its grants away.
func TestInstallKeepsTheRolesTableInStep(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	if _, err := apps.Install(ctx, pool, roleApp(t, "audit", "1.0.0", map[string]string{"auditor.yaml": auditor}), apps.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if app, perms, ok := roleRow(t, pool, "auditor"); !ok || app != "audit" || strings.Join(perms, ",") != "tickets:read_all,users:read" {
		t.Fatalf("installed: %v %v %v", app, perms, ok)
	}
	var group string
	if err := pool.QueryRow(ctx, "INSERT INTO groups (name) VALUES ('Audit') RETURNING id").Scan(&group); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO role_grants (role, group_id) VALUES ('auditor', $1)", group); err != nil {
		t.Fatal(err)
	}

	wider := strings.Replace(auditor, "users:read]", "users:read, workflows:read_all]", 1)
	if _, err := apps.Install(ctx, pool, roleApp(t, "audit", "1.1.0", map[string]string{"auditor.yaml": wider}), apps.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, perms, _ := roleRow(t, pool, "auditor"); len(perms) != 3 {
		t.Errorf("updated: %v", perms)
	}
	if n := count(t, pool, "SELECT count(*) FROM role_grants WHERE role = 'auditor'"); n != 1 {
		t.Errorf("an update dropped the grants: %d", n)
	}

	plan, err := apps.Install(ctx, pool, roleApp(t, "audit", "2.0.0", map[string]string{"other.yaml": strings.Replace(auditor, "auditor", "reader", 1)}), apps.InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "removing the role auditor takes it away") {
		t.Errorf("the plan does not warn:\n%s", plan)
	}
	if _, _, ok := roleRow(t, pool, "auditor"); ok {
		t.Error("a role removed from its app is still there")
	}
	if n := count(t, pool, "SELECT count(*) FROM role_grants WHERE role = 'auditor'"); n != 0 {
		t.Errorf("grants of a removed role: %d", n)
	}

	if _, err := apps.Uninstall(ctx, pool, "audit"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := roleRow(t, pool, "reader"); ok {
		t.Error("uninstalling left the app's role behind")
	}
	if _, _, ok := roleRow(t, pool, "agent"); !ok {
		t.Error("uninstalling an app removed a built-in role")
	}
}

func TestTwoAppsCannotDefineTheSameRole(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	if _, err := apps.Install(ctx, pool, roleApp(t, "one", "1.0.0", map[string]string{"auditor.yaml": auditor}), apps.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := apps.Install(ctx, pool, roleApp(t, "two", "1.0.0", map[string]string{"auditor.yaml": auditor}), apps.InstallOptions{})
	if err == nil || !strings.Contains(err.Error(), "role auditor is already provided by one") {
		t.Errorf("want a conflict, got %v", err)
	}
}
