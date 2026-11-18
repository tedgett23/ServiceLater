package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// Admin → SLAs (docs/apps-platform.md §6.6): which hours each SLA counts. An SLA chooses its calendar by the record
// (its priority, its team, any condition), and the choice is kept as an override of calendar: in the app's studio
// layer, like the other editors' changes. New clocks use it, and running ones from their next save.

// slaDefDTO is an SLA and the hours it counts.
type slaDefDTO struct {
	App   string `json:"app"`
	Name  string `json:"name"`
	Label string `json:"label"`
	// RecordType is the type it runs on; RecordTypeLabel what people call those records (Incidents).
	RecordType      string `json:"record_type"`
	RecordTypeLabel string `json:"record_type_label"`
	// Target is the working time allowed when it is fixed (8h), else empty and TargetRule is the expression that works
	// it out from the record.
	Target     string `json:"target"`
	TargetRule string `json:"target_rule"`
	// Calendar is how it chooses its hours: the first rule whose when holds (always, when empty) decides; use is a
	// calendar's name, 24x7 (every hour) or group (the assignment group's own hours, skipped when it has none).
	Calendar []apps.CalendarRule `json:"calendar"`
	// Edited says the choice was changed here, apart from the app.
	Edited bool `json:"edited"`
}

func toSLADef(app string, d apps.Definition, labels map[string]string) slaDefDTO {
	s := d.SLA
	out := slaDefDTO{App: app, Name: s.Name, Label: s.Label, RecordType: s.RecordType, RecordTypeLabel: labels[s.RecordType],
		Calendar: s.CalendarRulesOf(), Edited: slices.Contains(d.Layers, "studio")}
	if out.RecordTypeLabel == "" {
		out.RecordTypeLabel = s.RecordType
	}
	if s.Target != nil {
		if s.Target.Expr != "" {
			out.TargetRule = s.Target.Expr
		} else {
			out.Target = fmt.Sprint(s.Target.Literal)
		}
	}
	return out
}

// typeLabels are the record types' plural names, as the caller may read them.
func typeLabels(r *http.Request, q *dbgen.Queries) (map[string]string, error) {
	types, err := q.ListTaskTypes(r.Context())
	out := map[string]string{}
	for _, t := range types {
		out[t.Type] = t.Plural
	}
	return out, err
}

// GET /slas (slas:manage): every installed app's SLAs and the hours each counts, by record type then label.
func (h *handlers) listSLADefs(w http.ResponseWriter, r *http.Request) {
	var labels map[string]string
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		labels, err = typeLabels(r, q)
		return err
	}) {
		return
	}
	out := []slaDefDTO{}
	for _, a := range h.apps.Apps() {
		s, err := h.loadStudioApp(r.Context(), a.Name)
		if err != nil || s.compiled == nil {
			continue
		}
		for _, d := range s.compiled.Definitions {
			if d.SLA != nil {
				out = append(out, toSLADef(a.Name, d, labels))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RecordType != out[j].RecordType {
			return out[i].RecordType < out[j].RecordType
		}
		return out[i].Label < out[j].Label
	})
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// slaDefIn finds an app's SLA, as installed with its edits.
func (h *handlers) slaDefIn(r *http.Request) (*studioApp, apps.Definition, error) {
	s, err := h.loadStudioApp(r.Context(), chi.URLParam(r, "app"))
	if err != nil {
		return nil, apps.Definition{}, err
	}
	d, ok := s.definitionOf("sla", chi.URLParam(r, "name"))
	if !ok {
		return nil, apps.Definition{}, notFound()
	}
	return s, d, nil
}

// savedSLADef is an SLA as compiled c has it, for the answer to a change.
func savedSLADef(r *http.Request, q *dbgen.Queries, app, name string, c *apps.Compiled) (slaDefDTO, error) {
	labels, err := typeLabels(r, q)
	if err != nil {
		return slaDefDTO{}, err
	}
	saved := studioApp{compiled: c}
	d, ok := saved.definitionOf("sla", name)
	if !ok {
		return slaDefDTO{}, notFound()
	}
	return toSLADef(app, d, labels), nil
}

// slaCalendarRequest is how an SLA chooses its hours.
type slaCalendarRequest struct {
	// Calendar is the rules, in order: when (a condition on the record; empty always holds) and use (a calendar's name,
	// 24x7 or group). The last should hold always, so every record gets hours.
	Calendar []apps.CalendarRule `json:"calendar"`
}

// PUT /slas/{app}/{name}/calendar (slas:manage) with {"calendar": [{"when", "use"}...]}: choose which hours an SLA counts.
// It is saved into the app's studio layer and the app reinstalled at once, checked as an install checks it (422 with
// problems, such as a condition that does not compile). Clocks that start from now use it, and running clocks from
// their next save, keeping the time they have used.
func (h *handlers) putSLACalendar(w http.ResponseWriter, r *http.Request) {
	var req slaCalendarRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.Calendar) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "calendar needs at least one rule")
		return
	}
	s, d, err := h.slaDefIn(r)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	app := s.compiled.Manifest.Name
	below, _, err := s.bundle.Below(d.Path)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	content, err := apps.SLACalendarOverride(below, s.bundle.Studio[d.Path], req.Calendar)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	c := s.compiled
	if content != nil || slices.Contains(d.Layers, "studio") {
		if c, err = h.installStudio(r, app, d.Path, content); err != nil {
			var pe *problemsError
			if errors.As(err, &pe) {
				pe.msg = "the SLA's hours cannot be saved"
			}
			writeStudioError(w, err)
			return
		}
	}
	var out slaDefDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		out, err = savedSLADef(r, q, app, d.Name, c)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /slas/{app}/{name}/edits (slas:manage): undo the changes made here: the SLA counts the hours its app ships.
func (h *handlers) resetSLADef(w http.ResponseWriter, r *http.Request) {
	s, d, err := h.slaDefIn(r)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	if !slices.Contains(d.Layers, "studio") {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s has no changes made here", d.Name))
		return
	}
	c, err := h.installStudio(r, s.compiled.Manifest.Name, d.Path, nil)
	if err != nil {
		var pe *problemsError
		if errors.As(err, &pe) {
			pe.msg = "the change cannot be undone"
		}
		writeStudioError(w, err)
		return
	}
	var out slaDefDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		out, err = savedSLADef(r, q, s.compiled.Manifest.Name, d.Name, c)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
