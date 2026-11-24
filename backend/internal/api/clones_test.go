package api_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/clone"
)

// Admin → Clones: only instance:clone holders see or use it. A profile is checked (a bad exclusion is refused with the
// reason), previewed (what it would keep, nothing made), and a package made from it by the worker opens only with
// its passphrase and holds what the profile chose. Packages keep no passphrase once made, and can be deleted.
func TestClonePackagesAreMadeFromProfiles(t *testing.T) {
	h, pool, svc, _ := setupBackups(t)
	ctx := context.Background()
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	expect(t, agent.do(h, "GET", "/api/v1/clones", ""), 403, "an agent")
	expect(t, agent.do(h, "POST", "/api/v1/clones/preview", `{"config":{}}`), 403, "an agent previewing")

	open := decodeBody[ticket](t, admin.do(h, "POST", "/api/v1/tickets", `{"title":"Still open"}`))
	closed := decodeBody[ticket](t, admin.do(h, "POST", "/api/v1/tickets", `{"title":"Settled"}`))
	if _, err := pool.Exec(ctx, `UPDATE tasks SET state_category = 'closed', status = 'closed' WHERE id = $1`, closed.ID); err != nil {
		t.Fatal(err)
	}
	_ = open

	refusal(t, admin.do(h, "POST", "/api/v1/clones/profiles", `{"name":"UAT!","config":{"records":"all","attachments":"kept","history":"all","people":"all"}}`),
		422, "lowercase", "a bad name")
	refusal(t, admin.do(h, "POST", "/api/v1/clones/profiles", `{"name":"uat","config":{"records":"open","attachments":"kept","history":"all","people":"all",
		"exclude":[{"record_type":"incident","when":"record.nonsense == 1"}]}}`), 422, "exclude 1", "an exclusion that does not compile")
	body := `{"name":"uat","description":"Weekly UAT refresh","config":{"records":"open","internal_notes":true,"attachments":"kept","history":"all",
		"people":"referenced","passwords":false,"mask":{"emails":true},"exclude":[{"record_type":"incident","when":"record.title == 'Nothing'"}]}}`
	expect(t, admin.do(h, "POST", "/api/v1/clones/profiles", body), 201, "adding a profile")
	expect(t, admin.do(h, "POST", "/api/v1/clones/profiles", body), 409, "the same name again")

	preview := decodeBody[clone.Report](t, admin.do(h, "POST", "/api/v1/clones/preview", `{"config":{"records":"open"}}`))
	// Settings left out keep their defaults.
	var kept, total int64
	for _, c := range preview.Records {
		if c.Type == "incident" {
			kept, total = c.Kept, c.Total
		}
	}
	if kept != 1 || total != 2 {
		t.Errorf("preview kept %d of %d incidents, want 1 of 2: %+v", kept, total, preview)
	}

	refusal(t, admin.do(h, "POST", "/api/v1/clones/packages", `{"profile":"uat","passphrase":"short","destination":"folder"}`), 422, "passphrase", "a short passphrase")
	expect(t, admin.do(h, "POST", "/api/v1/clones/packages", `{"profile":"nope","passphrase":"correct horse battery","destination":"folder"}`), 404, "no such profile")
	type pkg struct {
		ID, Status, Error, Name, Profile string
		Report                           *clone.Report
	}
	made := decodeBody[pkg](t, admin.do(h, "POST", "/api/v1/clones/packages", `{"profile":"uat","passphrase":"correct horse battery","destination":"folder"}`))
	if made.Status != "pending" {
		t.Fatalf("asked for: %+v", made)
	}
	var sealed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM clone_packages WHERE passphrase IS NOT NULL`).Scan(&sealed); err != nil || sealed != 1 {
		t.Fatalf("the passphrase waits sealed: %d %v", sealed, err)
	}
	if err := svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	list := decodeBody[struct{ Packages []pkg }](t, admin.do(h, "GET", "/api/v1/clones", ""))
	if len(list.Packages) != 1 || list.Packages[0].Status != "ready" || list.Packages[0].Report == nil {
		t.Fatalf("packages: %+v", list.Packages)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM clone_packages WHERE passphrase IS NOT NULL OR passphrase_nonce IS NOT NULL`).Scan(&sealed); err != nil || sealed != 0 {
		t.Errorf("the passphrase was kept after the package was made: %d %v", sealed, err)
	}
	w := admin.do(h, "GET", "/api/v1/clones/packages/"+made.ID+"/file", "")
	expect(t, w, 200, "downloading it")
	data, _ := io.ReadAll(w.Body)
	if _, err := clone.Verify(bytes.NewReader(data), "wrong horse battery"); err == nil {
		t.Error("the package opened with the wrong passphrase")
	}
	sum, err := clone.Verify(bytes.NewReader(data), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Rows["tasks"] != 1 || sum.Rows["user_credentials"] != 0 || sum.Profile != "uat" || sum.IdentitySalt == "" {
		t.Errorf("the package holds %v (profile %q)", sum.Rows, sum.Profile)
	}
	expect(t, agent.do(h, "DELETE", "/api/v1/clones/packages/"+made.ID, ""), 403, "an agent deleting one")
	expect(t, admin.do(h, "DELETE", "/api/v1/clones/packages/"+made.ID, ""), 204, "deleting it")
	expect(t, admin.do(h, "GET", "/api/v1/clones/packages/"+made.ID+"/file", ""), 404, "its file")
	expect(t, admin.do(h, "DELETE", "/api/v1/clones/profiles/uat", ""), 204, "removing the profile")
	expect(t, admin.do(h, "DELETE", "/api/v1/clones/profiles/uat", ""), 404, "removing it again")
}
