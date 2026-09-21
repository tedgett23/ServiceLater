package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

const (
	maxTitle       = 200
	maxDescription = 10000
	maxResolution  = 10000
	defaultLimit   = 25
	maxLimit       = 100
)

type ticketDTO struct {
	ID uuid.UUID `json:"id"`
	// Type is the kind of ticket (incident, change ...), Number counts within that type, and Reference is how people
	// name it: the type's prefix and the number, such as INC42.
	Type          string     `json:"type"`
	Number        int64      `json:"number"`
	Reference     string     `json:"reference"`
	Title         string     `json:"title"`
	Description   string     `json:"description"`
	Status        string     `json:"status"`
	StateCategory string     `json:"state_category"`
	Priority      int16      `json:"priority"`
	RequesterID   uuid.UUID  `json:"requester_id"`
	Requester     string     `json:"requester_name"`
	AssigneeID    *uuid.UUID `json:"assignee_id"`
	Assignee      *string    `json:"assignee_name"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	// Resolution is how an incident was fixed. It lives in the incident's own table; other types have none.
	Resolution string `json:"resolution"`
	// Moves is set on single-ticket responses (not lists): what the person may do next, and why anything is blocked.
	Moves []moveDTO `json:"moves,omitempty"`
}

func toTicket(t dbgen.Task, prefix, requesterName string, assigneeName pgtype.Text, resolution string) ticketDTO {
	d := ticketDTO{ID: t.ID, Type: t.Type, Number: t.Number, Reference: prefix + strconv.FormatInt(t.Number, 10), Resolution: resolution, Title: t.Title, Description: t.Description, Status: t.Status, StateCategory: t.StateCategory,
		Priority: t.Priority, RequesterID: t.RequesterID, Requester: requesterName,
		CreatedAt: t.CreatedAt.Time, UpdatedAt: t.UpdatedAt.Time}
	if t.AssigneeID.Valid {
		id := t.AssigneeID.UUID
		d.AssigneeID = &id
	}
	if assigneeName.Valid {
		d.Assignee = &assigneeName.String
	}
	return d
}

// ticketView loads a ticket with its display names, by id, as one RLS-scoped query.
func ticketView(ctx context.Context, q *dbgen.Queries, id uuid.UUID) (ticketDTO, error) {
	v, err := q.GetTicketView(ctx, id)
	if err != nil {
		return ticketDTO{}, err
	}
	return toTicket(v.Task, v.TypePrefix, v.RequesterName, v.AssigneeName, v.Resolution), nil
}

// loadTicketView is loadTicket plus display names.
func loadTicketView(r *http.Request, q *dbgen.Queries) (ticketDTO, error) {
	t, err := loadTicket(r, q)
	if err != nil {
		return ticketDTO{}, err
	}
	return ticketView(r.Context(), q, t.ID)
}

func isStaff(role string) bool { return role == "admin" || role == "agent" }

func validTitle(s string) error {
	if n := utf8.RuneCountInString(strings.TrimSpace(s)); n < 1 || n > maxTitle {
		return invalid("title must be 1 to 200 characters")
	}
	return nil
}

func validPriority(p int16) error {
	if p < 1 || p > 4 {
		return invalid("priority must be 1 (critical) to 4 (low)")
	}
	return nil
}

var referenceRE = regexp.MustCompile(`^([A-Za-z]{2,5})([0-9]+)$`)

// loadTicket resolves the {id} path segment: a UUID, a reference such as INC42, or a bare number. A bare number
// means an incident, which keeps links from before there were several types.
// Rows hidden by RLS come back as ErrNoRows, which the caller maps to 404.
func loadTicket(r *http.Request, q *dbgen.Queries) (dbgen.Task, error) {
	raw := chi.URLParam(r, "id")
	if id, err := uuid.Parse(raw); err == nil {
		return q.GetTask(r.Context(), id)
	}
	if m := referenceRE.FindStringSubmatch(raw); m != nil {
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil || n < 1 {
			return dbgen.Task{}, notFound()
		}
		return q.GetTaskByReference(r.Context(), dbgen.GetTaskByReferenceParams{Prefix: strings.ToUpper(m[1]), Number: n})
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		return dbgen.Task{}, notFound()
	}
	return q.GetTaskByTypeNumber(r.Context(), dbgen.GetTaskByTypeNumberParams{Type: "incident", Number: n})
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
		if err != nil || validPriority(int16(n)) != nil {
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
		if err := validTitle(req.Title); err != nil {
			return err
		}
		if len(req.Description) > maxDescription {
			return invalid("description too long")
		}
		prio := int16(3)
		if req.Priority != nil {
			prio = *req.Priority
		}
		if err := validPriority(prio); err != nil {
			return err
		}
		requester := p.UserID
		if req.RequesterID != nil && *req.RequesterID != p.UserID {
			if !isStaff(p.Role) {
				return forbidden("only staff can file on behalf of others")
			}
			requester = *req.RequesterID
		}
		// A ticket is a task plus its type's own row. Both are written in this transaction, or neither is.
		lc := h.lifecycleFor("incident")
		category, _ := lc.Category(lc.Initial)
		t, err := q.CreateTask(r.Context(), dbgen.CreateTaskParams{
			Type: "incident", Title: strings.TrimSpace(req.Title), Description: req.Description, Priority: prio, RequesterID: requester,
			Status: lc.Initial, StateCategory: string(category)})
		if err != nil {
			return err
		}
		if err := q.CreateIncident(r.Context(), t.ID); err != nil {
			return err
		}
		if out, err = ticketView(r.Context(), q, t.ID); err != nil {
			return err
		}
		out.Moves = h.movesFor(r.Context(), q, p, out)
		return nil
	}) {
		return
	}
	w.Header().Set("Location", "/api/v1/tickets/"+out.ID.String())
	writeJSON(w, http.StatusCreated, out)
}

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
	var out ticketDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		args := dbgen.UpdateTaskParams{}
		if req.Title != nil {
			if err := validTitle(*req.Title); err != nil {
				return err
			}
			args.Title = pgtype.Text{String: strings.TrimSpace(*req.Title), Valid: true}
		}
		if req.Description != nil {
			if len(*req.Description) > maxDescription {
				return invalid("description too long")
			}
			args.Description = pgtype.Text{String: *req.Description, Valid: true}
		}
		if req.Priority != nil {
			if err := validPriority(*req.Priority); err != nil {
				return err
			}
			args.Priority = pgtype.Int2{Int16: *req.Priority, Valid: true}
		}
		if req.AssigneeID != nil {
			args.SetAssignee = true
			if string(req.AssigneeID) != "null" {
				var id uuid.UUID
				if err := json.Unmarshal(req.AssigneeID, &id); err != nil {
					return badRequest("assignee_id must be a user id or null")
				}
				u, err := q.GetUser(r.Context(), id)
				if err != nil {
					return invalid("assignee does not exist")
				}
				if !isStaff(u.Role) {
					return invalid("assignee must be an agent or admin")
				}
				args.AssigneeID = uuid.NullUUID{UUID: id, Valid: true}
			}
		}
		t, err := loadTicket(r, q)
		if err != nil {
			return err
		}
		args.ID = t.ID
		if _, err = q.UpdateTask(r.Context(), args); err != nil {
			return err
		}
		if req.Resolution != nil {
			// The resolution belongs to the incident's own row, so only tickets of that type have one.
			if t.Type != "incident" {
				return invalid("this kind of ticket has no resolution notes")
			}
			if len(*req.Resolution) > maxResolution {
				return invalid("resolution notes are too long")
			}
			if err := q.UpdateIncident(r.Context(), dbgen.UpdateIncidentParams{TaskID: t.ID, Resolution: *req.Resolution}); err != nil {
				return err
			}
		}
		if out, err = ticketView(r.Context(), q, t.ID); err != nil {
			return err
		}
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
		t, err := loadTicket(r, q)
		if err != nil {
			return err
		}
		// Lock the row so two concurrent transitions can't both pass the check against the same state.
		if t, err = q.GetTaskForUpdate(r.Context(), t.ID); err != nil {
			return err
		}
		// Which lifecycle applies depends on the ticket's type, so it is known only once the ticket is loaded.
		lc := h.lifecycleFor(t.Type)
		if !lc.HasState(req.To) {
			return invalid("unknown status")
		}
		if !lc.CanTransition(t.Status, req.To) {
			return conflict("cannot move a " + lc.Label(t.Status) + " ticket to " + lc.Label(req.To))
		}
		// The lifecycle's guard for this move, evaluated against the ticket as it would be after it. Anything other
		// than a definite yes blocks the move. What a guard needs is only loaded when the move has one.
		if lc.Guarded(t.Status, req.To) {
			cur, err := ticketView(r.Context(), q, t.ID)
			if err != nil {
				return err
			}
			user, err := q.GetUser(r.Context(), p.UserID)
			if err != nil {
				return err
			}
			ok, message, err := lc.Check(r.Context(), t.Status, req.To, guardInput(cur, req.To, lc, user))
			if err != nil {
				slog.Error("move guard", "ticket", cur.Number, "from", t.Status, "to", req.To, "err", err)
				return invalid(cannotCheck)
			}
			if !ok {
				return invalid(message)
			}
		}
		category, _ := lc.Category(req.To)
		if _, err = q.UpdateTaskStatus(r.Context(), dbgen.UpdateTaskStatusParams{ID: t.ID, Status: req.To, StateCategory: string(category)}); err != nil {
			return err
		}
		if out, err = ticketView(r.Context(), q, t.ID); err != nil {
			return err
		}
		out.Moves = h.movesFor(r.Context(), q, p, out)
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
