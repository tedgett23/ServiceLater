package apps_test

import (
	"context"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
)

func permApp(t *testing.T, version, perms, rules string) *apps.Compiled {
	t.Helper()
	files := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: helper\nversion: " + version + "\nauthor: t\ndescription: \"Helps.\"\npermissions: " + perms + "\n")}
	if rules != "" {
		files["default/rules/r.yaml"] = []byte(rules)
	}
	c, err := (&apps.Bundle{Files: files}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// An app's permissions are what its principal may do for anyone, so new ones are shown and need approving.
func TestNewPermissionsNeedApproval(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	runAs := "kind: rule\nname: note\nrecord_type: task\ntiming: after\non: [update]\nrun_as: app\nrun:\n  - action: comment.add\n    body: hi\n"
	v1 := permApp(t, "1.0.0", "[tickets:read_all]", runAs)

	_, err := apps.Install(ctx, pool, v1, apps.InstallOptions{})
	if err == nil || !strings.Contains(err.Error(), "asks for permissions it does not have yet: tickets:read_all") {
		t.Fatalf("want a refusal, got %v", err)
	}
	plan, err := apps.Install(ctx, pool, v1, apps.InstallOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"permits    tickets:read_all", "new        tickets:read_all (needs approval)", "run_as     rule note acts as the app"} {
		if !strings.Contains(plan.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}
	if _, err := apps.Install(ctx, pool, v1, apps.InstallOptions{ApprovePermissions: true}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, "SELECT count(*) FROM users WHERE app_name = 'helper' AND deactivated_at IS NULL"); n != 1 {
		t.Errorf("the app's principal: %d", n)
	}
	// The same permissions again need nothing; one more needs approving again; fewer do not.
	if _, err := apps.Install(ctx, pool, permApp(t, "1.0.1", "[tickets:read_all]", runAs), apps.InstallOptions{}); err != nil {
		t.Errorf("an upgrade asking for nothing new: %v", err)
	}
	if _, err := apps.Install(ctx, pool, permApp(t, "1.1.0", "[tickets:read_all, tickets:update]", runAs), apps.InstallOptions{}); err == nil ||
		!strings.Contains(err.Error(), "tickets:update") || strings.Contains(err.Error(), "tickets:read_all,") {
		t.Errorf("an upgrade asking for more: %v", err)
	}
	if _, err := apps.Install(ctx, pool, permApp(t, "1.2.0", "[]", runAs), apps.InstallOptions{}); err != nil {
		t.Errorf("an upgrade asking for less: %v", err)
	}

	// Uninstalling deactivates the principal (it keeps what it wrote); installing again brings it back.
	if _, err := apps.Uninstall(ctx, pool, "helper"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, "SELECT count(*) FROM users WHERE app_name = 'helper' AND deactivated_at IS NOT NULL"); n != 1 {
		t.Errorf("after uninstalling: %d", n)
	}
	if _, err := apps.Install(ctx, pool, v1, apps.InstallOptions{ApprovePermissions: true}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, "SELECT count(*) FROM users WHERE app_name = 'helper' AND deactivated_at IS NULL"); n != 1 {
		t.Errorf("after installing again: %d", n)
	}
}

func TestRunAsIsOnlyForAfterRules(t *testing.T) {
	for timing, want := range map[string]string{"before": "not supported for a before rule", "async": "every async rule runs already"} {
		src := "kind: rule\nname: x\nrecord_type: task\ntiming: " + timing + "\non: [update]\nrun_as: app\nrun:\n  - action: abort\n    message: no\n"
		if timing == "async" {
			src = strings.Replace(src, "action: abort\n    message: no", "action: comment.add\n    body: hi", 1)
		}
		files := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: x\nversion: 1.0.0\nauthor: t\ndescription: \"X.\"\n"), "default/rules/x.yaml": []byte(src)}
		if _, err := (&apps.Bundle{Files: files}).Compile(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", timing, want, err)
		}
	}
}
