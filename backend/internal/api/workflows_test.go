package api_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/ruleworker"
	"github.com/servicelater/servicelater/backend/internal/workflow"
	"github.com/servicelater/servicelater/backend/internal/workflowengine"
)

// installFlows installs an app of workflows and rules on top of the first-party apps and reloads the registry.
func installFlows(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry, workflows, rules map[string]string) {
	t.Helper()
	files := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: house-flows\nversion: 1.0.0\nauthor: t\ndescription: \"Flows.\"\npermissions: [records:read:*, records:update:*, comments:internal]\n")}
	for name, src := range workflows {
		files["default/workflows/"+name+".yaml"] = []byte(src)
	}
	for name, src := range rules {
		files["default/rules/"+name+".yaml"] = []byte(src)
	}
	c, err := (&apps.Bundle{Files: files}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Install(context.Background(), pool, c, apps.InstallOptions{ApprovePermissions: true}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// startFlowWorker runs a real worker with both handlers and the workflow timer, as the worker role does.
func startFlowWorker(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := jobs.NewWorker(pool, jobs.Options{Concurrency: 2, PollInterval: 50 * time.Millisecond, Grace: time.Second})
	w.Register(ruleruns.Kind, ruleworker.Handler(pool, reg))
	engine := workflowengine.New(pool, &records.Service{Apps: reg, Pool: pool})
	w.Register(workflow.RunKind, engine.Handler())
	done := make(chan struct{}, 2)
	go func() { w.Run(ctx); done <- struct{}{} }()
	go func() { engine.RunTimer(ctx, 50*time.Millisecond); done <- struct{}{} }()
	t.Cleanup(func() { cancel(); <-done; <-done })
}

type run struct {
	ID          string  `json:"id"`
	App         string  `json:"app"`
	Workflow    string  `json:"workflow"`
	TicketID    string  `json:"ticket_id"`
	Reference   string  `json:"reference"`
	Status      string  `json:"status"`
	CurrentStep string  `json:"current_step"`
	StepsRun    int     `json:"steps_run"`
	Error       string  `json:"error"`
	WakeAt      *string `json:"wake_at"`
	Start       string  `json:"start"`
	Graph       []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Next []struct {
			To   string `json:"to"`
			Kind string `json:"kind"`
		} `json:"next"`
	} `json:"graph"`
	Vars  map[string]any `json:"vars"`
	Steps []struct {
		Seq     int             `json:"seq"`
		StepID  string          `json:"step_id"`
		Type    string          `json:"type"`
		Outcome string          `json:"outcome"`
		Detail  json.RawMessage `json:"detail"`
		Error   string          `json:"error"`
	} `json:"steps"`
}

type runList struct {
	Items   []run `json:"items"`
	HasMore bool  `json:"has_more"`
}

func wfSrc(name, start, steps string) string {
	return "kind: workflow\nname: " + name + "\nrecord_type: incident\nstart: " + start + "\nsteps:\n" + steps
}

const triage = `  tag:
    type: set
    vars: { level: '=record.priority' }
    next: bump
  bump:
    type: record.update
    set:
      priority: 2
    next: say
  say:
    type: comment.add
    body: '="triaged from " + string(vars.level)'
    internal: true
    next: done
  done:
    type: end
`

const napFlow = `  nap:
    type: wait
    for: 1h
    next: done
  done:
    type: end
`

const startOnCreate = "kind: rule\nname: triage-new\nrecord_type: incident\ntiming: after\non: [create]\nrun:\n  - action: workflow.start\n    workflow: triage\n"

func TestARuleStartsAWorkflowThatRunsInAWorkerAndShowsInTheViewer(t *testing.T) {
	h, pool, reg := setupApps(t)
	installFlows(t, pool, reg, map[string]string{"triage": wfSrc("triage", "tag", triage)}, map[string]string{"start": startOnCreate})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	startFlowWorker(t, pool, reg)

	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Printer","priority":4}`))
	var r run
	waitFor(t, "the workflow to finish", func() bool {
		l := decodeBody[runList](t, agent.do(h, "GET", "/api/v1/workflow-runs?ticket="+tk.ID, ""))
		if len(l.Items) != 1 {
			return false
		}
		r = l.Items[0]
		return r.Status == "done"
	})
	if r.Workflow != "triage" || r.App != "house-flows" || r.Reference == "" || !strings.HasPrefix(r.Reference, "INC") || r.StepsRun != 4 {
		t.Errorf("run: %+v", r)
	}
	// The workflow acted on the ticket, as the person who filed it.
	if got := decodeBody[ticket](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")); got.Priority != 2 {
		t.Errorf("priority %d, want 2 from the workflow", got.Priority)
	}
	cs := decodeBody[commentList](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/comments", ""))
	if len(cs.Items) != 1 || cs.Items[0].Body != "triaged from 4" || !cs.Items[0].Internal {
		t.Errorf("comments: %+v", cs.Items)
	}

	// One run in the viewer: the graph it was started with, its variables, and each step's outcome.
	one := decodeBody[run](t, agent.do(h, "GET", "/api/v1/workflow-runs/"+r.ID, ""))
	if one.Start != "tag" || len(one.Graph) != 4 || one.Graph[0].ID != "tag" || one.Graph[0].Next[0].To != "bump" ||
		one.Graph[0].Next[0].Kind != "" || one.Graph[3].Type != "end" {
		t.Errorf("start %q, graph: %+v", one.Start, one.Graph)
	}
	if one.Vars["level"] != float64(4) || len(one.Steps) != 4 || one.Steps[1].StepID != "bump" || one.Steps[1].Outcome != "ok" ||
		!strings.Contains(string(one.Steps[1].Detail), "priority") {
		t.Errorf("run: vars %v, steps %+v", one.Vars, one.Steps)
	}
}

func TestOnlyStaffSeeAndDriveRuns(t *testing.T) {
	h, pool, reg := setupApps(t)
	installFlows(t, pool, reg, map[string]string{"nap": wfSrc("nap", "nap", napFlow)}, nil)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	other := newActor(t, h, pool, "other@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t"}`))

	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/workflow-runs", ""}, {"GET", "/api/v1/workflows", ""},
		{"POST", "/api/v1/workflow-runs", `{"workflow":"house-flows/nap","ticket":"` + tk.ID + `"}`},
	} {
		expect(t, do(h, c.method, c.path, c.body, nil), 401, "anonymous "+c.method+" "+c.path)
		expect(t, lena.do(h, c.method, c.path, c.body), 403, "requester "+c.method+" "+c.path)
	}

	w := agent.do(h, "POST", "/api/v1/workflow-runs", `{"workflow":"house-flows/nap","ticket":"`+tk.ID+`"}`)
	expect(t, w, 201, "start by hand")
	r := decodeBody[run](t, w)
	if r.Status != "queued" || r.Workflow != "nap" || r.TicketID != tk.ID || w.Header().Get("Location") != "/api/v1/workflow-runs/"+r.ID {
		t.Errorf("started: %+v", r)
	}
	// Any staff member sees every run; a requester does not, even by id.
	expect(t, other.do(h, "GET", "/api/v1/workflow-runs/"+r.ID, ""), 200, "another agent")
	expect(t, lena.do(h, "GET", "/api/v1/workflow-runs/"+r.ID, ""), 403, "requester by id")
	if l := decodeBody[runList](t, other.do(h, "GET", "/api/v1/workflow-runs", "")); len(l.Items) != 1 {
		t.Errorf("another agent sees %d runs", len(l.Items))
	}
	ws := decodeBody[struct {
		Items []struct{ App, Name, RecordType string } `json:"items"`
	}](t, agent.do(h, "GET", "/api/v1/workflows", ""))
	// The test app's workflow is listed among the first-party ones (change-management ships its boards).
	found := false
	for _, x := range ws.Items {
		found = found || (x.App == "house-flows" && x.Name == "nap")
	}
	if !found {
		t.Errorf("workflows: %+v", ws)
	}
}

func TestStartingByHandChecksWhatItStarts(t *testing.T) {
	h, pool, reg := setupApps(t)
	installFlows(t, pool, reg, map[string]string{"nap": wfSrc("nap", "nap", napFlow),
		"changes": strings.Replace(wfSrc("changes", "nap", napFlow), "record_type: incident", "record_type: change", 1)}, nil)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t"}`))
	start := func(wf, ticket string) int {
		return agent.do(h, "POST", "/api/v1/workflow-runs", `{"workflow":"`+wf+`","ticket":"`+ticket+`"}`).Code
	}
	if c := start("house-flows/nap", "INC"+strconv.FormatInt(tk.Number, 10)); c != 201 {
		t.Errorf("by reference: %d", c)
	}
	for name, c := range map[string]struct {
		wf, ticket string
		want       int
	}{
		"a workflow that does not exist":        {"house-flows/nope", tk.ID, 404},
		"a ticket that does not exist":          {"house-flows/nap", "INC9999", 404},
		"a workflow for another kind of ticket": {"house-flows/changes", tk.ID, 422},
		"a workflow name without its app":       {"nap", tk.ID, 400},
		"no ticket":                             {"house-flows/nap", "", 400},
	} {
		if got := start(c.wf, c.ticket); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
	expect(t, agent.do(h, "GET", "/api/v1/workflow-runs?status=sleeping", ""), 400, "unknown status")
	expect(t, agent.do(h, "GET", "/api/v1/workflow-runs?limit=0", ""), 400, "bad limit")
	expect(t, agent.do(h, "GET", "/api/v1/workflow-runs?ticket=INC9999", ""), 404, "filter by a missing ticket")
	expect(t, agent.do(h, "GET", "/api/v1/workflow-runs/not-a-uuid", ""), 404, "bad id")
}

func TestARunCanBeCancelledOnce(t *testing.T) {
	h, pool, reg := setupApps(t)
	installFlows(t, pool, reg, map[string]string{"nap": wfSrc("nap", "nap", napFlow)}, nil)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	startFlowWorker(t, pool, reg)
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t"}`))
	r := decodeBody[run](t, agent.do(h, "POST", "/api/v1/workflow-runs", `{"workflow":"house-flows/nap","ticket":"`+tk.ID+`"}`))
	waitFor(t, "it to go to sleep", func() bool {
		return decodeBody[run](t, agent.do(h, "GET", "/api/v1/workflow-runs/"+r.ID, "")).Status == "waiting"
	})
	if got := decodeBody[run](t, agent.do(h, "GET", "/api/v1/workflow-runs/"+r.ID, "")); got.WakeAt == nil {
		t.Error("a sleeping run says when it wakes")
	}
	w := agent.do(h, "POST", "/api/v1/workflow-runs/"+r.ID+"/cancel", "")
	expect(t, w, 200, "cancel")
	if got := decodeBody[run](t, w); got.Status != "cancelled" {
		t.Errorf("status %s", got.Status)
	}
	expect(t, agent.do(h, "POST", "/api/v1/workflow-runs/"+r.ID+"/cancel", ""), 409, "cancel again")
	expect(t, agent.do(h, "POST", "/api/v1/workflow-runs/"+"00000000-0000-0000-0000-000000000000"+"/cancel", ""), 404, "cancel a missing run")
	l := decodeBody[runList](t, agent.do(h, "GET", "/api/v1/workflow-runs?status=cancelled", ""))
	if len(l.Items) != 1 || l.Items[0].ID != r.ID {
		t.Errorf("cancelled runs: %+v", l.Items)
	}
}

// A failed run says which step failed and why, so an administrator can act on it.
func TestAFailedRunSaysWhyInTheViewer(t *testing.T) {
	h, pool, reg := setupApps(t)
	installFlows(t, pool, reg, map[string]string{"bad": wfSrc("bad", "one", "  one:\n    type: comment.add\n    body: fine\n    next: two\n  two:\n    type: set\n    vars:\n      x: '=string(record.priority / (record.priority - 3))'\n    next: three\n  three:\n    type: end\n")}, nil)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	startFlowWorker(t, pool, reg)
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"t","priority":3}`))
	r := decodeBody[run](t, agent.do(h, "POST", "/api/v1/workflow-runs", `{"workflow":"house-flows/bad","ticket":"`+tk.ID+`"}`))
	waitFor(t, "the run to fail", func() bool {
		return decodeBody[run](t, agent.do(h, "GET", "/api/v1/workflow-runs/"+r.ID, "")).Status == "failed"
	})
	got := decodeBody[run](t, agent.do(h, "GET", "/api/v1/workflow-runs/"+r.ID, ""))
	if got.CurrentStep != "two" || !strings.Contains(got.Error, "could not be evaluated") || len(got.Steps) != 2 ||
		got.Steps[0].Outcome != "ok" || got.Steps[1].Outcome != "failed" || got.Steps[1].Error == "" {
		t.Errorf("run: %+v", got)
	}
}
