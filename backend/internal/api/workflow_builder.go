package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// workflowDefDTO is a workflow as the builder edits it: the same definition its YAML file holds.
type workflowDefDTO struct {
	App        string `json:"app"`
	Name       string `json:"name"`
	RecordType string `json:"record_type"`
	Start      string `json:"start"`
	// Steps are in the order they are written; which fields a step has depends on its type (see the workflow docs).
	Steps []apps.Step `json:"steps"`
	// Layout is where each step sits in the diagram; steps without a place are laid out from the graph.
	Layout map[string]apps.Point `json:"layout"`
	// Schedule starts it by the clock, if it has one.
	Schedule *apps.Schedule `json:"schedule,omitempty"`
	// Webhook starts it when another system calls POST /api/v1/hooks/{app}/{name}, if it has one.
	Webhook *apps.Webhook `json:"webhook,omitempty"`
	// WebhookSecretSet says whether the webhook's secret has been set (Admin → Connections); until it is, every call
	// is refused.
	WebhookSecretSet bool `json:"webhook_secret_set"`
	// AppWorkflows are the app's workflows (what subflow and foreach may run); AppSecrets the secrets and AppHosts
	// the hosts its http.request steps may use.
	AppWorkflows []string `json:"app_workflows"`
	AppSecrets   []string `json:"app_secrets"`
	AppHosts     []string `json:"app_hosts"`
	// Edited says it was changed in ServiceLater (the app's studio layer); Shipped that the app itself defines it, so
	// resetting an edited one goes back to the app's version rather than removing it.
	Edited   bool       `json:"edited"`
	Shipped  bool       `json:"shipped"`
	EditedAt *time.Time `json:"edited_at,omitempty"`
	EditedBy *string    `json:"edited_by,omitempty"`
	// Layers are where the definition comes from: default, local, instance (files on the server) and studio.
	Layers []string `json:"layers"`
	// YAML is the definition as a file, for those who keep apps in version control.
	YAML string `json:"yaml"`
	// Override is the edit as it is kept (only what it changes, over the app's version): what
	// `servicelater app export-local` writes, as the app's local/ file.
	Override string `json:"override,omitempty"`
	// Path is the file the definition and its override are kept in, within an app's layers (workflows/cab.yaml).
	Path string `json:"path"`
	// AppPermissions are what the app may do (its manifest's permissions): a workflow's steps act as the app, for
	// whoever started it, so a step needs one of these to read or change a record.
	AppPermissions []string `json:"app_permissions"`
}

// workflowSaveRequest is a workflow as the builder saves it.
type workflowSaveRequest struct {
	RecordType string                `json:"record_type"`
	Start      string                `json:"start"`
	Steps      []apps.Step           `json:"steps"`
	Layout     map[string]apps.Point `json:"layout"`
	// Schedule starts it by the clock: cron, timezone and, to run once per record, for_each.
	Schedule *apps.Schedule `json:"schedule"`
	// Webhook starts it when another system calls it: secret, auth (token or hmac) and header names.
	Webhook *apps.Webhook `json:"webhook"`
}

// problemsError is an app that does not compile with an edit: every problem, as `servicelater app validate` would say.
type problemsError struct {
	msg      string
	problems []string
}

func (e *problemsError) Error() string { return e.msg }

// studioApp is an installed app as files with every layer, compiled: what the builder edits.
type studioApp struct {
	bundle   *apps.Bundle
	compiled *apps.Compiled
}

