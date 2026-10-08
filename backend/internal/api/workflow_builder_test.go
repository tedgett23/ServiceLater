package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
)

type workflowDefBody struct {
	App        string                `json:"app"`
	Name       string                `json:"name"`
	RecordType string                `json:"record_type"`
	Start      string                `json:"start"`
	Steps      []json.RawMessage     `json:"steps"`
	Layout     map[string]apps.Point `json:"layout"`
	Edited     bool                  `json:"edited"`
	Shipped    bool                  `json:"shipped"`
	EditedBy   *string               `json:"edited_by"`
	YAML       string                `json:"yaml"`
}

func stepIDs(def workflowDefBody) string {
	var ids []string
	for _, s := range def.Steps {
		var x struct{ ID string }
		_ = json.Unmarshal(s, &x)
		ids = append(ids, x.ID)
	}
	return strings.Join(ids, ",")
}

// An admin edits a shipped workflow in the builder: the edit is checked as an install checks it, installed at once,
// kept through a sync of the bundled apps, and can be reset to the app's version.
func TestTheBuilderEditsAShippedWorkflow(t *testing.T) {
	h, pool, reg := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	const path = "/api/v1/workflows/change-management/cab-approval"

	shipped := decodeBody[workflowDefBody](t, agent.do(h, "GET", path, ""))
	if shipped.Edited || !shipped.Shipped || shipped.Start != "owners" || !strings.Contains(shipped.YAML, "kind: workflow") || len(shipped.Steps) < 5 {
		t.Fatalf("shipped: %+v", shipped)
	}
	expect(t, agent.do(h, "PUT", path, `{}`), 403, "an agent editing a workflow")

	// Add a comment before asking the board, and place the steps.
	var steps []map[string]any
	for _, raw := range shipped.Steps {
		var s map[string]any
		_ = json.Unmarshal(raw, &s)
		if s["id"] == "board" {
			s["then"] = "heads_up"
		}
		steps = append(steps, s)
	}
	steps = append(steps, map[string]any{"id": "heads_up", "type": "comment.add", "body": map[string]any{"literal": "Asking the board."}, "next": "ask"})
	save := func(steps []map[string]any) workflowDefBody {
		body, _ := json.Marshal(map[string]any{"record_type": "change", "start": "owners", "steps": steps,
			"layout": map[string]any{"board": map[string]int{"x": 0, "y": 40}}})
		w := admin.do(h, "PUT", path, string(body))
		expect(t, w, 200, "saving")
		return decodeBody[workflowDefBody](t, w)
	}
	edited := save(steps)
	if !edited.Edited || !edited.Shipped || !strings.Contains(stepIDs(edited), "heads_up") || edited.Layout["board"].Y != 40 || edited.EditedBy == nil {
		t.Fatalf("edited: %+v", edited)
	}
	// Only the change is kept, as an override that merges: the steps it did not touch still come from the app.
	stored := func() string {
		var content string
		_ = db.WithSystem(context.Background(), pool, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), "SELECT coalesce(max(content), '') FROM app_overrides WHERE app_name = 'change-management'").Scan(&content)
		})
		return content
	}
	if o := stored(); strings.Contains(o, "override: replace") || strings.Contains(o, "still_waiting") || !strings.Contains(o, "heads_up") {
		t.Errorf("the override is not just the change:\n%s", o)
	}
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if wf := reg.Workflow("change-management", "cab-approval"); wf == nil || len(wf.Steps) != len(steps) {
		t.Errorf("installed at once: %+v", wf)
	}
	list := decodeBody[struct {
		Items []struct {
			Name   string
			Edited bool
		}
	}](t, agent.do(h, "GET", "/api/v1/workflows", "")).Items
	for _, wf := range list {
		if wf.Name == "cab-approval" && !wf.Edited {
			t.Error("the list says it was edited")
		}
	}

	// A deleted step stays deleted (the edit replaces the shipped definition rather than merging into it).
	var fewer []map[string]any
	for _, s := range steps {
		if s["id"] != "heads_up" {
			fewer = append(fewer, s)
		}
		if s["id"] == "board" {
			s["then"] = "ask"
		}
	}
	if again := save(fewer); strings.Contains(stepIDs(again), "heads_up") {
		t.Errorf("a deleted step came back: %s", stepIDs(again))
	}
	if o := stored(); strings.Contains(o, "steps") || !strings.Contains(o, "layout") {
		t.Errorf("back to the app's steps, only the layout differs:\n%s", o)
	}

	// Every problem is listed, and nothing changes.
	broken := append([]map[string]any{}, fewer...)
	broken = append(broken, map[string]any{"id": "Bad id", "type": "end"}, map[string]any{"id": "lost", "type": "comment.add", "next": "nowhere"})
	body, _ := json.Marshal(map[string]any{"record_type": "change", "start": "board", "steps": broken})
	w := admin.do(h, "PUT", path, string(body))
	expect(t, w, 422, "a broken edit")
	res := decodeBody[struct{ Problems []string }](t, w)
	all := strings.Join(res.Problems, "\n")
	for _, want := range []string{`step id "Bad id"`, `there is no step "nowhere"`, "comment.add needs a body"} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q in\n%s", want, all)
		}
	}
	if still := decodeBody[workflowDefBody](t, agent.do(h, "GET", path, "")); strings.Contains(stepIDs(still), "lost") {
		t.Error("a refused edit was saved")
	}

	// Starting the server again (a sync of the bundled apps) keeps the edit; resetting goes back to the app's version.
	names, _ := apps.BundledSelection("")
	for _, r := range apps.SyncBundled(context.Background(), pool, names, "") {
		if r.Err != nil {
			t.Fatalf("sync %s: %v", r.App, r.Err)
		}
	}
	if kept := decodeBody[workflowDefBody](t, agent.do(h, "GET", path, "")); !kept.Edited || kept.Layout["board"].Y != 40 {
		t.Errorf("after a sync: %+v", kept)
	}
	expect(t, agent.do(h, "DELETE", path+"/edits", ""), 403, "an agent resetting")
	reset := decodeBody[workflowDefBody](t, admin.do(h, "DELETE", path+"/edits", ""))
	if reset.Edited || stepIDs(reset) != stepIDs(shipped) {
		t.Errorf("reset: %+v", reset)
	}
	expect(t, admin.do(h, "DELETE", path+"/edits", ""), 404, "resetting it again")
}

