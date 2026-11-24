package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

// swapp packs an app from files, as `servicelater app pack` does.
func swapp(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, data, err := apps.Pack(dir)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// rawSwapp zips files without checking them, for an app that pack would refuse.
func rawSwapp(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func toolsFiles(version string, extra map[string]string) map[string]string {
	files := map[string]string{
		"app.yaml": "apiVersion: 1\nname: house-tools\nversion: " + version + "\nauthor: Tests\ndescription: \"Tools.\"\n" +
			"dependencies:\n  - core-itsm: \">=0.1.0\"\npermissions:\n  - records:read:incident\n",
		"default/roles/installer.yaml": "kind: role\nname: installer\nlabel: Installer\ndescription: \"Installs apps.\"\npermissions: [apps:manage]\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	return files
}

func toolsApp(t *testing.T, version string, extra map[string]string) string {
	return swapp(t, toolsFiles(version, extra))
}

type appPlan struct {
	App            string   `json:"app"`
	FromVersion    string   `json:"from_version"`
	ToVersion      string   `json:"to_version"`
	Added          []string `json:"added"`
	NewPermissions []string `json:"new_permissions"`
	NoChange       bool     `json:"no_change"`
}

type installedApps struct {
	Items []struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		FirstParty  bool   `json:"first_party"`
		InstalledBy string `json:"installed_by"`
	} `json:"items"`
}

// Admin → Apps: apps:manage holders list apps, see what an uploaded .swapp would do, install and update it, and
// remove it. Approving an app's new permissions also needs roles:manage. A lower version is refused, so is a
// first-party app (it comes with ServiceLater), and a development reload is refused unless the instance is marked
// development.
func TestAppsAreInstalledFromTheAdminPage(t *testing.T) {
	h, pool, _, watcher := setupBackups(t)
	ctx := context.Background()
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	priya := newActor(t, h, pool, "priya@x.test", "agent")
	send := func(a actor, path, body string) *httptest.ResponseRecorder {
		return a.do(h, "POST", "/api/v1/apps"+path, body)
	}

	expect(t, priya.do(h, "GET", "/api/v1/apps", ""), 403, "an agent listing apps")
	list := decodeBody[installedApps](t, admin.do(h, "GET", "/api/v1/apps", ""))
	for _, a := range list.Items {
		if a.Name == "core-itsm" && !a.FirstParty {
			t.Error("core-itsm is not first-party")
		}
	}

	// What installing would do, and that it asks for a permission it does not have yet.
	v1 := toolsApp(t, "1.0.0", nil)
	plan := decodeBody[appPlan](t, send(admin, "/plan", v1))
	if plan.App != "house-tools" || plan.FromVersion != "" || plan.ToVersion != "1.0.0" || !strings.Contains(strings.Join(plan.Added, ","), "role installer") ||
		strings.Join(plan.NewPermissions, ",") != "records:read:incident" {
		t.Errorf("the plan: %+v", plan)
	}
	refusal(t, send(admin, "/install", v1), 422, "permissions", "installing without approving its permissions")
	done := decodeBody[appPlan](t, send(admin, "/install?approve_permissions=true", v1))
	if done.App != "house-tools" || done.ToVersion != "1.0.0" {
		t.Errorf("installed: %+v", done)
	}
	list = decodeBody[installedApps](t, admin.do(h, "GET", "/api/v1/apps", ""))
	found := false
	for _, a := range list.Items {
		found = found || a.Name == "house-tools" && a.Version == "1.0.0" && a.InstalledBy == "boss@x.test" && !a.FirstParty
	}
	if !found {
		t.Errorf("after the install: %+v", list.Items)
	}

	// Someone given apps:manage by a role installs updates, but cannot approve permissions.
	expect(t, admin.do(h, "POST", "/api/v1/roles/installer/grants", `{"user_id":"`+priya.id+`"}`), 201, "granting the installer role")
	priya = signInAgain(t, h, "priya@x.test")
	expect(t, priya.do(h, "GET", "/api/v1/apps", ""), 200, "the installer listing apps")
	v11 := toolsApp(t, "1.1.0", map[string]string{"default/notifications/heads-up.yaml": "kind: notification\nname: heads-up\ntitle: \"{{record.reference}}\"\n"})
	refusal(t, send(priya, "/install?approve_permissions=true", v11), 403, "roles:manage", "approving permissions without roles:manage")
	up := decodeBody[appPlan](t, send(priya, "/install", v11))
	if up.FromVersion != "1.0.0" || up.ToVersion != "1.1.0" || strings.Join(up.Added, ",") != "notification heads-up" {
		t.Errorf("the update: %+v", up)
	}

	// Refused: older, first-party, broken, not an archive.
	refusal(t, send(admin, "/install", v1), 422, "older than the installed 1.1.0", "going back a version")
	core, _ := filepath.Abs("../../../apps/core-itsm")
	_, coreArchive, err := apps.Pack(core)
	if err != nil {
		t.Fatal(err)
	}
	refusal(t, send(admin, "/plan", string(coreArchive)), 409, "comes with ServiceLater", "a first-party app")
	broken := rawSwapp(t, toolsFiles("1.2.0", map[string]string{"default/rules/bad.yaml": "kind: rule\nname: bad\nrecord_type: incident\ntiming: before\non: [update]\nrun:\n  - action: explode\n"}))
	if w := send(admin, "/plan", broken); w.Code != 422 || !strings.Contains(w.Body.String(), `unknown action \"explode\"`) {
		t.Errorf("a broken app: %d %s", w.Code, w.Body.String())
	}
	refusal(t, send(admin, "/plan", "not a zip"), 422, "not a .swapp", "a file that is not an app")

	// A development reload, only into a development instance.
	refusal(t, send(admin, "/install?dev=true", v11), 409, "development", "a dev reload into an unmarked instance")
	expect(t, admin.do(h, "PUT", "/api/v1/settings/instance", `{"name":"Dev box","environment":"development"}`), 200, "marking it development")
	if err := watcher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	dev := decodeBody[appPlan](t, send(admin, "/install?dev=true", toolsApp(t, "1.1.0", map[string]string{
		"default/notifications/heads-up.yaml": "kind: notification\nname: heads-up\ntitle: \"Look at {{record.reference}}\"\n"})))
	if dev.FromVersion != "1.1.0" || dev.NoChange {
		t.Errorf("the dev reload: %+v", dev)
	}
	list = decodeBody[installedApps](t, admin.do(h, "GET", "/api/v1/apps", ""))
	for _, a := range list.Items {
		if a.Name == "house-tools" && a.InstalledBy != "boss@x.test (dev)" {
			t.Errorf("installed by: %q", a.InstalledBy)
		}
	}

	// Removing it.
	refusal(t, admin.do(h, "DELETE", "/api/v1/apps/core-itsm?confirm=core-itsm", ""), 409, "comes with ServiceLater", "removing a first-party app")
	refusal(t, admin.do(h, "DELETE", "/api/v1/apps/house-tools", ""), 422, "type the app's name", "removing it unconfirmed")
	expect(t, admin.do(h, "DELETE", "/api/v1/apps/house-tools?confirm=house-tools", ""), http.StatusNoContent, "removing it")
	expect(t, admin.do(h, "DELETE", "/api/v1/apps/house-tools?confirm=house-tools", ""), 404, "removing it twice")
	expect(t, priya.do(h, "GET", "/api/v1/apps", ""), 403, "the installer after the role went with its app")
}
