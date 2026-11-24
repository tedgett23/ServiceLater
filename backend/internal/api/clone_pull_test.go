package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/servicelater/servicelater/backend/internal/clone"
)

// The scheduled pull: the source hands out a pull key for a profile, which answers only its own endpoints (401
// without it, 403 for another profile, 422 without a passphrase). The target keeps the source's address and key,
// tests them, pulls by hand (fetched by the worker, then previewed with no passphrase to type), and turns on the
// schedule (confirmed with the instance's name), after which the worker pulls and applies a package when it is due,
// as whoever turned it on. Here the instance is its own source, over a real HTTP server on this machine.
func TestACloneIsPulledFromTheSource(t *testing.T) {
	h, pool, svc, watcher := setupBackups(t)
	ctx := context.Background()
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	srv := httptest.NewServer(h)
	defer srv.Close()

	// The source: a profile and a key for it.
	decodeBody[ticket](t, admin.do(h, "POST", "/api/v1/tickets", `{"title":"From the source"}`))
	expect(t, admin.do(h, "POST", "/api/v1/clones/profiles", `{"name":"uat","config":{}}`), 201, "a profile")
	expect(t, agent.do(h, "POST", "/api/v1/clones/profiles/uat/keys", `{"name":"UAT"}`), 403, "an agent making a key")
	refusal(t, admin.do(h, "POST", "/api/v1/clones/profiles/nope/keys", `{"name":"UAT"}`), 404, "", "a key for no profile")
	made := decodeBody[struct{ ID, Key, Prefix string }](t, admin.do(h, "POST", "/api/v1/clones/profiles/uat/keys", `{"name":"UAT"}`))
	if !strings.HasPrefix(made.Key, clone.PullKeyPrefix) || !strings.HasPrefix(made.Key, made.Prefix) {
		t.Fatalf("the key: %+v", made)
	}
	get := func(path, key string, header map[string]string) (int, string) {
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return res.StatusCode, string(b)
	}
	if code, _ := get(clone.SourcePath, "", nil); code != 401 {
		t.Errorf("without a key: %d", code)
	}
	for _, c := range admin.cookies {
		if code, _ := get(clone.SourcePath, c.Value, nil); code != 401 {
			t.Errorf("with the session's %s: %d", c.Name, code)
		}
	}
	if code, _ := get(clone.SourcePath, clone.PullKeyPrefix+"nope", nil); code != 401 {
		t.Errorf("with a made-up key: %d", code)
	}
	if code, body := get(clone.SourcePath, made.Key, nil); code != 200 || !strings.Contains(body, `"profile":"uat"`) {
		t.Errorf("the source: %d %s", code, body)
	}
	if code, _ := get(clone.PackagePath, made.Key, nil); code != 422 {
		t.Errorf("no passphrase: %d", code)
	}
	if code, _ := get(clone.PackagePath+"?profile=other", made.Key, map[string]string{clone.PassphraseHeader: "a long enough passphrase"}); code != 403 {
		t.Errorf("another profile: %d", code)
	}

	// The target (this instance, marked UAT): the source's address and key.
	expect(t, admin.do(h, "PUT", "/api/v1/settings/instance", `{"name":"Acme UAT","environment":"uat"}`), 200, "naming it")
	refusal(t, admin.do(h, "PUT", "/api/v1/clone-in/source", `{"url":"http://example.com","cron":"0 2 * * 0","timezone":"UTC"}`), 422, "https", "plain http elsewhere")
	refusal(t, admin.do(h, "PUT", "/api/v1/clone-in/source", `{"url":"`+srv.URL+`","key":"slk_x","cron":"0 2 * * 0","timezone":"UTC"}`), 422, "slp_", "an API key")
	refusal(t, admin.do(h, "POST", "/api/v1/clone-in/pull", ""), 422, "pull key", "pulling with nothing set")
	src := `{"url":"` + srv.URL + `","key":"` + made.Key + `","cron":"0 2 * * 0","timezone":"UTC","keep_people":true,"keep_studio":true}`
	if got := decodeBody[struct {
		KeySet bool `json:"key_set"`
	}](t, admin.do(h, "PUT", "/api/v1/clone-in/source", src)); !got.KeySet {
		t.Fatal("the key is not set")
	}
	tested := decodeBody[struct {
		Profile, Instance string
		Warnings          []string
	}](t, admin.do(h, "POST", "/api/v1/clone-in/source/test", `{}`))
	if tested.Profile != "uat" || tested.Instance != "Acme UAT" || len(tested.Warnings) != 1 {
		t.Errorf("tested: %+v", tested)
	}

	// Pulled by hand: fetched by the worker, then waiting to be applied.
	pulled := decodeBody[struct{ ID, Status, Origin string }](t, admin.do(h, "POST", "/api/v1/clone-in/pull", ""))
	if pulled.Status != "pulling" || pulled.Origin != "pull" {
		t.Fatalf("pulled: %+v", pulled)
	}
	refusal(t, admin.do(h, "POST", "/api/v1/clone-in/pull", ""), 409, "already", "a second pull")
	refusal(t, admin.do(h, "POST", "/api/v1/clone-in/"+pulled.ID+"/preview", `{}`), 409, "nothing has been fetched", "previewing before it is fetched")
	if err := svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	type loads struct {
		Loads []struct {
			ID, Status, Origin, Error string
			Source                    clone.Header
		}
	}
	got := decodeBody[loads](t, admin.do(h, "GET", "/api/v1/clone-in", ""))
	if len(got.Loads) != 1 || got.Loads[0].Status != "uploaded" || got.Loads[0].Source.Profile != "uat" || got.Loads[0].Source.Instance != "Acme UAT" {
		t.Fatalf("after the pull: %+v", got.Loads)
	}
	pv := decodeBody[clone.LoadPreview](t, admin.do(h, "POST", "/api/v1/clone-in/"+pulled.ID+"/preview", `{"keep_people":true,"keep_studio":true}`))
	if len(pv.Problems) != 0 || len(pv.People) == 0 {
		t.Errorf("the pulled package's preview: problems %v people %+v", pv.Problems, pv.People)
	}
	served := decodeBody[struct {
		Pulls []struct {
			Status  string
			KeyName string `json:"key_name"`
			Size    int64
		}
		PullKeys []struct {
			LastUsedAt *string `json:"last_used_at"`
		} `json:"pull_keys"`
	}](t, admin.do(h, "GET", "/api/v1/clones", ""))
	if len(served.Pulls) != 1 || served.Pulls[0].Status != "done" || served.Pulls[0].KeyName != "UAT" || served.Pulls[0].Size == 0 ||
		len(served.PullKeys) != 1 || served.PullKeys[0].LastUsedAt == nil {
		t.Errorf("the source's record: %+v", served)
	}
	expect(t, admin.do(h, "DELETE", "/api/v1/clone-in/"+pulled.ID, ""), 204, "deleting the pulled package")

	// The schedule: confirmed with the instance's name, then pulled and applied when due. The worker's clock stands
	// still, so every round below sees the same due time.
	at := time.Now()
	svc.Now = func() time.Time { return at }
	decodeBody[ticket](t, admin.do(h, "POST", "/api/v1/tickets", `{"title":"Only on the target"}`))
	on := strings.Replace(src, `"cron":"0 2 * * 0"`, `"enabled":true,"cron":"* * * * *"`, 1)
	refusal(t, admin.do(h, "PUT", "/api/v1/clone-in/source", on), 422, "Acme UAT", "turning it on unconfirmed")
	set := decodeBody[struct {
		ConfirmedByName string `json:"confirmed_by_name"`
	}](t, admin.do(h, "PUT", "/api/v1/clone-in/source", strings.Replace(on, `"url"`, `"confirm":"Acme UAT","url"`, 1)))
	if set.ConfirmedByName == "" {
		t.Error("nobody confirmed the schedule")
	}
	if err := svc.Tick(ctx); err != nil { // queued, fetched, asked to be applied
		t.Fatal(err)
	}
	if err := watcher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !watcher.InMaintenance() {
		t.Fatal("the scheduled pull is not being applied")
	}
	if err := svc.Tick(ctx); err != nil { // applied
		t.Fatal(err)
	}
	if err := watcher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var titles string
	if err := pool.QueryRow(ctx, `SELECT string_agg(title, ',' ORDER BY title) FROM tasks`).Scan(&titles); err != nil || titles != "From the source,Only on the target" {
		// The package was made from this very instance, after the second record: both come back.
		t.Errorf("records after the scheduled clone: %q (%v)", titles, err)
	}
	admin = signInAgain(t, h, "boss@x.test")
	got = decodeBody[loads](t, admin.do(h, "GET", "/api/v1/clone-in", ""))
	if len(got.Loads) != 1 || got.Loads[0].Status != "done" || got.Loads[0].Origin != "pull" {
		t.Fatalf("after the scheduled clone: %+v", got.Loads)
	}
	if err := svc.Tick(ctx); err != nil { // the same due time is not pulled again
		t.Fatal(err)
	}
	if again := decodeBody[loads](t, admin.do(h, "GET", "/api/v1/clone-in", "")); len(again.Loads) != 1 {
		t.Errorf("pulled twice for one due time: %+v", again.Loads)
	}

	// Revoked, the key stops at once.
	expect(t, admin.do(h, "DELETE", "/api/v1/clones/profiles/uat/keys/"+made.ID, ""), 204, "revoking the key")
	if code, _ := get(clone.SourcePath, made.Key, nil); code != 401 {
		t.Errorf("a revoked key: %d", code)
	}
}
