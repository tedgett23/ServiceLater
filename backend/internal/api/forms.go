package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
)

// formDTO is how a record type's fields are laid out: every field the form places, which go on the form for filing
// one and in which sections on the record. Whether each is shown, required or writable right now is a ticket's
// field_states, or for a record being filed, POST /forms/{type}/states.
type formDTO struct {
	RecordType string         `json:"record_type"`
	Fields     []formFieldDTO `json:"fields"`
	// Create lists the fields on the form for filing one, in order.
	Create []string `json:"create"`
	// CreateSections lays them out: each section's fields flow into as many columns as fit (up to three), or sit in
	// the columns it gives.
	CreateSections []apps.Section `json:"create_sections"`
	// Detail lists the sections of fields shown on the record, in order. Title, description and priority have places
	// of their own there and are not listed.
	Detail []apps.Section `json:"detail"`
	// Reactive are the fields the form's conditions read: when one of them changes while filing, ask for the states
	// again.
	Reactive []string `json:"reactive"`
	// App is the app that defines the form; empty for the form a type has without one.
	App string `json:"app,omitempty"`
}

// formFieldDTO is one field: one of the record's own (core) or an extra field the form declares.
type formFieldDTO struct {
	Name string `json:"name"`
	// Type is text, textarea, number, boolean, date, datetime or choice; the core priority field is priority.
	Type  string `json:"type"`
	Label string `json:"label"`
	Help  string `json:"help,omitempty"`
	// Core is set for the record's own fields, which requests name directly (title, resolution) or under change;
	// extra fields go under fields.
	Core    bool        `json:"core"`
	Options []optionDTO `json:"options,omitempty"`
	// Required is set when the field is always required; field_states says when it is now.
	Required bool `json:"required,omitempty"`
	// Default is what a new record starts with, for extra fields that have one.
	Default   any    `json:"default,omitempty"`
	MaxLength int    `json:"max_length,omitempty"`
	Min       *int64 `json:"min,omitempty"`
	Max       *int64 `json:"max,omitempty"`
}

// optionDTO is one value a choice offers.
type optionDTO struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func toFormDTO(f *apps.Form) formDTO {
	out := formDTO{RecordType: f.RecordType, Fields: []formFieldDTO{}, Create: f.Create, CreateSections: f.CreateSections,
		Detail: f.Detail, Reactive: f.Reactive(), App: f.App}
	if out.Create == nil {
		out.Create = []string{}
	}
	if out.CreateSections == nil {
		out.CreateSections = []apps.Section{}
	}
	if out.Detail == nil {
		out.Detail = []apps.Section{}
	}
	if out.Reactive == nil {
		out.Reactive = []string{}
	}
	for _, fd := range f.Placed() {
		out.Fields = append(out.Fields, toFieldDTO(fd))
	}
	return out
}

func toFieldDTO(fd apps.FormField) formFieldDTO {
	d := formFieldDTO{Name: fd.Name, Type: fd.Type, Label: fd.Label, Help: fd.Help, Core: fd.Core(), Required: fd.Required,
		MaxLength: fd.MaxLength, Min: fd.Min, Max: fd.Max}
	for _, o := range fd.Options {
		d.Options = append(d.Options, optionDTO{Value: o.Value, Label: o.Label})
	}
	if fd.Default != nil {
		d.Default = fd.Default.Literal
	}
	return d
}

// formType is the {type} path segment, if it names a record type with records of its own.
func (h *handlers) formType(r *http.Request) (string, bool) {
	t := chi.URLParam(r, "type")
	return t, t != "task" && h.apps.Exists(t)
}

// GET /forms/{type}?item=: the form for a record type, for anyone signed in (people file incidents). A type no app
// gives a form to has one made of its own fields. With item, a request's: the catalog item's questions, which is how a
// request ordered from it is laid out (the type's form when the item is gone).
func (h *handlers) getForm(w http.ResponseWriter, r *http.Request) {
	t, ok := h.formType(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, toFormDTO(h.apps.FormOf(t, r.URL.Query().Get("item"))))
}

// formStatesDTO is how the form for filing stands for a draft.
type formStatesDTO struct {
	// States are each field's state, by name.
	States map[string]apps.FieldState `json:"states"`
	// Values are what the before rules would set fields to, by name, where that differs from the draft.
	Values map[string]any `json:"values"`
}

// formStatesRequest is a record being filed, as far as it is filled in.
type formStatesRequest struct {
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Priority    *int16        `json:"priority"`
	Change      *changeFields `json:"change"`
	// Fields are the extra fields filled in so far, by name.
	Fields map[string]any `json:"fields"`
}

// POST /forms/{type}/states: whether each field of the form is shown, required and writable for a record being filed
// with these values, which options a choice offers, and in values what the type's before rules would set fields to
// (a priority worked out from impact and urgency). Nothing is saved. The form for filing asks again when a field
// listed in the form's reactive changes. Saving checks all of it again.
func (h *handlers) formStates(w http.ResponseWriter, r *http.Request) {
	t, ok := h.formType(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var req formStatesRequest
	if !decode(w, r, &req) {
		return
	}
	var out formStatesDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		var err error
		out.States, out.Values, err = h.svc.DraftStates(r.Context(), q, actorOf(p), records.CreateInput{Type: t, Title: req.Title,
			Description: req.Description, Priority: req.Priority, Change: req.Change.patch(), Fields: req.Fields})
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
