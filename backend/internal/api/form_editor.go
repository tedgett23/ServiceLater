package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// The form editor (Admin > Forms) changes a record type's form the way the catalog editor changes items: an edit is
// kept in an app's studio layer as only what it changes, and the app is reinstalled with it at once
// (docs/apps-platform.md §8.1). A type with no form of its own gets one in the app that owns the type. Removing a
// field takes it off the form; the values records hold are kept.

// formEntryDTO is a record type in the editor's list.
type formEntryDTO struct {
	RecordType string `json:"record_type"`
	Label      string `json:"label"`
	Prefix     string `json:"prefix"`
	// App is the app whose form it is, or that an edit goes in (the type's owner when it has no form).
	App string `json:"app"`
	// Builtin says no app gives the type a form: it has the one made of its own fields.
	Builtin bool `json:"builtin"`
	// Catalog says its forms are its catalog items' questions, edited in the catalog editor.
	Catalog bool `json:"catalog"`
	Edited  bool `json:"edited"`
	// ExtraFields counts the fields the form adds.
	ExtraFields int `json:"extra_fields"`
}

// coreFieldDTO is one of a type's own fields, which a form may relabel, explain, lay out and set conditions on.
type coreFieldDTO struct {
	Name    string        `json:"name"`
	Label   string        `json:"label"`
	Type    string        `json:"type"`
	Options []apps.Option `json:"options,omitempty"`
	// OnCreate says it may be asked when filing; Detail that it may be laid out on the record.
	OnCreate bool `json:"on_create"`
	Detail   bool `json:"detail"`
}

// formDefDTO is a form as the editor changes it: the definition its file holds, and what the editor needs around it.
type formDefDTO struct {
	RecordType string     `json:"record_type"`
	Label      string     `json:"label"`
	Form       *apps.Form `json:"form"`
	// App is the app the form is (or an edit would be) kept in.
	App      string     `json:"app"`
	Builtin  bool       `json:"builtin"`
	Shipped  bool       `json:"shipped"`
	Edited   bool       `json:"edited"`
	EditedAt *time.Time `json:"edited_at,omitempty"`
	EditedBy *string    `json:"edited_by,omitempty"`
	Layers   []string   `json:"layers"`
	// CoreFields are the type's own fields.
	CoreFields []coreFieldDTO `json:"core_fields"`
	// Values counts the records holding a value for each extra field, by name, including fields the form no longer
	// has: a field with values keeps its name and type.
	Values map[string]int64 `json:"values"`
	// YAML is the form as a file; Override the edit as it is kept (what `servicelater app export-local` writes).
	YAML     string `json:"yaml"`
	Override string `json:"override,omitempty"`
	Path     string `json:"path"`
}

// formOwner is the app a type's form is kept in: the form's own app, else the app that defines the type, else the
// one that gives it its lifecycle.
func (h *handlers) formOwner(t string) (app string, builtin bool) {
	f := h.apps.Form(t)
	if !f.Builtin && f.App != "" {
		return f.App, false
	}
	if rt := h.apps.RecordType(t); rt != nil {
		return rt.App, true
	}
	if lc := h.apps.Lifecycle(t); lc != nil && lc.RecordType == t && lc.App != "" {
		return lc.App, true
	}
	return "", true
}

// editableType is the {type} path segment of a type whose form can be edited here.
func (h *handlers) editableType(r *http.Request) (string, error) {
	t := chi.URLParam(r, "type")
	switch {
	case t == "task" || !h.apps.KnownType(t):
		return "", notFound()
	case t == "request":
		return "", &apiError{http.StatusConflict, "a request's form is its catalog item's questions: change them in the catalog editor"}
	}
	return t, nil
}

