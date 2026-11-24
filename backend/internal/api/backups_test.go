package api_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/api"
	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/backup"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
	"github.com/servicelater/servicelater/backend/internal/instance"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/snapshot"
)

func setupBackups(t *testing.T) (http.Handler, *pgxpool.Pool, *backup.Service, *instance.Watcher) {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.InstallFirstParty(t, pool)
	reg := apps.NewRegistry(pool)
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.NewKeyring(testKey(5))
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := auth.NewTokens("0123456789abcdef0123456789abcdef", time.Minute)
	svc := auth.NewService(pool, tk, time.Hour)
	if _, err := svc.Bootstrap(context.Background(), "admin@example.com", "Admin", "hunter2hunter2"); err != nil {
		t.Fatal(err)
	}
	backups := &backup.Service{Pool: pool, Keys: keys, Dir: t.TempDir(), Binary: "test", Registry: reg}
	watcher := instance.NewWatcher(pool)
	h := api.NewRouter(api.Deps{Pool: pool, Auth: svc, Tokens: tk, Apps: reg, Keys: keys, Backups: backups, Instance: watcher})
	return h, pool, backups, watcher
}

// Admin → Backups: only instance:backup holders see or use it. A snapshot asked for is taken by the worker, listed,
// downloaded and restored: while the restore waits the instance is in maintenance (503 but for GET /instance), and
// afterwards what was filed since is gone. A snapshot from elsewhere can be uploaded, and a file that is not one is
// refused. Deleting one removes its file.
func TestBackupsAreTakenAndRestored(t *testing.T) {
	h, pool, svc, watcher := setupBackups(t)
	ctx := context.Background()
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	expect(t, agent.do(h, "GET", "/api/v1/backups", ""), 403, "an agent")
	expect(t, agent.do(h, "POST", "/api/v1/backups", ""), 403, "an agent taking one")
	refusal(t, admin.do(h, "PUT", "/api/v1/backups/settings", `{"enabled":true,"cron":"every night","timezone":"UTC","destination":"folder","keep_last":3}`), 422, "cron", "a bad schedule")
	expect(t, admin.do(h, "PUT", "/api/v1/settings/instance", `{"name":"Acme UAT","environment":"uat"}`), 200, "naming the instance")
	inst := decodeBody[struct {
		Name, Environment string
		Maintenance       *struct{ Operation string }
	}](t, agent.do(h, "GET", "/api/v1/instance", ""))
	if inst.Name != "Acme UAT" || inst.Environment != "uat" || inst.Maintenance != nil {
		t.Fatalf("the instance: %+v", inst)
	}

	before := decodeBody[ticket](t, admin.do(h, "POST", "/api/v1/tickets", `{"title":"Before the snapshot"}`))
	w := admin.do(h, "POST", "/api/v1/backups", "")
	expect(t, w, 202, "taking one")
	if err := svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	type snap struct {
		ID, Kind, Status, Error, Name, Instance string
		Rows                                    int64
		Size                                    int64
	}
	list := decodeBody[struct{ Snapshots []snap }](t, admin.do(h, "GET", "/api/v1/backups", ""))
	if len(list.Snapshots) != 1 || list.Snapshots[0].Status != "ready" || list.Snapshots[0].Instance != "Acme UAT" || list.Snapshots[0].Rows == 0 {
		t.Fatalf("after the worker: %+v", list.Snapshots)
	}
	s := list.Snapshots[0]
	if !strings.HasPrefix(s.Name, "servicelater-acme-uat-") {
		t.Errorf("file name %q", s.Name)
	}
	if _, err := os.Stat(filepath.Join(svc.Dir, s.Name)); err != nil {
		t.Errorf("the file: %v", err)
	}
	dl := admin.do(h, "GET", "/api/v1/backups/"+s.ID+"/file", "")
	expect(t, dl, 200, "downloading it")
	if !bytes.HasPrefix(dl.Body.Bytes(), []byte(snapshot.Magic)) || int64(dl.Body.Len()) != s.Size {
		t.Fatalf("the download: %d bytes, want %d", dl.Body.Len(), s.Size)
	}
	file := dl.Body.Bytes()

	after := decodeBody[ticket](t, admin.do(h, "POST", "/api/v1/tickets", `{"title":"After the snapshot"}`))
	refusal(t, admin.do(h, "POST", "/api/v1/backups/"+s.ID+"/restore", `{"confirm":"restore"}`), 422, "Acme UAT", "the wrong confirmation")
	expect(t, admin.do(h, "POST", "/api/v1/backups/"+s.ID+"/restore", `{"confirm":"Acme UAT"}`), 202, "restoring it")
	refusal(t, agent.do(h, "GET", "/api/v1/tickets", ""), 503, "restoring a snapshot", "while it waits")
	// Strangers learn only that the instance is in maintenance, not its name or who is restoring it.
	if anon := do(h, "GET", "/api/v1/instance", "", nil).Body.String(); !strings.Contains(anon, `"operation":"restore"`) ||
		strings.Contains(anon, "Acme") || strings.Contains(anon, "boss@x.test") {
		t.Errorf("signed out: %s", anon)
	}
	inst = decodeBody[struct {
		Name, Environment string
		Maintenance       *struct{ Operation string }
	}](t, agent.do(h, "GET", "/api/v1/instance", ""))
	if inst.Maintenance == nil || inst.Maintenance.Operation != "restore" {
		t.Fatalf("maintenance: %+v", inst)
	}
	if err := svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var titles []string
	rows, err := pool.Query(ctx, `SELECT title FROM tasks ORDER BY title`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		titles = append(titles, s)
	}
	rows.Close()
	if strings.Join(titles, ",") != before.Title {
		t.Errorf("after the restore the records are %v, want only %q (not %q)", titles, before.Title, after.Title)
	}
	var sessions int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessions)
	if sessions != 0 {
		t.Errorf("%d sessions after a restore", sessions)
	}
	// Everyone signs in again; the record of the snapshot and its restore survives the restore.
	admin = signInAgain(t, h, "boss@x.test")
	got := decodeBody[struct {
		Snapshots []snap
		Restores  []struct{ Status, Error string }
	}](t, admin.do(h, "GET", "/api/v1/backups", ""))
	if len(got.Snapshots) != 1 || len(got.Restores) != 1 || got.Restores[0].Status != "done" {
		t.Fatalf("after the restore: %+v", got)
	}

	// An upload: the same file again, then something that is not a snapshot.
	expect(t, admin.do(h, "POST", "/api/v1/backups/upload", string(file)), 201, "uploading a snapshot")
	refusal(t, admin.do(h, "POST", "/api/v1/backups/upload", "not a snapshot"), 422, "not a ServiceLater snapshot", "uploading anything else")
	expect(t, admin.do(h, "DELETE", "/api/v1/backups/"+s.ID, ""), 204, "deleting it")
	if _, err := os.Stat(filepath.Join(svc.Dir, s.Name)); !os.IsNotExist(err) {
		t.Errorf("the file is still there: %v", err)
	}
}

