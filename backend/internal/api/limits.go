package api

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/ratelimit"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// limitsDTO is the rate limits and what they would be if nobody had changed them.
type limitsDTO struct {
	settings.Limits
	// Defaults are the limits as shipped, for the page to show beside each one.
	Defaults settings.Limits `json:"defaults"`
}

func (h *handlers) readLimits(r *http.Request, q *dbgen.Queries) (limitsDTO, error) {
	rows, err := q.ListSettings(r.Context(), settings.LimitsPrefix)
	if err != nil {
		return limitsDTO{}, err
	}
	stored := map[string][]byte{}
	for _, s := range rows {
		stored[s.Name] = s.Value
	}
	return limitsDTO{Limits: settings.LimitsFrom(stored), Defaults: settings.DefaultLimits}, nil
}

// GET /settings/limits (settings:manage): the rate limits (Admin → Rate limits).
func (h *handlers) getLimits(w http.ResponseWriter, r *http.Request) {
	var out limitsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		out, err = h.readLimits(r, q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT /settings/limits (settings:manage) with the limits to change (the rest stay): how many API requests a person and an API key may make a
// minute (0 for no limit; a key may have its own), sign-in attempts and their window, calls a webhook takes a minute,
// SQL queries a person may run an hour and at once, SMS codes an hour, and live connections a person may hold. The
// API limit reaches every server within a second; the others apply at once.
func (h *handlers) putLimits(w http.ResponseWriter, r *http.Request) {
	// What the request leaves out stays as it is (0 would turn a limit off).
	var req settings.Limits
	if err := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		var err error
		req, err = ratelimit.Load(r.Context(), dbgen.New(tx))
		return err
	}); err != nil {
		writeDBError(w, err)
		return
	}
	if !decode(w, r, &req) {
		return
	}
	var out limitsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if err := req.Check(); err != nil {
			return invalid(err.Error())
		}
		for name, value := range req.Rows() {
			if err := q.UpsertSetting(r.Context(), dbgen.UpsertSettingParams{Name: name, Value: value}); err != nil {
				return err
			}
		}
		var err error
		out, err = h.readLimits(r, q)
		return err
	}) {
		return
	}
	// This server need not wait for its next sync.
	if err := h.limits.Sync(r.Context()); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