// GET /forms (forms:manage): every record type in use, with where its form comes from and whether it was changed here.
func (h *handlers) listFormDefs(w http.ResponseWriter, r *http.Request) {
	var types []dbgen.TaskType
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		types, err = q.ListTaskTypes(r.Context())
		return err
	}) {
		return
	}
	edited := map[string]bool{}
	if err := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		defs, err := dbgen.New(tx).ListCurrentDefinitions(r.Context())
		for _, d := range defs {
			if d.Kind == "form" && slices.Contains(strings.Split(d.Source, "+"), "studio") {
				edited[d.Name] = true
			}
		}
		return err
	}); err != nil {
		writeDBError(w, err)
		return
	}
	out := []formEntryDTO{}
	for _, tt := range types {
		if tt.Type == "task" || !h.apps.Provides(tt.Type) {
			continue
		}
		f := h.apps.Form(tt.Type)
		app, builtin := h.formOwner(tt.Type)
		e := formEntryDTO{RecordType: tt.Type, Label: tt.Label, Prefix: tt.Prefix, App: app, Builtin: builtin,
			Catalog: tt.FromCatalog, Edited: edited[tt.Type]}
		for _, fd := range f.Fields {
			if !fd.Core() {
				e.ExtraFields++
			}
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// formStudio loads the app a type's form is kept in, with the form's definition there if it has one.
func (h *handlers) formStudio(ctx context.Context, t string) (*studioApp, string, *apps.Definition, error) {
	app, _ := h.formOwner(t)
	if app == "" {
		return nil, "", nil, &apiError{http.StatusConflict, "no installed app owns " + t + " records, so there is nowhere to keep a form for them"}
	}
	s, err := h.loadStudioApp(ctx, app)
	if err != nil {
		return nil, app, nil, err
	}
	if d, ok := s.definitionOf("form", t); ok {
		return s, app, &d, nil
	}
	return s, app, nil, nil
}

func (h *handlers) toFormDef(ctx context.Context, t, app string, c *apps.Compiled, d *apps.Definition) (formDefDTO, error) {
	out := formDefDTO{RecordType: t, App: app, Path: "forms/" + t + ".yaml", Layers: []string{}, Values: map[string]int64{},
		CoreFields: []coreFieldDTO{}}
	if rt := h.apps.RecordType(t); rt != nil {
		out.Label = rt.Label
	}
	if d != nil {
		out.Form, out.Layers, out.Path = d.Form, d.Layers, d.Path
		out.Edited = slices.Contains(d.Layers, "studio")
		out.Shipped = slices.ContainsFunc(d.Layers, func(l string) bool { return l != "studio" })
	} else {
		out.Form, out.Builtin = apps.FormFor(t), true
	}
	// A copy, with empty lists rather than none, so the editor never meets a null.
	f := *out.Form
	for _, l := range []*[]string{&f.Create} {
		if *l == nil {
			*l = []string{}
		}
	}
	for _, l := range []*[]apps.Section{&f.CreateSections, &f.Detail} {
		if *l == nil {
			*l = []apps.Section{}
		}
	}
	if f.Fields == nil {
		f.Fields = []apps.FormField{}
	}
	if f.Validate == nil {
		f.Validate = []apps.Guard{}
	}
	out.Form = &f
	for _, c := range apps.CoreFields {
		if c.RecordType != "" && c.RecordType != t || !c.OnCreate && !c.Detail {
			continue
		}
		out.CoreFields = append(out.CoreFields, coreFieldDTO{Name: c.Name, Label: c.Label, Type: c.Type, Options: c.Options,
			OnCreate: c.OnCreate && (c.RecordType == "" || c.RecordType == t), Detail: c.Detail})
	}
	data, err := apps.FormYAML(out.Form, false)
	if err != nil {
		return out, err
	}
	out.YAML = string(data)
	err = db.WithSystem(ctx, h.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if out.Label == "" {
			if tt, err := q.GetTaskType(ctx, t); err == nil {
				out.Label = tt.Label
			}
		}
		if !out.Edited {
			return nil
		}
		o, err := q.GetAppOverride(ctx, dbgen.GetAppOverrideParams{AppName: app, Path: out.Path})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out.EditedAt, out.EditedBy, out.Override = &o.UpdatedAt.Time, textPtr(o.UpdatedByName), o.Content
		return err
	})
	return out, err
}

// fieldValues counts the records of a type holding a value for each extra field: all of them, whoever may read them,
// because what it decides (a field's type is fixed) protects every record. Only counts leave it.
func (h *handlers) fieldValues(r *http.Request, t string) (map[string]int64, error) {
	out := map[string]int64{}
	err := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		rows, err := dbgen.New(tx).CountFieldValues(r.Context(), t)
		for _, x := range rows {
			out[x.Name] = x.Records
		}
		return err
	})
	return out, err
}

