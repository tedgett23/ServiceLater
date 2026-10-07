package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

const (
	maxFreezeName   = 200
	maxFreezeReason = 2000
	maxFreezeItems  = 200
)

// freezeDTO is a change freeze: a period when changes may not be scheduled. What a freeze stops is up to the apps
// installed (change-management refuses a planned window that overlaps one, except for an emergency change, and holds
// back work that would start during one).
type freezeDTO struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Reason   string    `json:"reason"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	// CreatedByName is who declared it; null once that person is deleted.
	CreatedByName *string   `json:"created_by_name"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func toFreeze(f dbgen.GetFreezeWindowRow) freezeDTO {
	out := freezeDTO{ID: f.ID, Name: f.Name, Reason: f.Reason, StartsAt: f.StartsAt.Time, EndsAt: f.EndsAt.Time,
		CreatedAt: f.CreatedAt.Time, UpdatedAt: f.UpdatedAt.Time}
	if f.CreatedByName.Valid {
		out.CreatedByName = &f.CreatedByName.String
	}
	return out
}

// freezeRequest is a freeze as it is declared or changed. On PATCH, a field left out keeps its value.
type freezeRequest struct {
	Name     *string    `json:"name"`
	Reason   *string    `json:"reason"`
	StartsAt *time.Time `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at"`
}

// apply checks the request against the freeze as it is (zero on create) and returns the result.
func (req freezeRequest) apply(cur dbgen.UpdateFreezeWindowParams) (dbgen.UpdateFreezeWindowParams, error) {
	if req.Name != nil {
		cur.Name = strings.TrimSpace(*req.Name)
	}
	if req.Reason != nil {
		cur.Reason = strings.TrimSpace(*req.Reason)
	}
	if req.StartsAt != nil {
		cur.StartsAt = pgtype.Timestamptz{Time: *req.StartsAt, Valid: true}
	}
	if req.EndsAt != nil {
		cur.EndsAt = pgtype.Timestamptz{Time: *req.EndsAt, Valid: true}
	}
	switch n := utf8.RuneCountInString(cur.Name); {
	case n < 1 || n > maxFreezeName:
		return cur, invalid(fmt.Sprintf("name must be 1 to %d characters", maxFreezeName))
	case utf8.RuneCountInString(cur.Reason) > maxFreezeReason:
		return cur, invalid(fmt.Sprintf("reason must be at most %d characters", maxFreezeReason))
	case !cur.StartsAt.Valid || !cur.EndsAt.Valid:
		return cur, invalid("starts_at and ends_at are required")
	case !cur.EndsAt.Time.After(cur.StartsAt.Time):
		return cur, invalid("a freeze must end after it starts")
	}
	return cur, nil
}

// calendarRange reads ?from=&to= (RFC 3339, at most 92 days apart), answering 400 itself when they are not usable.
func calendarRange(w http.ResponseWriter, r *http.Request) (from, to time.Time, ok bool) {
	qs := r.URL.Query()
	from, err1 := time.Parse(time.RFC3339, qs.Get("from"))
	to, err2 := time.Parse(time.RFC3339, qs.Get("to"))
	switch {
	case err1 != nil || err2 != nil:
		writeError(w, http.StatusBadRequest, "from and to are required, as RFC 3339 times such as 2026-11-01T00:00:00Z")
		return from, to, false
	case !to.After(from):
		writeError(w, http.StatusBadRequest, "to must be after from")
		return from, to, false
	case to.Sub(from) > maxCalendarRange:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the range may be at most %d days", int(maxCalendarRange.Hours()/24)))
		return from, to, false
	}
	return from, to, true
}

func listFreezes(r *http.Request, q *dbgen.Queries, from, to time.Time) ([]freezeDTO, bool, error) {
	rows, err := q.ListFreezeWindows(r.Context(), dbgen.ListFreezeWindowsParams{From: pgtype.Timestamptz{Time: from, Valid: true}, To: pgtype.Timestamptz{Time: to, Valid: true}})
	truncated := len(rows) > maxFreezeItems
	if truncated {
		rows = rows[:maxFreezeItems]
	}
	out := make([]freezeDTO, len(rows))
	for i, f := range rows {
		out[i] = toFreeze(dbgen.GetFreezeWindowRow(f))
	}
	return out, truncated, err
}

// GET /freeze-windows?from=&to=: the change freezes that overlap from..to (RFC 3339 times, at most 92 days apart),
// by start. Anyone signed in may read them. truncated says more than 200 matched.
func (h *handlers) listFreezeWindows(w http.ResponseWriter, r *http.Request) {
	from, to, ok := calendarRange(w, r)
	if !ok {
		return
	}
	var out []freezeDTO
	var truncated bool
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		out, truncated, err = listFreezes(r, q, from, to)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "truncated": truncated})
}

// POST /freeze-windows (freezes:manage) with {"name", "reason"?, "starts_at", "ends_at"}: declare a change freeze.
func (h *handlers) createFreezeWindow(w http.ResponseWriter, r *http.Request) {
	var req freezeRequest
	if !decode(w, r, &req) {
		return
	}
	var out freezeDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		f, err := req.apply(dbgen.UpdateFreezeWindowParams{})
		if err != nil {
			return err
		}
		id, err := q.CreateFreezeWindow(r.Context(), dbgen.CreateFreezeWindowParams{Name: f.Name, Reason: f.Reason, StartsAt: f.StartsAt, EndsAt: f.EndsAt})
		if err != nil {
			return err
		}
		got, err := q.GetFreezeWindow(r.Context(), id)
		out = toFreeze(got)
		return err
	}) {
		return
	}
	w.Header().Set("Location", "/api/v1/freeze-windows/"+out.ID.String())
	writeJSON(w, http.StatusCreated, out)
}

// GET /freeze-windows/{id}: one change freeze.
func (h *handlers) getFreezeWindow(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var out freezeDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		got, err := q.GetFreezeWindow(r.Context(), id)
		out = toFreeze(got)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PATCH /freeze-windows/{id} (freezes:manage) with any of {"name", "reason", "starts_at", "ends_at"}. Changes already
// planned are not checked again: the calendar shows the ones a freeze now covers.
func (h *handlers) updateFreezeWindow(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req freezeRequest
	if !decode(w, r, &req) {
		return
	}
	var out freezeDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		cur, err := q.GetFreezeWindow(r.Context(), id)
		if err != nil {
			return err
		}
		f, err := req.apply(dbgen.UpdateFreezeWindowParams{ID: id, Name: cur.Name, Reason: cur.Reason, StartsAt: cur.StartsAt, EndsAt: cur.EndsAt})
		if err != nil {
			return err
		}
		if _, err := q.UpdateFreezeWindow(r.Context(), f); err != nil {
			return err // pgx.ErrNoRows: RLS hid the row from the update, so the caller may not change freezes
		}
		got, err := q.GetFreezeWindow(r.Context(), id)
		out = toFreeze(got)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /freeze-windows/{id} (freezes:manage): lift a change freeze.
func (h *handlers) deleteFreezeWindow(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		n, err := q.DeleteFreezeWindow(r.Context(), id)
		if err == nil && n == 0 {
			return notFound()
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
