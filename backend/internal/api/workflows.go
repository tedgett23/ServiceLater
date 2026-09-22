package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/workflow"
)

// runDTO is a workflow execution as the run viewer shows it.
type runDTO struct {
	ID          uuid.UUID  `json:"id"`
	App         string     `json:"app"`
	Workflow    string     `json:"workflow"`
	AppVersion  string     `json:"app_version"`
	TicketID    *uuid.UUID `json:"ticket_id"`
	Reference   string     `json:"reference"`
	StartedBy   uuid.UUID  `json:"started_by"`
	Status      string     `json:"status"`
	CurrentStep string     `json:"current_step"`
	StepsRun    int32      `json:"steps_run"`
	Error       string     `json:"error"`
	WakeAt      *time.Time `json:"wake_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	FinishedAt  *time.Time `json:"finished_at"`

	// Only on a single run: what the workflow looked like when it started, the variables now, and what happened.
	Graph []graphStep `json:"graph,omitempty"`
	Vars  any         `json:"vars,omitempty"`
	Steps []stepDTO   `json:"steps,omitempty"`
}

type graphStep struct {
	ID   string   `json:"id"`
	Type string   `json:"type"`
	Next []string `json:"next"`
}

type stepDTO struct {
	Seq        int32           `json:"seq"`
	StepID     string          `json:"step_id"`
	Type       string          `json:"type"`
	Outcome    string          `json:"outcome"`
	Detail     json.RawMessage `json:"detail"`
	Error      string          `json:"error"`
	DurationMs int32           `json:"duration_ms"`
	At         time.Time       `json:"at"`
}

func ts(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

func toRun(e dbgen.WorkflowExecution, reference string) runDTO {
	d := runDTO{ID: e.ID, App: e.App, Workflow: e.Workflow, AppVersion: e.AppVersion, Reference: reference, StartedBy: e.StartedBy,
		Status: e.Status, CurrentStep: e.CurrentStep, StepsRun: e.StepsRun, Error: e.Error, WakeAt: ts(e.WakeAt),
		CreatedAt: e.CreatedAt.Time, UpdatedAt: e.UpdatedAt.Time, FinishedAt: ts(e.FinishedAt)}
	if e.RecordID.Valid {
		id := e.RecordID.UUID
		d.TicketID = &id
	}
	return d
}

// graphOf reads the pinned definition into the shape the viewer draws.
func graphOf(definition []byte) []graphStep {
	var wf apps.Workflow
	if json.Unmarshal(definition, &wf) != nil {
		return nil
	}
	out := make([]graphStep, 0, len(wf.Steps))
	for _, s := range wf.Steps {
		g := graphStep{ID: s.ID, Type: s.Type, Next: []string{}}
		for _, n := range []string{s.Next, s.Then, s.Else, s.OnTimeout} {
			if n != "" {
				g.Next = append(g.Next, n)
			}
		}
		out = append(out, g)
	}
	return out
}

type workflowDTO struct {
	App        string `json:"app"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	RecordType string `json:"record_type"`
	Steps      int    `json:"steps"`
}

