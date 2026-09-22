package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
)

const (
	defaultLimit = 25
	maxLimit     = 100
)

// ticketDTO is a ticket plus, on single-ticket responses (not lists), what the person may do next.
type ticketDTO struct {
	records.Ticket
	// Moves says what the person may do next, and why anything is blocked.
	Moves []moveDTO `json:"moves,omitempty"`
}

func toTicket(t dbgen.Task, prefix, requesterName string, assigneeName pgtype.Text, resolution string) ticketDTO {
	return ticketDTO{Ticket: records.FromParts(t, prefix, requesterName, assigneeName, resolution)}
}

// ticketView loads a ticket with its display names, by id, as one RLS-scoped query.
func ticketView(ctx context.Context, q *dbgen.Queries, id uuid.UUID) (ticketDTO, error) {
	t, err := records.Load(ctx, q, id)
	return ticketDTO{Ticket: t}, err
}

// loadTicketView is loadTicket plus display names.
func loadTicketView(r *http.Request, q *dbgen.Queries) (ticketDTO, error) {
	t, err := loadTicket(r, q)
	if err != nil {
		return ticketDTO{}, err
	}
	return ticketView(r.Context(), q, t.ID)
}

func isStaff(role string) bool { return records.IsStaff(role) }

var referenceRE = regexp.MustCompile(`^([A-Za-z]{2,5})([0-9]+)$`)

// loadTicket resolves the {id} path segment: see resolveTicket.
func loadTicket(r *http.Request, q *dbgen.Queries) (dbgen.Task, error) {
	return resolveTicket(r.Context(), q, chi.URLParam(r, "id"))
}

// resolveTicket finds a ticket by a UUID, a reference such as INC42, or a bare number. A bare number means an
// incident, which keeps links from before there were several types.
// Rows hidden by RLS come back as ErrNoRows, which the caller maps to 404.
func resolveTicket(ctx context.Context, q *dbgen.Queries, raw string) (dbgen.Task, error) {
	if id, err := uuid.Parse(raw); err == nil {
		return q.GetTask(ctx, id)
	}
	if m := referenceRE.FindStringSubmatch(raw); m != nil {
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil || n < 1 {
			return dbgen.Task{}, notFound()
		}
		return q.GetTaskByReference(ctx, dbgen.GetTaskByReferenceParams{Prefix: strings.ToUpper(m[1]), Number: n})
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		return dbgen.Task{}, notFound()
	}
	return q.GetTaskByTypeNumber(ctx, dbgen.GetTaskByTypeNumberParams{Type: "incident", Number: n})
}

// intParam reads an optional whole-number query parameter within bounds, answering 400 itself when it is not one.
func intParam(w http.ResponseWriter, raw, name string, lo, hi int, into *int) bool {
	if raw == "" {
		return true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be %d to %d", name, lo, hi))
		return false
	}
	*into = n
	return true
}

// GET /tickets?type=&status=&priority=&assignee=&requester=&q=&limit=&offset=
// assignee and requester accept a user id or "me"; assignee also accepts "none". open=true hides resolved and closed.
func (h *handlers) listTickets(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	var args dbgen.ListTicketsParams
	args.Limit, args.Offset = defaultLimit, 0

	if s := qs.Get("status"); s != "" {
		if !h.knownStatus(s) {
			writeError(w, http.StatusBadRequest, "unknown status")
			return
		}
		args.Status = pgtype.Text{String: s, Valid: true}
	}
	if s := qs.Get("type"); s != "" {
		if s == "task" || !slices.Contains(apps.KnownRecordTypes, s) {
			writeError(w, http.StatusBadRequest, "unknown type")
			return
		}
		args.Type = pgtype.Text{String: s, Valid: true}
	}
	if s := qs.Get("priority"); s != "" {
		n, err := strconv.ParseInt(s, 10, 16)
		if err != nil || n < 1 || n > 4 {
			writeError(w, http.StatusBadRequest, "priority must be 1 to 4")
			return
		}
		args.Priority = pgtype.Int2{Int16: int16(n), Valid: true}
	}
	if s := qs.Get("limit"); s != "" {
		n, err := strconv.ParseInt(s, 10, 32)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, http.StatusBadRequest, "limit must be 1 to 100")
			return
		}
		args.Limit = int32(n)
	}
	if s := qs.Get("offset"); s != "" {
		n, err := strconv.ParseInt(s, 10, 32)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset must be 0 or more")
			return
		}
		args.Offset = int32(n)
	}
	if s := strings.TrimSpace(qs.Get("q")); s != "" {
		// Escape LIKE wildcards so the search is a plain substring match.
		s = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
		args.Search = pgtype.Text{String: s, Valid: true}
	}
	args.OpenOnly = qs.Get("open") == "true"
	p, _ := auth.PrincipalFrom(r.Context())
	if qs.Get("assignee") == "none" {
		args.Unassigned = true
		qs.Del("assignee")
	}
	for name, dst := range map[string]*uuid.NullUUID{"assignee": &args.AssigneeID, "requester": &args.RequesterID} {
		s := qs.Get(name)
		if s == "" {
			continue
		}
		id := p.UserID
		if s != "me" {
			var err error
			if id, err = uuid.Parse(s); err != nil {
				writeError(w, http.StatusBadRequest, name+" must be a user id or \"me\"")
				return
			}
		}
		*dst = uuid.NullUUID{UUID: id, Valid: true}
	}

	limit := args.Limit
	args.Limit++ // fetch one extra row to learn whether there is another page
	var rows []dbgen.ListTicketsRow
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		rows, err = q.ListTickets(r.Context(), args)
		return err
	}) {
		return
	}
	more := len(rows) > int(limit)
	if more {
		rows = rows[:limit]
	}
	items := make([]ticketDTO, len(rows))
	for i, t := range rows {
		items[i] = toTicket(t.Task, t.TypePrefix, t.RequesterName, t.AssigneeName, "")
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": args.Offset, "has_more": more})
}