// loadStudioApp reads an installed app's files and layers as the system and compiles them.
func (h *handlers) loadStudioApp(ctx context.Context, app string) (*studioApp, error) {
	if !slices.ContainsFunc(h.apps.Apps(), func(a apps.Installed) bool { return a.Name == app }) {
		return nil, notFound()
	}
	var b *apps.Bundle
	err := db.WithSystem(ctx, h.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var err error
		if b, err = apps.StoredBundle(ctx, q, app); err != nil {
			return &apiError{http.StatusConflict, err.Error()}
		}
		b.Studio, err = apps.LoadStudio(ctx, q, app)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := b.AttachInstanceLocal(apps.LocalRoot()); err != nil {
		return nil, err
	}
	c, err := b.Compile()
	if err != nil {
		return &studioApp{bundle: b}, problems(app+" does not compile as it is now", err)
	}
	return &studioApp{bundle: b, compiled: c}, nil
}

func problems(msg string, err error) error {
	var p apps.Problems
	if !errors.As(err, &p) {
		return err
	}
	out := &problemsError{msg: msg}
	for _, x := range p {
		out.problems = append(out.problems, problemText(x))
	}
	return out
}

// problemText is a problem as an editor shows it: one in a workflow or form is about what is on screen; one elsewhere
// (a rule naming it) says which file.
func problemText(x apps.Problem) string {
	if x.File != "" && !strings.Contains(x.File, "workflows/") && !strings.Contains(x.File, "forms/") {
		return x.File + ": " + x.Message
	}
	return x.Message
}

// definition finds a workflow among the compiled app's definitions.
func (s *studioApp) definition(name string) (apps.Definition, bool) {
	if s.compiled == nil {
		return apps.Definition{}, false
	}
	for _, d := range s.compiled.Definitions {
		if d.Kind == "workflow" && d.Name == name {
			return d, true
		}
	}
	return apps.Definition{}, false
}

func (h *handlers) toWorkflowDef(ctx context.Context, c *apps.Compiled, d apps.Definition) (workflowDefDTO, error) {
	wf, app := d.Workflow, c.Manifest.Name
	perms := append([]string{}, c.Manifest.Permissions...)
	out := workflowDefDTO{App: app, AppPermissions: perms, Name: wf.Name, RecordType: wf.RecordType, Start: wf.Start, Steps: wf.Steps, Layout: wf.Layout,
		Layers: d.Layers, Edited: slices.Contains(d.Layers, "studio"), Path: d.Path,
		Shipped: slices.ContainsFunc(d.Layers, func(l string) bool { return l != "studio" })}
	if out.Layout == nil {
		out.Layout = map[string]apps.Point{}
	}
	out.Schedule, out.Webhook = wf.Schedule, wf.Webhook
	if wf.Webhook != nil && wf.Webhook.Secret != "" {
		// Only whether it is set: the builder links to Connections, which those who manage settings use.
		err := db.WithSystem(ctx, h.pool, func(tx pgx.Tx) error {
			_, err := dbgen.New(tx).GetSecret(ctx, apps.AppSecretName(app, wf.Webhook.Secret))
			out.WebhookSecretSet = err == nil
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		})
		if err != nil {
			return out, err
		}
	}
	out.AppWorkflows, out.AppSecrets, out.AppHosts = []string{}, []string{}, apps.HTTPHosts(c.Manifest.Permissions)
	if out.AppHosts == nil {
		out.AppHosts = []string{}
	}
	for _, x := range c.Definitions {
		if x.Workflow != nil {
			out.AppWorkflows = append(out.AppWorkflows, x.Workflow.Name)
		}
	}
	slices.Sort(out.AppWorkflows)
	for _, item := range c.Manifest.Config {
		if item.Type == "secret" {
			out.AppSecrets = append(out.AppSecrets, item.Key)
		}
	}
	data, err := apps.WorkflowYAML(wf, false)
	if err != nil {
		return out, err
	}
	out.YAML = string(data)
	if out.Edited {
		err = db.WithSystem(ctx, h.pool, func(tx pgx.Tx) error {
			o, err := dbgen.New(tx).GetAppOverride(ctx, dbgen.GetAppOverrideParams{AppName: app, Path: d.Path})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			out.EditedAt, out.EditedBy, out.Override = &o.UpdatedAt.Time, textPtr(o.UpdatedByName), o.Content
			return err
		})
	}
	return out, err
}

// writeStudioError answers for a failed builder request: the problems with an edit as a list, anything else as usual.
func writeStudioError(w http.ResponseWriter, err error) {
	var pe *problemsError
	if errors.As(err, &pe) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": pe.msg, "problems": pe.problems})
		return
	}
	writeDBError(w, err)
}

