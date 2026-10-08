package api

import (
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
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
	// Freezes are the ids of the change freezes this window overlaps that apply to it: those for every change, and
	// those limited to an item it is linked to (see freezes in the response).
	Freezes []uuid.UUID `json:"freezes"`
}

// GET /calendar?from=&to=: the records whose planned window overlaps from..to (RFC 3339 times, at most 92 days
// apart), as the caller may see them, ordered by start, with the overlaps between them, and the change freezes in the
// range. Only changes have a planned window so far. truncated says more than 500 records matched and only the first
// 500 are listed.
func (h *handlers) listCalendar(w http.ResponseWriter, r *http.Request) {
	from, to, ok := calendarRange(w, r)
	if !ok {
		return
	}
	var rows []dbgen.ListPlannedWindowsRow
	var freezes []freezeDTO
	// The items each record is linked to, when the reader may see links. Without them every freeze in a record's window
	// is shown as applying to it, which errs on the side of a freeze.
	var linked map[uuid.UUID][]uuid.UUID
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		var err error
		rows, err = q.ListPlannedWindows(r.Context(), dbgen.ListPlannedWindowsParams{From: pgtype.Timestamptz{Time: from, Valid: true}, To: pgtype.Timestamptz{Time: to, Valid: true}})
		if err != nil {
			return err
		}
		if freezes, _, err = listFreezes(r, q, from, to); err != nil {
			return err
		}
		if err := h.withItems(r.Context(), q, freezes); err != nil {
			return err
		}
		if p.Can(authz.CMDBRead) {
			ids := make([]uuid.UUID, len(rows))
			for i, x := range rows {
				ids[i] = x.ID
			}
			links, err := q.ListTasksCIIDs(r.Context(), ids)
			linked = map[uuid.UUID][]uuid.UUID{}
			for _, l := range links {
				linked[l.TaskID] = append(linked[l.TaskID], l.CiID)
			}
			return err
		}
		return nil
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
			Risk: x.Risk, PlannedStart: x.PlannedStart.Time, PlannedEnd: x.PlannedEnd.Time, Overlaps: []string{}, Freezes: []uuid.UUID{}}
		if x.AssigneeName.Valid {
			items[i].AssigneeName = &x.AssigneeName.String
		}
		for _, f := range freezes {
			applies := len(f.ItemIDs) == 0 || linked == nil || slices.ContainsFunc(linked[x.ID], func(ci uuid.UUID) bool { return slices.Contains(f.ItemIDs, ci) })
			if applies && x.PlannedStart.Time.Before(f.EndsAt) && f.StartsAt.Before(x.PlannedEnd.Time) {
				items[i].Freezes = append(items[i].Freezes, f.ID)
			}
		}
	}
	markOverlaps(items)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "freezes": freezes, "from": from, "to": to, "truncated": truncated})
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
