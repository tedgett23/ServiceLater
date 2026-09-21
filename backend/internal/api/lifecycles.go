package api

import (
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

type lifecycleDTO struct {
	RecordType string       `json:"record_type"`
	Initial    string       `json:"initial"`
	States     []apps.State `json:"states"`
	App        string       `json:"app"`
	Version    string       `json:"version"`
}

// GET /lifecycles/{recordType}: the states, so the UI can label statuses and pick their shape. What may be done
// next is not here: each ticket carries its own moves, because guards depend on the ticket and the person.
func (h *handlers) getLifecycle(w http.ResponseWriter, r *http.Request) {
	rt := chi.URLParam(r, "recordType")
	if !slices.Contains(apps.KnownRecordTypes, rt) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	lc := h.apps.Lifecycle(rt)
	writeJSON(w, http.StatusOK, lifecycleDTO{RecordType: rt, Initial: lc.Initial, States: lc.States, App: lc.App, Version: lc.Version})
}
