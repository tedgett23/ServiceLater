package workflowengine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/workflow"
	"github.com/servicelater/servicelater/backend/internal/workflowengine"
)

const housePerms = "[records:read:*, records:update:*, records:create:incident, records:create:work_task, comments:internal]"

// installManifest installs an app with its own manifest extras (permissions, config) and files.
func (w *world) installManifest(t *testing.T, name, extra string, files map[string]string) {
	t.Helper()
	all := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: " + name + "\nversion: 1.0.0\nauthor: t\ndescription: \"Test.\"\n" + extra)}
	for f, src := range files {
		all["default/"+f] = []byte(src)
	}
	c, err := (&apps.Bundle{Files: all}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Install(context.Background(), w.pool, c, apps.InstallOptions{ApprovePermissions: true}); err != nil {
		t.Fatal(err)
	}
	if err := w.reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// settle runs queued runs and wakes due ones until nothing moves: parents, their children, and the parents again.
func (w *world) settle(t *testing.T) {
	t.Helper()
	for range 60 {
		var ids []uuid.UUID
		if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(context.Background(), `SELECT id FROM workflow_executions WHERE status IN ('queued', 'running') ORDER BY created_at`)
			if err != nil {
				return err
			}
			ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		woken, err := w.engine.WakeDue(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) == 0 && woken == 0 {
			return
		}
		for _, id := range ids {
			_ = w.run(id)
		}
	}
	t.Fatal("runs did not settle")
}

func (w *world) vars(t *testing.T, id uuid.UUID) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(w.execution(t, id).Vars, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

const childFlow = `kind: workflow
name: child
record_type: incident
start: s
steps:
  s:
    type: script
    script: |
      if vars.get("fail"):
          abort("asked to fail")
      return {"vars": {"doubled": vars.get("n", vars.get("item", 0)) * 2, "seen": record.title if record else ""}}
    next: done
  done:
    type: end
`

// A subflow runs another workflow of the app with inputs and waits for it; its variables come back. A child that
// fails takes the step's on_error.
func TestASubflowRunsAndReturnsItsVariables(t *testing.T) {
	w := setup(t)
	w.installWith(t, "house", housePerms, map[string]string{"child": childFlow, "parent": wf("parent", "call",
		"  call:\n    type: subflow\n    workflow: child\n    inputs:\n      n: 21\n      fail: =false\n    next: say\n    on_error: oops\n"+
			"  say:\n    type: comment.add\n    body: '=\"got \" + string(vars.call_vars.doubled) + \" from \" + vars.call_vars.seen + \" (\" + vars.call_outcome + \")\"'\n    next: done\n"+
			"  oops:\n    type: comment.add\n    body: '=\"child failed: \" + vars.call_outcome'\n    next: done\n  done:\n    type: end\n")}, nil)
	id := w.start(t, "house", "parent", w.agent)
	w.settle(t)
	if ex := w.execution(t, id); ex.Status != workflow.Done {
		t.Fatalf("parent: %s %s", ex.Status, ex.Error)
	}
	if got := strings.Join(w.comments(t), " | "); got != "got 42 from Printer (done)" {
		t.Errorf("comments: %s", got)
	}

	w.installWith(t, "house", housePerms, map[string]string{"child": childFlow, "parent": strings.Replace(wf("parent", "call",
		"  call:\n    type: subflow\n    workflow: child\n    inputs:\n      fail: =true\n    next: done\n    on_error: oops\n"+
			"  oops:\n    type: comment.add\n    body: '=\"child failed: \" + vars.call_outcome'\n    next: done\n  done:\n    type: end\n"), "", "", 1)}, nil)
	id = w.start(t, "house", "parent", w.agent)
	w.settle(t)
	if ex := w.execution(t, id); ex.Status != workflow.Done || !strings.Contains(strings.Join(w.comments(t), "|"), "child failed: failed") {
		t.Errorf("a failing child: %s %v", ex.Status, w.comments(t))
	}
}

// A foreach runs a workflow per item, two at a time, and collects what each left; with each: record it runs on the
// records a query found.
func TestAForeachRunsPerItemAndPerRecord(t *testing.T) {
	w := setup(t)
	w.installWith(t, "house", housePerms, map[string]string{"child": childFlow, "parent": wf("parent", "each",
		"  each:\n    type: foreach\n    workflow: child\n    items: '[1, 2, 3, 4, 5]'\n    parallel: 2\n    next: sum\n"+
			"  sum:\n    type: comment.add\n    body: '=\"results \" + string(size(vars.each_results)) + \", failed \" + string(vars.each_failed)'\n    next: done\n  done:\n    type: end\n")}, nil)
	id := w.start(t, "house", "parent", w.agent)
	if err := w.run(id); err != nil {
		t.Fatal(err)
	}
	if n := w.count(t, "SELECT count(*) FROM workflow_executions WHERE parent_id = $1", id); n != 2 {
		t.Errorf("%d children started at once, want 2", n)
	}
	w.settle(t)
	if got := strings.Join(w.comments(t), " | "); got != "results 5, failed 0" {
		t.Errorf("comments: %s (%s)", got, w.execution(t, id).Error)
	}
	var doubled []string
	for _, r := range w.vars(t, id)["each_results"].([]any) {
		doubled = append(doubled, fmt.Sprint(r.(map[string]any)["doubled"]))
	}
	if strings.Join(doubled, ",") != "2,4,6,8,10" {
		t.Errorf("results in order: %v", doubled)
	}

	// Records: a query finds incidents, and a foreach runs the child on each.
	w.exec(t, `INSERT INTO tasks (type, title, priority, requester_id, status, state_category) VALUES
		('incident', 'Disk A', 3, $1, 'new', 'open'), ('incident', 'Disk B', 3, $1, 'new', 'open')`, w.requester)
	w.installWith(t, "house", housePerms, map[string]string{"child": childFlow, "parent": wf("parent", "find",
		"  find:\n    type: query\n    find:\n      type: incident\n      where:\n        state_category: { eq: open }\n    next: each\n"+
			"  each:\n    type: foreach\n    workflow: child\n    items: =vars.find_ids\n    each: record\n    next: sum\n"+
			"  sum:\n    type: comment.add\n    body: '=\"saw \" + string(vars.find_count)'\n    next: done\n  done:\n    type: end\n")}, nil)
	before := len(w.comments(t))
	id = w.start(t, "house", "parent", w.agent)
	w.settle(t)
	got := w.comments(t)
	if ex := w.execution(t, id); ex.Status != workflow.Done || len(got) != before+1 || got[before] != "saw 3" {
		t.Errorf("records: %s %s %v", ex.Status, ex.Error, got)
	}
	var seen []string
	for _, r := range w.vars(t, id)["each_results"].([]any) {
		seen = append(seen, r.(map[string]any)["seen"].(string))
	}
	slices.Sort(seen) // Disk A and B were filed at the same moment
	if strings.Join(seen, ",") != "Disk A,Disk B,Printer" {
		t.Errorf("each ran on its record: %v", seen)
	}
}

// record.create files a record through the save path and keeps its reference; notify sends a template.
func TestRecordCreateAndNotifySteps(t *testing.T) {
	w := setup(t)
	w.installWith(t, "house", housePerms, map[string]string{"parent": wf("parent", "file",
		"  file:\n    type: record.create\n    record_type: work_task\n    title: '=\"Follow up on \" + record.title'\n    parent: =record.id\n    next: say\n"+
			"  say:\n    type: comment.add\n    body: '=\"filed \" + vars.file_reference'\n    next: done\n  done:\n    type: end\n")}, nil)
	id := w.start(t, "house", "parent", w.agent)
	w.settle(t)
	if ex := w.execution(t, id); ex.Status != workflow.Done {
		t.Fatalf("%s %s", ex.Status, ex.Error)
	}
	if n := w.count(t, "SELECT count(*) FROM tasks WHERE type = 'work_task' AND title = 'Follow up on Printer' AND parent_id = $1", w.ticket); n != 1 {
		t.Error("the task was not filed")
	}
	if c := w.comments(t); len(c) != 1 || !strings.HasPrefix(c[0], "filed TASK") {
		t.Errorf("comments: %v", c)
	}

	w.installManifest(t, "house", "permissions: "+housePerms+"\n", map[string]string{
		"notifications/heads-up.yaml": "kind: notification\nname: heads-up\ntitle: '{{record.reference}} needs a look'\n",
		"workflows/parent.yaml":       wf("parent", "tell", "  tell:\n    type: notify\n    template: heads-up\n    recipients: [requester]\n    next: done\n  done:\n    type: end\n")})
	id = w.start(t, "house", "parent", w.agent)
	w.settle(t)
	if n := w.count(t, "SELECT count(*) FROM notifications WHERE user_id = $1 AND template = 'heads-up'", w.requester); n != 1 {
		t.Errorf("the requester got %d notifications (%s)", n, w.execution(t, id).Error)
	}
}

// http.request calls an allowed host with the app's secret, keeps the answer, takes on_error when it fails, and is
// refused a host the manifest does not list and (unless allowed) a private address.
func TestAnHTTPRequestStep(t *testing.T) {
	w := setup(t)
	keys, _ := secrets.NewKeyring([]byte(strings.Repeat("k", 32)))
	w.engine.Keys, w.engine.AllowPrivateHTTP = keys, true
	var got []*http.Request
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, bodies = append(got, r), append(bodies, string(b))
		if r.URL.Path == "/broken" {
			http.Error(rw, "down", http.StatusServiceUnavailable)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"id": 77, "state": "open"}`))
	}))
	defer srv.Close()
	host := strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[0]
	call := func(path, extra string) string {
		return wf("parent", "call", "  call:\n    type: http.request\n    method: POST\n    url: '"+srv.URL+path+"'\n"+
			"    body: '={\"title\": record.title, \"priority\": record.priority}'\n    secret: token\n    auth: bearer\n"+extra+"    next: say\n"+
			"  say:\n    type: comment.add\n    body: '=\"status \" + string(vars.call_status) + \" id \" + string(vars.call_body.id)'\n    next: done\n"+
			"  oops:\n    type: comment.add\n    body: '=\"failed: \" + vars.call_error'\n    next: done\n  done:\n    type: end\n")
	}
	manifest := "permissions: [records:read:*, records:update:*, comments:internal, \"http:" + host + "\"]\nconfig:\n  - { key: token, label: API token, type: secret }\n"
	w.installManifest(t, "house", manifest, map[string]string{"workflows/parent.yaml": call("/items", "    on_error: oops\n")})
	var admin uuid.UUID
	if err := db.WithSystem(context.Background(), w.pool, func(tx pgx.Tx) error {
		u, err := dbgen.New(tx).CreateUser(context.Background(), dbgen.CreateUserParams{Email: "boss@x.test", Name: "Boss", Role: "admin"})
		admin = u.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.WithUser(context.Background(), w.pool, admin, func(tx pgx.Tx) error {
		return secrets.Set(context.Background(), dbgen.New(tx), keys, apps.AppSecretName("house", "token"), "s3cret")
	}); err != nil {
		t.Fatal(err)
	}
	id := w.start(t, "house", "parent", w.agent)
	w.settle(t)
	if ex := w.execution(t, id); ex.Status != workflow.Done {
		t.Fatalf("%s %s", ex.Status, ex.Error)
	}
	if c := w.comments(t); len(c) != 1 || c[0] != "status 200 id 77" {
		t.Errorf("comments: %v", c)
	}
	if len(got) != 1 || got[0].Header.Get("Authorization") != "Bearer s3cret" || got[0].Header.Get("Idempotency-Key") == "" ||
		!strings.Contains(bodies[0], `"title":"Printer"`) {
		t.Errorf("the request: %v %v", got, bodies)
	}

	// A 503 is tried again, then on_error.
	w.installManifest(t, "house", manifest, map[string]string{"workflows/parent.yaml": call("/broken", "    retries: 2\n    on_error: oops\n")})
	got = nil
	id = w.start(t, "house", "parent", w.agent)
	w.settle(t)
	if c := w.comments(t); len(got) != 3 || !strings.Contains(c[len(c)-1], "failed: POST") || !strings.Contains(c[len(c)-1], "answered 503") {
		t.Errorf("retries %d, comments %v (%s)", len(got), c, w.execution(t, id).Error)
	}

	// A host the manifest does not list is refused at install when written out, and at run time when worked out.
	files := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: house\nversion: 1.0.1\nauthor: t\ndescription: \"Test.\"\n" + manifest),
		"default/workflows/parent.yaml": []byte(strings.Replace(call("/items", "    on_error: oops\n"), srv.URL, "https://elsewhere.example", 1))}
	if _, err := (&apps.Bundle{Files: files}).Compile(); err == nil || !strings.Contains(err.Error(), "elsewhere.example is not a host this app may call") {
		t.Errorf("an unlisted host: %v", err)
	}

	// Private addresses are refused unless allowed (a fresh engine: the client is made once, with the setting).
	w.engine = workflowengine.New(w.pool, w.svc)
	w.engine.Keys = keys
	w.installManifest(t, "house", manifest, map[string]string{"workflows/parent.yaml": call("/items", "    on_error: oops\n")})
	id = w.start(t, "house", "parent", w.agent)
	_ = w.run(id)
	// It took on_error, with the reason.
	if c := w.comments(t); !strings.Contains(c[len(c)-1], "private network") {
		t.Errorf("a private address: %v (%s)", c, w.execution(t, id).Error)
	}
}

// Schedules: the first look only notes the time; each later due time starts the workflow once, as the app, on every
// record its for_each finds.
func TestScheduledWorkflowsStartOnTime(t *testing.T) {
	w := setup(t)
	w.installWith(t, "house", housePerms, map[string]string{"sweep": strings.Replace(wf("sweep", "say",
		"  say:\n    type: comment.add\n    body: swept\n    next: done\n  done:\n    type: end\n"), "start: say\n",
		"start: say\nschedule:\n  cron: \"0 * * * *\"\n  for_each:\n    type: incident\n    where:\n      state_category: { eq: open }\n", 1)}, nil)
	base := time.Date(2026, 11, 18, 10, 30, 0, 0, time.UTC)
	if n, err := w.engine.ScheduleTick(context.Background(), base); err != nil || n != 0 {
		t.Fatalf("the first look: %d %v", n, err)
	}
	if n, _ := w.engine.ScheduleTick(context.Background(), base.Add(10*time.Minute)); n != 0 {
		t.Errorf("before the next hour: %d", n)
	}
	n, err := w.engine.ScheduleTick(context.Background(), base.Add(31*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("at 11:00: %d %v", n, err)
	}
	if again, _ := w.engine.ScheduleTick(context.Background(), base.Add(40*time.Minute)); again != 0 {
		t.Errorf("the same hour twice: %d", again)
	}
	w.settle(t)
	if c := w.comments(t); len(c) != 1 || c[0] != "swept" {
		t.Errorf("comments: %v", c)
	}
	if n := w.count(t, "SELECT count(*) FROM workflow_executions e JOIN users u ON u.id = e.started_by WHERE e.workflow = 'sweep' AND u.app_name = 'house'"); n != 1 {
		t.Errorf("%d runs started as the app", n)
	}
}
