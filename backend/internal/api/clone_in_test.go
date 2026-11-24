package api_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/servicelater/servicelater/backend/internal/clone"
)

// Admin → Clone in: only instance:clone holders see it. A package made here (standing in for production's) is
// uploaded, previewed with its passphrase, refused while this instance is marked production and without the
// confirmation, then applied: the instance is in maintenance until the worker has loaded it, and afterwards its records
// are the package's, the admin who applied it can sign in again, and the safety switch is on until lifted. People can
// be put on and taken off the keep list.
type holds struct {
	Settings struct {
		HoldHTTP     bool `json:"hold_http"`
		HoldWebhooks bool `json:"hold_webhooks"`
	}
}

func TestAClonePackageIsLoaded(t *testing.T) {
	h, pool, svc, watcher := setupBackups(t)
	ctx := context.Background()
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	expect(t, agent.do(h, "GET", "/api/v1/clone-in", ""), 403, "an agent")

	// "Production": one record with a file attached, made into a package.
	prod := decodeBody[ticket](t, admin.do(h, "POST", "/api/v1/tickets", `{"title":"From production"}`))
	const fileKey = "attachments/ab/cloned-file"
	if err := os.MkdirAll(filepath.Join(svc.Files.LocalDir, "attachments", "ab"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.Files.LocalDir, "attachments", "ab", "cloned-file"), []byte("log lines"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO attachments (task_id, filename, content_type, size, backend, key, part_size, status, created_by)
		SELECT $1, 'log.txt', 'text/plain', 9, 'local', $2, 9, 'ready', requester_id FROM tasks WHERE id = $1`, prod.ID, fileKey); err != nil {
		t.Fatal(err)
	}
	expect(t, admin.do(h, "POST", "/api/v1/clones/profiles", `{"name":"uat","config":{}}`), 201, "a profile")
	made := decodeBody[struct{ ID string }](t, admin.do(h, "POST", "/api/v1/clones/packages", `{"profile":"uat","passphrase":"correct horse battery","destination":"folder"}`))
	if err := svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	dl := admin.do(h, "GET", "/api/v1/clones/packages/"+made.ID+"/file", "")
	expect(t, dl, 200, "downloading the package")
	pkg := dl.Body.Bytes()

	// "UAT": a record of its own, and marked production by mistake.
	decodeBody[ticket](t, admin.do(h, "POST", "/api/v1/tickets", `{"title":"Only in UAT"}`))
	expect(t, admin.do(h, "PUT", "/api/v1/settings/instance", `{"name":"Acme UAT","environment":"production"}`), 200, "naming it")
	refusal(t, admin.do(h, "POST", "/api/v1/clone-in/upload", "not a package"), 422, "not a ServiceLater clone package", "uploading anything else")
	up := decodeBody[struct {
		ID     string
		Status string
		Source clone.Header
	}](t, admin.do(h, "POST", "/api/v1/clone-in/upload", string(pkg)))
	if up.Status != "uploaded" || up.Source.Profile != "uat" {
		t.Fatalf("uploaded: %+v", up)
	}
	base := "/api/v1/clone-in/" + up.ID
	refusal(t, admin.do(h, "POST", base+"/preview", `{"passphrase":"wrong horse battery"}`), 422, "passphrase", "the wrong passphrase")
	pv := decodeBody[clone.LoadPreview](t, admin.do(h, "POST", base+"/preview", `{"passphrase":"correct horse battery","keep_people":true,"keep_studio":true}`))
	if len(pv.People) != 1 || pv.People[0].Outcome != "matched" || len(pv.Problems) != 1 || !strings.Contains(pv.Problems[0], "production") {
		t.Fatalf("preview: people %+v problems %v", pv.People, pv.Problems)
	}
	apply := `{"passphrase":"correct horse battery","keep_people":true,"keep_studio":true,"confirm":"Acme UAT"}`
	refusal(t, admin.do(h, "POST", base+"/apply", apply), 409, "marked production", "into production")
	expect(t, admin.do(h, "PUT", "/api/v1/settings/instance", `{"name":"Acme UAT","environment":"uat"}`), 200, "marking it UAT")
	refusal(t, admin.do(h, "POST", base+"/apply", strings.Replace(apply, "Acme UAT", "Acme", 1)), 422, "Acme UAT", "the wrong confirmation")
	refusal(t, admin.do(h, "POST", base+"/apply", strings.Replace(apply, "correct", "wrong", 1)), 422, "passphrase", "the wrong passphrase")
	expect(t, admin.do(h, "POST", base+"/apply", apply), 202, "applying it")
	refusal(t, agent.do(h, "GET", "/api/v1/tickets", ""), 503, "loading a clone", "while it waits")
	if anon := do(h, "GET", "/api/v1/instance", "", nil).Body.String(); !strings.Contains(anon, `"operation":"clone"`) {
		t.Errorf("signed out: %s", anon)
	}

	// While it loads, the instance's state never waits for it (the load locks the settings table).
	if err := os.Remove(filepath.Join(svc.Files.LocalDir, "attachments", "ab", "cloned-file")); err != nil {
		t.Fatal(err)
	}
	loaded := make(chan error, 1)
	go func() {
		tick, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		loaded <- svc.Tick(tick)
	}()
	if err := <-loaded; err != nil {
		t.Fatal(err)
	}
	if err := watcher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// The file came with the package (a load that stores files must not wait on its own locks: it did, for ever).
	if b, err := os.ReadFile(filepath.Join(svc.Files.LocalDir, "attachments", "ab", "cloned-file")); err != nil || string(b) != "log lines" {
		t.Errorf("the attachment's file after the clone: %q %v", b, err)
	}
	var titles string
	if err := pool.QueryRow(ctx, `SELECT string_agg(title, ',' ORDER BY title) FROM tasks`).Scan(&titles); err != nil || titles != "From production" {
		t.Errorf("records after the clone: %q (%v)", titles, err)
	}
	admin = signInAgain(t, h, "boss@x.test")
	got := decodeBody[struct {
		Loads []struct {
			Status, Error string
			Report        *clone.LoadReport
		}
		Switched []struct {
			Label string
			On    bool
		}
	}](t, admin.do(h, "GET", "/api/v1/clone-in", ""))
	if len(got.Loads) != 1 || got.Loads[0].Status != "done" || got.Loads[0].Report == nil || got.Loads[0].Report.Rows["tasks"] != 1 ||
		got.Loads[0].Report.Files != 1 || got.Loads[0].Report.MissingFiles != 0 {
		t.Fatalf("after the clone: %+v", got.Loads)
	}
	held := decodeBody[holds](t, admin.do(h, "GET", "/api/v1/clone-in", ""))
	if !held.Settings.HoldHTTP || !held.Settings.HoldWebhooks {
		t.Errorf("the holds are not on: %+v", held)
	}
	expect(t, admin.do(h, "PUT", "/api/v1/clone-in/settings", `{"hold_http":false,"hold_schedules":false,"hold_webhooks":false}`), 200, "lifting the holds")
	held = decodeBody[holds](t, admin.do(h, "GET", "/api/v1/clone-in", ""))
	if held.Settings.HoldHTTP || held.Settings.HoldWebhooks {
		t.Errorf("the holds are still on: %+v", held)
	}

	var agentID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE email = 'agent@x.test'`).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	expect(t, admin.do(h, "PUT", "/api/v1/clone-in/keep/"+agentID, ""), 204, "keeping the agent")
	keep := decodeBody[struct{ Keep []struct{ Email string } }](t, admin.do(h, "GET", "/api/v1/clone-in", ""))
	if len(keep.Keep) != 1 || keep.Keep[0].Email != "agent@x.test" {
		t.Errorf("keep list %+v", keep.Keep)
	}
	expect(t, admin.do(h, "DELETE", "/api/v1/clone-in/keep/"+agentID, ""), 204, "no longer keeping them")
	expect(t, admin.do(h, "DELETE", base, ""), 204, "deleting the package")
}
