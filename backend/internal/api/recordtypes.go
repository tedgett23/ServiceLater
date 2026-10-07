package api

import (
	"net/http"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// recordTypeDTO is a record type in use: an installed app defines its lifecycle.
type recordTypeDTO struct {
	Type   string `json:"type"`
	Prefix string `json:"prefix"`
	Label  string `json:"label"`
	// CanFile says whether the caller may file one as themselves: the type is open to everyone, or they have
	// records:create:<type>.
	CanFile bool `json:"can_file"`
	// CanFileForOthers says whether they may also name someone else as the requester (records:update:<type> too).
	CanFileForOthers bool `json:"can_file_for_others"`
}

// GET /record-types: the record types in use, and which of them the caller may file, for the New ticket dialog.
func (h *handlers) listRecordTypes(w http.ResponseWriter, r *http.Request) {
	out := []recordTypeDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		rows, err := q.ListTaskTypes(r.Context())
		for _, t := range rows {
			if !h.apps.Provides(t.Type) {
				continue
			}
			file := t.AnyoneMayFile || p.Perms.CanRecord(authz.Create, t.Type)
			out = append(out, recordTypeDTO{Type: t.Type, Prefix: t.Prefix, Label: t.Label, CanFile: file,
				CanFileForOthers: file && p.Perms.CanRecord(authz.Update, t.Type)})
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