// signInAgain signs someone in again (a restore ends every session).
func signInAgain(t *testing.T, h http.Handler, email string) actor {
	t.Helper()
	w := do(h, "POST", "/api/v1/auth/login", `{"email":"`+email+`","password":"password-`+email+`"}`, nil)
	if w.Code != 200 {
		t.Fatalf("login %s: %d %s", email, w.Code, w.Body)
	}
	return actor{cookies: w.Result().Cookies()}
}

// The schedule queues one snapshot per due time, and retention keeps the newest few and one a day and a week.
func TestScheduledSnapshotsAndRetention(t *testing.T) {
	h, pool, svc, _ := setupBackups(t)
	ctx := context.Background()
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	expect(t, admin.do(h, "PUT", "/api/v1/backups/settings", `{"enabled":true,"cron":"0 * * * *","timezone":"UTC","destination":"folder","keep_last":2,"keep_days":0,"keep_weeks":0}`), 200, "a schedule")
	now := time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	for range 3 {
		if err := svc.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if err := svc.Tick(ctx); err != nil { // the same hour again: nothing new
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
	}
	var ready int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM snapshots WHERE kind = 'scheduled' AND status = 'ready'`).Scan(&ready)
	if ready != 2 {
		t.Errorf("%d scheduled snapshots kept, want 2 (keep_last)", ready)
	}
	files, _ := filepath.Glob(filepath.Join(svc.Dir, "*.slsnap"))
	if len(files) != 2 {
		t.Errorf("%d files in the folder, want 2", len(files))
	}

	day := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, time.UTC) }
	taken := []time.Time{day(24, 2), day(23, 14), day(23, 2), day(22, 2), day(16, 2), day(10, 2), day(2, 2)}
	keep := backup.Keep(taken, 1, 3, 3, day(24, 9), time.UTC)
	var kept []int
	for i := range taken {
		if keep[i] {
			kept = append(kept, i)
		}
	}
	// The newest; the newest of each of the last three days (24th, 23rd, 22nd); the newest of each of the last three
	// ISO weeks: this one (the 24th), last one (Sunday the 22nd, so not Monday the 16th) and the one before (the 10th).
	// The 2nd is older than three weeks.
	if want := []int{0, 1, 3, 5}; !equalInts(kept, want) {
		t.Errorf("kept %v, want %v", kept, want)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