// GET /forms/{type}/definition (forms:manage): a type's form for the editor: its definition (the built-in one when no
// app gives it a form), where it is kept, whether it was changed here, the type's own fields, and how many records
// hold a value for each extra field.
func (h *handlers) getFormDef(w http.ResponseWriter, r *http.Request) {
	t, err := h.editableType(r)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	s, app, d, err := h.formStudio(r.Context(), t)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	out, err := h.toFormDef(r.Context(), t, app, s.compiled, d)
	if err == nil {
		out.Values, err = h.fieldValues(r, t)
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// formSaveRequest is a form as the editor saves it.
type formSaveRequest struct {
	Form *apps.Form `json:"form"`
}

// formContent is the studio file for the edited form: only what changed over a form the app ships, or the whole form
// for one added here. It also refuses a change of type for a field records hold values of.
func (h *handlers) formContent(r *http.Request, t string, s *studioApp, d *apps.Definition, f *apps.Form) (string, []byte, error) {
	f.RecordType = t
	f.Normalize()
	values, err := h.fieldValues(r, t)
	if err != nil {
		return "", nil, err
	}
	current := h.apps.Form(t)
	for _, fd := range f.Fields {
		old, ok := current.Field(fd.Name)
		if !ok || old.Core() || fd.Type == old.Type || values[fd.Name] == 0 {
			continue
		}
		if text := []string{apps.FieldText, apps.FieldTextarea}; slices.Contains(text, fd.Type) && slices.Contains(text, old.Type) {
			continue
		}
		return "", nil, invalid(fmt.Sprintf("%s holds values in %d records, so it stays a %s field", fd.Name, values[fd.Name], old.Type))
	}
	path := "forms/" + t + ".yaml"
	if d != nil {
		path = d.Path
	}
	base, below, err := s.bundle.Below(path)
	switch {
	case err != nil:
		return "", nil, err
	case below:
		content, err := apps.FormOverride(base, f)
		return path, content, err
	}
	content, err := apps.FormYAML(f, false)
	return path, content, err
}

// POST /forms/{type}/definition/check (forms:manage) with {"form"}: what saving it would refuse, without saving:
// {"problems": [...]}, empty when it would save. The editor asks as it is changed.
func (h *handlers) checkFormDef(w http.ResponseWriter, r *http.Request) {
	var req formSaveRequest
	if !decode(w, r, &req) {
		return
	}
	t, err := h.editableType(r)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	if req.Form == nil {
		writeError(w, http.StatusUnprocessableEntity, "form is required")
		return
	}
	s, _, d, err := h.formStudio(r.Context(), t)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	out := []string{}
	path, content, err := h.formContent(r, t, s, d, req.Form)
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		out = append(out, ae.msg)
	case err != nil:
		writeDBError(w, err)
		return
	default:
		b := *s.bundle
		b.Studio = maps.Clone(s.bundle.Studio)
		if b.Studio == nil {
			b.Studio = map[string][]byte{}
		}
		if content == nil {
			delete(b.Studio, path)
		} else {
			b.Studio[path] = content
		}
		if _, err := b.Compile(); err != nil {
			var p apps.Problems
			if !errors.As(err, &p) {
				out = append(out, err.Error())
			}
			for _, x := range p {
				out = append(out, problemText(x))
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"problems": out})
}

// PUT /forms/{type}/definition (forms:manage) with {"form"}: save a type's form into its app's studio layer (adding a
// form for a type that had none) and reinstall the app with it at once. It is checked as an install checks it; every
// problem is listed (422 with problems), such as a rule that reads a field taken off the form. Fields taken off keep
// their values in the records that have them; a field with values keeps its type.
func (h *handlers) putFormDef(w http.ResponseWriter, r *http.Request) {
	var req formSaveRequest
	if !decode(w, r, &req) {
		return
	}
	t, err := h.editableType(r)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	if req.Form == nil {
		writeError(w, http.StatusUnprocessableEntity, "form is required")
		return
	}
	s, app, d, err := h.formStudio(r.Context(), t)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	path, content, err := h.formContent(r, t, s, d, req.Form)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	c, err := h.installStudio(r, app, path, content)
	if err != nil {
		var pe *problemsError
		if errors.As(err, &pe) {
			pe.msg = "the form cannot be saved"
		}
		writeStudioError(w, err)
		return
	}
	saved := studioApp{bundle: s.bundle, compiled: c}
	var sd *apps.Definition
	if x, ok := saved.definitionOf("form", t); ok {
		sd = &x
	}
	out, err := h.toFormDef(r.Context(), t, app, c, sd)
	if err == nil {
		out.Values, err = h.fieldValues(r, t)
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /forms/{type}/edits (forms:manage): undo the changes made here: the form goes back to the one its app ships,
// or, for a type that had none, to the built-in form. Records keep the values they hold.
func (h *handlers) resetFormDef(w http.ResponseWriter, r *http.Request) {
	t, err := h.editableType(r)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	s, app, d, err := h.formStudio(r.Context(), t)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	if d == nil || !slices.Contains(d.Layers, "studio") {
		writeError(w, http.StatusNotFound, fmt.Sprintf("the %s form has no changes made here", t))
		return
	}
	c, err := h.installStudio(r, s.compiled.Manifest.Name, d.Path, nil)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	reset := studioApp{bundle: s.bundle, compiled: c}
	var rd *apps.Definition
	if x, ok := reset.definitionOf("form", t); ok {
		rd = &x
	}
	out, err := h.toFormDef(r.Context(), t, app, c, rd)
	if err == nil {
		out.Values, err = h.fieldValues(r, t)
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