// GET /workflows/{app}/{name} (workflows:read_all or workflows:edit): a workflow's definition for the builder, with
// where it comes from, whether it was edited here, and its YAML.
func (h *handlers) getWorkflowDef(w http.ResponseWriter, r *http.Request) {
	app, name := chi.URLParam(r, "app"), chi.URLParam(r, "name")
	s, err := h.loadStudioApp(r.Context(), app)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	d, ok := s.definition(name)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	out, err := h.toWorkflowDef(r.Context(), s.compiled, d)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// installStudio reinstalls an app with one file of its studio layer changed (content nil removes it), recording the
// change with the install: an edit that does not compile, or that the install refuses, changes nothing.
func (h *handlers) installStudio(r *http.Request, app, path string, content []byte) (*apps.Compiled, error) {
	p, _ := auth.PrincipalFrom(r.Context())
	c, _, err := apps.UpdateStudio(r.Context(), h.pool, apps.StudioUpdate{App: app, Files: map[string][]byte{path: content},
		UpdatedBy: nullUUID(&p.UserID), InstalledBy: "builder"})
	var pr apps.Problems
	switch {
	case errors.As(err, &pr):
		return nil, problems("the workflow cannot be saved", err)
	case err != nil:
		return nil, &problemsError{msg: "the workflow cannot be saved", problems: []string{err.Error()}}
	}
	return c, nil
}

// PUT /workflows/{app}/{name} (workflows:edit) with {"record_type", "start", "steps", "layout"}: save a workflow into
// the app's studio layer, creating it if the app has none of that name, and reinstall the app with it at once. It is
// checked as an install checks it; every problem is listed (422 with problems). Upgrades of the app keep the edit.
func (h *handlers) putWorkflowDef(w http.ResponseWriter, r *http.Request) {
	app, name := chi.URLParam(r, "app"), chi.URLParam(r, "name")
	var req workflowSaveRequest
	if !decode(w, r, &req) {
		return
	}
	s, err := h.loadStudioApp(r.Context(), app)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	wf := &apps.Workflow{Name: name, RecordType: req.RecordType, Start: req.Start, Steps: req.Steps, Layout: req.Layout, Schedule: req.Schedule, Webhook: req.Webhook}
	path := "workflows/" + name + ".yaml"
	var current *apps.Workflow
	if d, ok := s.definition(name); ok {
		path, current = d.Path, d.Workflow
	}
	// Workflows are not scripts: a new or changed script needs scripts:edit as well.
	if p, _ := auth.PrincipalFrom(r.Context()); scriptsChanged(current, wf) && !p.Can(authz.ScriptsEdit) {
		writeError(w, http.StatusForbidden, "changing a script needs the scripts:edit permission as well as workflows:edit")
		return
	}
	// Over a workflow the app (or a local file) defines, only what changed is kept, so the app's later changes to
	// the rest still arrive; a new one is kept whole.
	var content []byte
	base, below, err := s.bundle.Below(path)
	switch {
	case err != nil:
		writeDBError(w, err)
		return
	case below:
		content, err = apps.WorkflowOverride(base, wf)
	default:
		content, err = apps.WorkflowYAML(wf, false)
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	c, err := h.installStudio(r, app, path, content)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	saved := studioApp{bundle: s.bundle, compiled: c}
	d, _ := saved.definition(name)
	out, err := h.toWorkflowDef(r.Context(), c, d)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /workflows/{app}/{name}/edits (workflows:edit): undo the edits made here: a workflow the app ships goes back
// to the app's version; one created here is removed. Runs in progress keep the version they started with.
func (h *handlers) resetWorkflowDef(w http.ResponseWriter, r *http.Request) {
	app, name := chi.URLParam(r, "app"), chi.URLParam(r, "name")
	s, err := h.loadStudioApp(r.Context(), app)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	d, ok := s.definition(name)
	if !ok || !slices.Contains(d.Layers, "studio") {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s has no edits made here", name))
		return
	}
	c, err := h.installStudio(r, app, d.Path, nil)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	reset := studioApp{bundle: s.bundle, compiled: c}
	shipped, ok := reset.definition(name)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	out, err := h.toWorkflowDef(r.Context(), c, shipped)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// workflowEditedNames lists the workflows (app/name) edited here, from where each installed definition came from.
func (h *handlers) workflowEditedNames(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	err := db.WithSystem(ctx, h.pool, func(tx pgx.Tx) error {
		defs, err := dbgen.New(tx).ListCurrentDefinitions(ctx)
		for _, d := range defs {
			if d.Kind == "workflow" && strings.Contains(d.Source, "studio") {
				out[d.AppName+"/"+d.Name] = true
			}
		}
		return err
	})
	return out, err
}

// activityWindow is how far back the builder's step counts look.
const activityWindow = 7 * 24 * time.Hour

// stepActivityDTO is what is happening at one step.
type stepActivityDTO struct {
	// Now is how many runs are at this step at the moment (queued, running or waiting there).
	Now int64 `json:"now"`
	// Visits, Failed and Timeouts count the visits of runs started in the window: all of them, the ones that failed
	// the run there, and the waits that ran out.
	Visits   int64 `json:"visits"`
	Failed   int64 `json:"failed"`
	Timeouts int64 `json:"timeouts"`
}

// inFlightRunDTO is a run still going.
type inFlightRunDTO struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Step   string `json:"step"`
	// Reference and Title are the record it is on, when the reader may see it.
	Reference string     `json:"reference,omitempty"`
	Title     string     `json:"title,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	WakeAt    *time.Time `json:"wake_at,omitempty"`
}

// workflowActivityDTO is where a workflow's runs are now, and how its steps went lately.
type workflowActivityDTO struct {
	// Steps are by step id. A run started on an older version may be at a step this one no longer has.
	Steps map[string]stepActivityDTO `json:"steps"`
	// InFlight are the runs still going, oldest first (at most 100); More says there are more.
	InFlight []inFlightRunDTO `json:"in_flight"`
	More     bool             `json:"more"`
	// Runs counts the runs started in the window by how they stand: done, failed, cancelled, or still going.
	Runs  map[string]int64 `json:"runs"`
	Since time.Time        `json:"since"`
}

// GET /workflows/{app}/{name}/activity (workflows:read_all or workflows:edit): where the workflow's runs are right
// now, and how each step went for the runs started in the last 7 days, for the builder to show on its diagram. It
// changes as runs move: the run.changed event says when to ask again.
func (h *handlers) getWorkflowActivity(w http.ResponseWriter, r *http.Request) {
	app, name := chi.URLParam(r, "app"), chi.URLParam(r, "name")
	since := time.Now().Add(-activityWindow)
	out := workflowActivityDTO{Steps: map[string]stepActivityDTO{}, InFlight: []inFlightRunDTO{}, Runs: map[string]int64{}, Since: since}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		flying, err := q.WorkflowInFlight(r.Context(), dbgen.WorkflowInFlightParams{App: app, Workflow: name})
		if err != nil {
			return err
		}
		if len(flying) > 100 {
			flying, out.More = flying[:100], true
		}
		for _, e := range flying {
			run := inFlightRunDTO{ID: e.ID.String(), Status: e.Status, Step: e.CurrentStep, StartedAt: e.CreatedAt.Time}
			if e.RecordReference != "" {
				run.Reference, run.Title = e.RecordReference, e.RecordTitle.String
			}
			if e.WakeAt.Valid {
				run.WakeAt = &e.WakeAt.Time
			}
			out.InFlight = append(out.InFlight, run)
			s := out.Steps[e.CurrentStep]
			s.Now++
			out.Steps[e.CurrentStep] = s
		}
		window := pgtype.Timestamptz{Time: since, Valid: true}
		outcomes, err := q.WorkflowStepOutcomes(r.Context(), dbgen.WorkflowStepOutcomesParams{App: app, Workflow: name, Since: window})
		if err != nil {
			return err
		}
		for _, o := range outcomes {
			s := out.Steps[o.StepID]
			s.Visits += o.N
			switch o.Outcome {
			case "failed":
				s.Failed += o.N
			case "timeout":
				s.Timeouts += o.N
			}
			out.Steps[o.StepID] = s
		}
		counts, err := q.WorkflowRunCounts(r.Context(), dbgen.WorkflowRunCountsParams{App: app, Workflow: name, Since: window})
		for _, c := range counts {
			out.Runs[c.Status] = c.N
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// scriptsChanged reports whether a save adds or changes the Starlark of any step (taking one out does not).
func scriptsChanged(before, after *apps.Workflow) bool {
	was := map[string]string{}
	if before != nil {
		for _, st := range before.Steps {
			was[st.ID] = st.Script
		}
	}
	for _, st := range after.Steps {
		if st.Script != "" && strings.TrimRight(st.Script, "\n") != strings.TrimRight(was[st.ID], "\n") {
			return true
		}
	}
	return false
}
