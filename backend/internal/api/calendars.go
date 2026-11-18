package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/slaclock"
)

const (
	maxCalendarLabel    = 200
	maxCalendarHours    = 70
	maxCalendarHolidays = 1000
)

var calendarNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// calendarDTO is a business calendar: the working hours an SLA counts, in a time zone, less holidays. An SLA
// definition names one by name.
type calendarDTO struct {
	ID uuid.UUID `json:"id"`
	// Name is what an SLA definition writes after calendar: (lowercase letters, digits and hyphens).
	Name  string `json:"name"`
	Label string `json:"label"`
	// TimeZone is an IANA zone such as Europe/London; hours and holidays are in it.
	TimeZone string `json:"time_zone"`
	// Hours is the working week: day 0 is Sunday, start and end are HH:MM (end may be 24:00).
	Hours []slaclock.Hours `json:"hours"`
	// Holidays are dates (YYYY-MM-DD) with no working time.
	Holidays []string `json:"holidays"`
	// CreatedByName is who added it; null for the built-in one or once that person is deleted.
	CreatedByName *string   `json:"created_by_name"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func toCalendar(c dbgen.BusinessCalendar, createdBy pgtype.Text) calendarDTO {
	out := calendarDTO{ID: c.ID, Name: c.Name, Label: c.Label, TimeZone: c.TimeZone, Hours: []slaclock.Hours{}, Holidays: []string{},
		CreatedAt: c.CreatedAt.Time, UpdatedAt: c.UpdatedAt.Time}
	_ = json.Unmarshal(c.Hours, &out.Hours)
	for _, d := range c.Holidays {
		out.Holidays = append(out.Holidays, d.Time.Format(time.DateOnly))
	}
	if createdBy.Valid {
		out.CreatedByName = &createdBy.String
	}
	return out
}

// calendarRequest is a calendar as it is added or changed. On PATCH, a field left out keeps its value.
type calendarRequest struct {
	Name     *string           `json:"name"`
	Label    *string           `json:"label"`
	TimeZone *string           `json:"time_zone"`
	Hours    *[]slaclock.Hours `json:"hours"`
	Holidays *[]string         `json:"holidays"`
}

// apply checks the request against the calendar as it is (empty on create) and returns the result, ready to store.
func (req calendarRequest) apply(cur calendarDTO) (dbgen.UpdateBusinessCalendarParams, error) {
	if req.Name != nil {
		cur.Name = strings.TrimSpace(*req.Name)
	}
	if req.Label != nil {
		cur.Label = strings.TrimSpace(*req.Label)
	}
	if req.TimeZone != nil {
		cur.TimeZone = strings.TrimSpace(*req.TimeZone)
	}
	if req.Hours != nil {
		cur.Hours = *req.Hours
	}
	if req.Holidays != nil {
		cur.Holidays = *req.Holidays
	}
	out := dbgen.UpdateBusinessCalendarParams{ID: cur.ID, Name: cur.Name, Label: cur.Label, TimeZone: cur.TimeZone}
	switch n := utf8.RuneCountInString(cur.Label); {
	case !calendarNameRE.MatchString(cur.Name):
		return out, invalid("name must be 2 to 63 lowercase letters, digits and hyphens, starting with a letter")
	case n < 1 || n > maxCalendarLabel:
		return out, invalid(fmt.Sprintf("label must be 1 to %d characters", maxCalendarLabel))
	case len(cur.Hours) == 0:
		return out, invalid("hours must have at least one stretch of working time")
	case len(cur.Hours) > maxCalendarHours:
		return out, invalid(fmt.Sprintf("hours may have at most %d stretches", maxCalendarHours))
	case len(cur.Holidays) > maxCalendarHolidays:
		return out, invalid(fmt.Sprintf("a calendar may have at most %d holidays", maxCalendarHolidays))
	}
	cal, err := slaclock.Calendar(cur.TimeZone, cur.Hours, cur.Holidays)
	if err != nil {
		return out, invalid(err.Error())
	}
	out.Hours, _ = json.Marshal(cur.Hours)
	for _, h := range cal.Holidays { // in order, each once
		d, _ := time.Parse(time.DateOnly, h) // checked by slaclock.Calendar
		out.Holidays = append(out.Holidays, pgtype.Date{Time: d, Valid: true})
	}
	if out.Holidays == nil {
		out.Holidays = []pgtype.Date{}
	}
	return out, nil
}

// taken turns a second calendar of one name into a 409.
func taken(err error, name string) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return &apiError{http.StatusConflict, fmt.Sprintf("a calendar named %s already exists", name)}
	}
	return err
}

func getCalendar(r *http.Request, q *dbgen.Queries, id uuid.UUID) (calendarDTO, error) {
	got, err := q.GetBusinessCalendar(r.Context(), id)
	return toCalendar(got.BusinessCalendar, got.CreatedByName), err
}

// GET /business-calendars: every business calendar, by label. Anyone signed in may read them.
func (h *handlers) listBusinessCalendars(w http.ResponseWriter, r *http.Request) {
	var out []calendarDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListBusinessCalendars(r.Context())
		out = make([]calendarDTO, len(rows))
		for i, c := range rows {
			out[i] = toCalendar(c.BusinessCalendar, c.CreatedByName)
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// POST /business-calendars (calendars:manage) with {"name", "label", "time_zone", "hours", "holidays"?}.
func (h *handlers) createBusinessCalendar(w http.ResponseWriter, r *http.Request) {
	var req calendarRequest
	if !decode(w, r, &req) {
		return
	}
	var out calendarDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		c, err := req.apply(calendarDTO{})
		if err != nil {
			return err
		}
		id, err := q.CreateBusinessCalendar(r.Context(), dbgen.CreateBusinessCalendarParams{Name: c.Name, Label: c.Label,
			TimeZone: c.TimeZone, Hours: c.Hours, Holidays: c.Holidays})
		if err != nil {
			return taken(err, c.Name)
		}
		out, err = getCalendar(r, q, id)
		return err
	}) {
		return
	}
	w.Header().Set("Location", "/api/v1/business-calendars/"+out.ID.String())
	writeJSON(w, http.StatusCreated, out)
}

// GET /business-calendars/{id}: one business calendar.
func (h *handlers) getBusinessCalendar(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var out calendarDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		out, err = getCalendar(r, q, id)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PATCH /business-calendars/{id} (calendars:manage) with any of {"name", "label", "time_zone", "hours", "holidays"}.
// Running SLA clocks that count by it are rescheduled at once. Renaming it leaves the SLAs that name it without it
// (they then count every hour) until their definitions are changed too.
func (h *handlers) updateBusinessCalendar(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req calendarRequest
	if !decode(w, r, &req) {
		return
	}
	var out calendarDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		cur, err := getCalendar(r, q, id)
		if err != nil {
			return err
		}
		c, err := req.apply(cur)
		if err != nil {
			return err
		}
		if _, err := q.UpdateBusinessCalendar(r.Context(), c); err != nil {
			return taken(err, c.Name) // pgx.ErrNoRows: RLS hid the row from the update
		}
		out, err = getCalendar(r, q, id)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /business-calendars/{id} (calendars:manage): remove a calendar no running or paused SLA clock counts by
// (409 otherwise).
func (h *handlers) deleteBusinessCalendar(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		cur, err := getCalendar(r, q, id)
		if err != nil {
			return err
		}
		inUse, err := q.CountClocksByCalendar(r.Context(), cur.Name)
		if err != nil {
			return err
		}
		if inUse > 0 {
			return &apiError{http.StatusConflict, fmt.Sprintf("%d SLA clocks still count by this calendar", inUse)}
		}
		n, err := q.DeleteBusinessCalendar(r.Context(), id)
		var pe *pgconn.PgError
		switch {
		case errors.As(err, &pe) && pe.Code == "23503":
			return &apiError{http.StatusConflict, "a group works by this calendar; choose other hours for it first"}
		case err == nil && n == 0:
			return notFound()
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
