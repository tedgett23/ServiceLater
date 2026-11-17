package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// historySettingsDTO is what is recorded field by field. The basics are always recorded whatever it says: a
// record's creation, status, assignee and assignment group, and an item's creation, status and deletion.
type historySettingsDTO struct {
	settings.History
	// RecordTypeOptions are the record types in use, to choose from.
	RecordTypeOptions []recordTypeOptionDTO `json:"record_type_options"`
	// Exclusions are the fields apps leave out of history (history: false in a form or a class).
	Exclusions []historyExclusionDTO `json:"exclusions"`
	// Log is who changed these settings and when, newest first, so a gap in history can be told from a quiet record.
	Log []settingChangeDTO `json:"log"`
}

type recordTypeOptionDTO struct {
	Type  string `json:"type"`
	Label string `json:"label"`
}

// historyExclusionDTO is a field an app leaves out of history.
type historyExclusionDTO struct {
	App string `json:"app"`
	// Subject is task (a record type, Kind) or ci (a class, Kind).
	Subject string `json:"subject"`
	Kind    string `json:"kind"`
	Field   string `json:"field"`
	Label   string `json:"label"`
}

// settingChangeDTO is one change to a setting.
type settingChangeDTO struct {
	At        time.Time `json:"at"`
	ActorName *string   `json:"actor_name"`
	Setting   string    `json:"setting"`
	Old       any       `json:"old"`
	New       any       `json:"new"`
}

func (h *handlers) historySettings(ctx context.Context, q *dbgen.Queries) (historySettingsDTO, error) {
	rows, err := q.ListSettings(ctx, settings.HistoryPrefix)
	if err != nil {
		return historySettingsDTO{}, err
	}
	stored := map[string][]byte{}
	for _, r := range rows {
		stored[r.Name] = r.Value
	}
	out := historySettingsDTO{History: settings.HistoryFrom(stored), RecordTypeOptions: []recordTypeOptionDTO{},
		Exclusions: []historyExclusionDTO{}, Log: []settingChangeDTO{}}
	types, err := q.ListTaskTypes(ctx)
	if err != nil {
		return out, err
	}
	for _, t := range types {
		if h.apps.Provides(t.Type) {
			out.RecordTypeOptions = append(out.RecordTypeOptions, recordTypeOptionDTO{Type: t.Type, Label: t.Label})
		}
	}
	ex, err := q.ListHistoryExclusions(ctx)
	if err != nil {
		return out, err
	}
	for _, e := range ex {
		name := strings.TrimPrefix(e.Field, "fields.")
		label := humanField(name)
		if e.Subject == "task" {
			if fd, ok := h.apps.Form(e.Kind).Field(name); ok {
				label = fd.Label
			}
		} else if c := h.apps.Class(e.Kind); c != nil {
			if fd, ok := c.Field(name); ok {
				label = fd.Label
			}
		}
		out.Exclusions = append(out.Exclusions, historyExclusionDTO{App: e.AppName, Subject: e.Subject, Kind: e.Kind, Field: e.Field, Label: label})
	}
	log, err := q.ListSettingHistory(ctx, settings.HistoryPrefix)
	for _, l := range log {
		out.Log = append(out.Log, settingChangeDTO{At: l.At.Time, ActorName: textPtr(l.ActorName), Setting: l.Field,
			Old: decodeValue(l.Old), New: decodeValue(l.New)})
	}
	return out, err
}

// GET /settings/history (settings:manage): which changes are recorded field by field, the fields apps leave out, and
// who changed these settings.
func (h *handlers) getHistorySettings(w http.ResponseWriter, r *http.Request) {
	var out historySettingsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		out, err = h.historySettings(r.Context(), q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT /settings/history (settings:manage) with {"record_types", "cmdb"}: choose what is recorded field by field:
// record_types lists record types (or ["*"] for every type, including ones added later), cmdb is configuration
// items. The basics are always recorded, and the change itself is recorded.
func (h *handlers) putHistorySettings(w http.ResponseWriter, r *http.Request) {
	var req settings.History
	if !decode(w, r, &req) {
		return
	}
	var out historySettingsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if err := req.Check(h.apps.RecordTypes()); err != nil {
			return invalid(err.Error())
		}
		for name, value := range req.Rows() {
			if err := q.UpsertSetting(r.Context(), dbgen.UpsertSettingParams{Name: name, Value: value}); err != nil {
				return err
			}
		}
		var err error
		out, err = h.historySettings(r.Context(), q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
