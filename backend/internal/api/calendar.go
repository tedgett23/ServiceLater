package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

const (
	maxCalendarRange = 92 * 24 * time.Hour
	maxCalendarItems = 500
)

// calendarEntryDTO is one record's planned window on the calendar.
type calendarEntryDTO struct {
	ID            uuid.UUID `json:"id"`
	Type          string    `json:"type"`
	Number        int64     `json:"number"`
	Reference     string    `json:"reference"`
	Title         string    `json:"title"`
	Status        string    `json:"status"`
	StateCategory string    `json:"state_category"`
	Priority      int16     `json:"priority"`
	AssigneeName  *string   `json:"assignee_name"`
	// ChangeType and Risk are the change's own fields (standard, normal or emergency; low, medium, high or "").
	ChangeType   string    `json:"change_type"`
	Risk         string    `json:"risk"`
	PlannedStart time.Time `json:"planned_start"`
	PlannedEnd   time.Time `json:"planned_end"`
	// Overlaps are the references of the other entries whose windows overlap this one, counting only records still
	// to be done (open or waiting): two changes in the same window are a conflict to look at.
	Overlaps []string `json:"overlaps"`
}

// GET /calendar?from=&to=: the records whose planned window overlaps from..to (RFC 3339 times, at most 92 days
// apart), as the caller may see them, ordered by start, with the overlaps between them. Only changes have a planned
// window so far. truncated says more than 500 matched and only the first 500 are listed.
func (h *handlers) listCalendar(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	from, err1 := time.Parse(time.RFC3339, qs.Get("from"))
	to, err2 := time.Parse(time.RFC3339, qs.Get("to"))
	switch {
	case err1 != nil || err2 != nil:
		writeError(w, http.StatusBadRequest, "from and to are required, as RFC 3339 times such as 2026-11-01T00:00:00Z")
		return
	case !to.After(from):
		writeError(w, http.StatusBadRequest, "to must be after from")
		return
	case to.Sub(from) > maxCalendarRange:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the range may be at most %d days", int(maxCalendarRange.Hours()/24)))
		return
	}
	var rows []dbgen.ListPlannedWindowsRow
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		rows, err = q.ListPlannedWindows(r.Context(), dbgen.ListPlannedWindowsParams{From: pgtype.Timestamptz{Time: from, Valid: true}, To: pgtype.Timestamptz{Time: to, Valid: true}})
		return err
	}) {
		return
	}
	truncated := len(rows) > maxCalendarItems
	if truncated {
		rows = rows[:maxCalendarItems]
	}
	items := make([]calendarEntryDTO, len(rows))
	for i, x := range rows {
		items[i] = calendarEntryDTO{ID: x.ID, Type: x.Type, Number: x.Number, Reference: fmt.Sprintf("%s%d", x.TypePrefix, x.Number),
			Title: x.Title, Status: x.Status, StateCategory: x.StateCategory, Priority: x.Priority, ChangeType: x.ChangeType,
			Risk: x.Risk, PlannedStart: x.PlannedStart.Time, PlannedEnd: x.PlannedEnd.Time, Overlaps: []string{}}
		if x.AssigneeName.Valid {
			items[i].AssigneeName = &x.AssigneeName.String
		}
	}
	markOverlaps(items)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "from": from, "to": to, "truncated": truncated})
}

// markOverlaps fills in each entry's overlaps. Entries are sorted by start, so each is compared only with the ones
// that start before it ends.
func markOverlaps(items []calendarEntryDTO) {
	active := func(e calendarEntryDTO) bool { return e.StateCategory == "open" || e.StateCategory == "waiting" }
	for i := range items {
		if !active(items[i]) {
			continue
		}
		for j := i + 1; j < len(items) && items[j].PlannedStart.Before(items[i].PlannedEnd); j++ {
			if active(items[j]) {
				items[i].Overlaps = append(items[i].Overlaps, items[j].Reference)
				items[j].Overlaps = append(items[j].Overlaps, items[i].Reference)
			}
		}
	}
}