// A workflow created in the builder belongs to an app; removing its edits removes it.
func TestTheBuilderCreatesAWorkflow(t *testing.T) {
	h, pool, reg := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	const path = "/api/v1/workflows/core-itsm/tell-the-requester"
	expect(t, admin.do(h, "GET", path, ""), 404, "before it exists")
	created := decodeBody[workflowDefBody](t, admin.do(h, "PUT", path, `{"record_type":"incident","start":"note","steps":[
		{"id":"note","type":"comment.add","body":{"expr":"\"We are on it, \" + record.requester_name"},"next":"done"},
		{"id":"done","type":"end"}]}`))
	if !created.Edited || created.Shipped || created.RecordType != "incident" || !strings.Contains(created.YAML, `"=\"We are on it, \" + record.requester_name"`) {
		t.Fatalf("created: %+v", created)
	}
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reg.Workflow("core-itsm", "tell-the-requester") == nil {
		t.Fatal("it is installed")
	}
	expect(t, admin.do(h, "DELETE", path+"/edits", ""), 204, "removing it")
	expect(t, admin.do(h, "GET", path, ""), 404, "after it is removed")
	expect(t, admin.do(h, "GET", "/api/v1/workflows/no-such-app/x", ""), 404, "an app that is not installed")
}

// The builder shows where a workflow's runs are and how its steps went: runs in flight by step, and the visits of
// the last week by outcome.
func TestTheBuilderShowsWhereRunsAre(t *testing.T) {
	h, pool, _ := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	const path = "/api/v1/workflows/change-management/cab-approval/activity"
	ch := decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Patch the edge"}`))
	run := decodeBody[struct{ ID string }](t, agent.do(h, "POST", "/api/v1/workflow-runs", `{"workflow":"change-management/cab-approval","ticket":"`+ch.ID+`"}`))
	if run.ID == "" {
		t.Fatal("no run started")
	}
	// What the engine would have recorded for an earlier run: through board, failed at ask.
	ctx := context.Background()
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workflow_steps (execution_id, seq, step_id, type, outcome) VALUES
			($1, 1, 'board', 'condition', 'ok'), ($1, 2, 'ask', 'approval', 'failed')`, run.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	type activityBody struct {
		Steps map[string]struct {
			Now, Visits, Failed int64
		}
		InFlight []struct {
			ID, Step, Reference string
		} `json:"in_flight"`
		Runs map[string]int64
	}
	got := decodeBody[activityBody](t, agent.do(h, "GET", path, ""))
	if got.Steps["owners"].Now != 1 || len(got.InFlight) != 1 || got.InFlight[0].Reference != ch.Reference || got.InFlight[0].Step != "owners" {
		t.Errorf("the run waits at its first step: %+v", got)
	}
	if got.Steps["ask"].Visits != 1 || got.Steps["ask"].Failed != 1 || got.Steps["board"].Visits != 1 {
		t.Errorf("visits by outcome: %+v", got.Steps)
	}
	if got.Runs["queued"] != 1 {
		t.Errorf("runs by status: %+v", got.Runs)
	}
	expect(t, lena.do(h, "GET", path, ""), 403, "an end user")
}