// GET /workflows (staff): what can be started by hand.
func (h *handlers) listWorkflows(w http.ResponseWriter, _ *http.Request) {
	out := []workflowDTO{}
	for _, wf := range h.apps.Workflows() {
		out = append(out, workflowDTO{App: wf.App, Name: wf.Name, Version: wf.Version, RecordType: wf.RecordType, Steps: len(wf.Steps)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// GET /workflow-runs?status=&ticket=&workflow=&limit=&offset= (staff)
func (h *handlers) listRuns(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	limit, offset := defaultLimit, 0
	if !intParam(w, qs.Get("limit"), "limit", 1, maxLimit, &limit) || !intParam(w, qs.Get("offset"), "offset", 0, 1<<30, &offset) {
		return
	}
	arg := dbgen.ListExecutionViewsParams{Limit: int32(limit + 1), Offset: int32(offset)} //nolint:gosec // bounded by intParam
	if s := qs.Get("status"); s != "" {
		switch s {
		case workflow.Queued, workflow.Running, workflow.Waiting, workflow.Done, workflow.Failed, workflow.Cancelled:
			arg.Status = pgtype.Text{String: s, Valid: true}
		default:
			writeError(w, http.StatusBadRequest, "unknown status")
			return
		}
	}
	if s := qs.Get("workflow"); s != "" {
		arg.Workflow = pgtype.Text{String: s, Valid: true}
	}
	var out []runDTO
	more := false
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if s := qs.Get("ticket"); s != "" {
			t, err := resolveTicket(r.Context(), q, s)
			if err != nil {
				return err
			}
			arg.RecordID = uuid.NullUUID{UUID: t.ID, Valid: true}
		}
		rows, err := q.ListExecutionViews(r.Context(), arg)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			rows, more = rows[:limit], true
		}
		for _, row := range rows {
			out = append(out, toRun(row.WorkflowExecution, row.Reference))
		}
		return nil
	}) {
		return
	}
	if out == nil {
		out = []runDTO{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "limit": limit, "offset": offset, "has_more": more})
}

// GET /workflow-runs/{id} (staff)
func (h *handlers) getRun(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var out runDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		v, err := q.GetExecutionView(r.Context(), id)
		if err != nil {
			return err
		}
		out = toRun(v.WorkflowExecution, v.Reference)
		out.Graph = graphOf(v.WorkflowExecution.Definition)
		_ = json.Unmarshal(v.WorkflowExecution.Vars, &out.Vars)
		steps, err := q.ListSteps(r.Context(), id)
		if err != nil {
			return err
		}
		out.Steps = make([]stepDTO, 0, len(steps))
		for _, s := range steps {
			out.Steps = append(out.Steps, stepDTO{Seq: s.Seq, StepID: s.StepID, Type: s.Type, Outcome: s.Outcome, Detail: s.Detail, Error: s.Error, DurationMs: s.DurationMs, At: s.CreatedAt.Time})
		}
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /workflow-runs (staff) with {"workflow": "app/name", "ticket": "<id or reference>"}.
func (h *handlers) startRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Workflow string `json:"workflow"`
		Ticket   string `json:"ticket"`
	}
	if !decode(w, r, &req) {
		return
	}
	app, name, ok := strings.Cut(req.Workflow, "/")
	if !ok || app == "" || name == "" || req.Ticket == "" {
		writeError(w, http.StatusBadRequest, `both "workflow" (as "app/name") and "ticket" are required`)
		return
	}
	var out runDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		wf := h.apps.Workflow(app, name)
		if wf == nil {
			return notFound()
		}
		row, err := resolveTicket(r.Context(), q, req.Ticket)
		if err != nil {
			return err
		}
		t, err := records.Load(r.Context(), q, row.ID)
		if err != nil {
			return err
		}
		id, err := h.svc.StartWorkflow(r.Context(), q, actorOf(p), ruleruns.Cause{}, t, wf)
		if err != nil {
			return err
		}
		v, err := q.GetExecutionView(r.Context(), id)
		out = toRun(v.WorkflowExecution, v.Reference)
		return err
	}) {
		return
	}
	w.Header().Set("Location", "/api/v1/workflow-runs/"+out.ID.String())
	writeJSON(w, http.StatusCreated, out)
}

// POST /workflow-runs/{id}/cancel (staff)
func (h *handlers) cancelRun(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var out runDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if _, err := q.GetExecution(r.Context(), id); err != nil { // hidden or missing: 404
			return err
		}
		n, err := q.CancelExecution(r.Context(), id)
		if err != nil {
			return err
		}
		if n == 0 {
			return records.Conflict("this run has already finished")
		}
		v, err := q.GetExecutionView(r.Context(), id)
		out = toRun(v.WorkflowExecution, v.Reference)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