// POST /tickets. Staff may file on behalf of another requester; everyone else files as themselves.
func (h *handlers) createTicket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title       string     `json:"title"`
		Description string     `json:"description"`
		Priority    *int16     `json:"priority"`
		RequesterID *uuid.UUID `json:"requester_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	var out ticketDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		saved, err := h.svc.Create(r.Context(), q, actorOf(p), ruleruns.Cause{}, records.CreateInput{
			Title: req.Title, Description: req.Description, Priority: req.Priority, RequesterID: req.RequesterID})
		if err != nil {
			return err
		}
		out = ticketDTO{Ticket: saved}
		out.Moves = h.movesFor(r.Context(), q, p, out)
		return nil
	}) {
		return
	}
	w.Header().Set("Location", "/api/v1/tickets/"+out.ID.String())
	writeJSON(w, http.StatusCreated, out)
}

func actorOf(p auth.Principal) records.Actor { return records.Actor{ID: p.UserID, Role: p.Role} }

func (h *handlers) getTicket(w http.ResponseWriter, r *http.Request) {
	var out ticketDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		var err error
		if out, err = loadTicketView(r, q); err != nil {
			return err
		}
		out.Moves = h.movesFor(r.Context(), q, p, out)
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PATCH /tickets/{id} (staff). Fields that are absent are left alone; "assignee_id": null unassigns.
func (h *handlers) updateTicket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title       *string         `json:"title"`
		Description *string         `json:"description"`
		Priority    *int16          `json:"priority"`
		AssigneeID  json.RawMessage `json:"assignee_id"`
		Resolution  *string         `json:"resolution"`
	}
	if !decode(w, r, &req) {
		return
	}
	patch := records.Patch{Title: req.Title, Description: req.Description, Priority: req.Priority, Resolution: req.Resolution}
	if req.AssigneeID != nil {
		patch.SetAssignee = true
		if string(req.AssigneeID) != "null" {
			var id uuid.UUID
			if err := json.Unmarshal(req.AssigneeID, &id); err != nil {
				writeError(w, http.StatusBadRequest, "assignee_id must be a user id or null")
				return
			}
			patch.AssigneeID = &id
		}
	}
	var out ticketDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		row, err := loadTicket(r, q)
		if err != nil {
			return err
		}
		saved, err := h.svc.Update(r.Context(), q, actorOf(p), ruleruns.Cause{}, row.ID, patch)
		if err != nil {
			return err
		}
		out = ticketDTO{Ticket: saved}
		out.Moves = h.movesFor(r.Context(), q, p, out) // an edit can change which moves are allowed
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /tickets/{id}/transitions (staff) with {"to": "<status>"}.
func (h *handlers) transitionTicket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To string `json:"to"`
	}
	if !decode(w, r, &req) {
		return
	}
	var out ticketDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		row, err := loadTicket(r, q)
		if err != nil {
			return err
		}
		saved, err := h.svc.Transition(r.Context(), q, actorOf(p), ruleruns.Cause{}, row.ID, req.To)
		if err != nil {
			return err
		}
		out = ticketDTO{Ticket: saved}
		out.Moves = h.movesFor(r.Context(), q, p, out)
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
