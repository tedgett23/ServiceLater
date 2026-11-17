package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
)

const (
	defaultLimit = 25
	maxLimit     = 100
)

// changeFields are a change's own fields in a request. Absent fields are left alone; planned_start and planned_end
// take null to clear them.
type changeFields struct {
	// ChangeType is standard, normal or emergency.
	ChangeType *string `json:"change_type"`
	// Risk is low, medium, high, or "" for not assessed.
	Risk *string `json:"risk"`
	// Impact is low, medium, high, or "" for not assessed.
	Impact *string `json:"impact"`
	// PlannedStart is when the change's window opens.
	PlannedStart nullable[time.Time] `json:"planned_start"`
	// PlannedEnd is when the window closes, after the start.
	PlannedEnd         nullable[time.Time] `json:"planned_end"`
	ImplementationPlan *string             `json:"implementation_plan"`
	BackoutPlan        *string             `json:"backout_plan"`
	TestPlan           *string             `json:"test_plan"`
}

func (c *changeFields) patch() *records.ChangePatch {
	if c == nil {
		return nil
	}
	return &records.ChangePatch{ChangeType: c.ChangeType, Risk: c.Risk, Impact: c.Impact,
		SetPlannedStart: c.PlannedStart.Set, PlannedStart: c.PlannedStart.Value,
		SetPlannedEnd: c.PlannedEnd.Set, PlannedEnd: c.PlannedEnd.Value,
		ImplementationPlan: c.ImplementationPlan, BackoutPlan: c.BackoutPlan, Test: c.TestPlan}
}

// ticketDTO is a ticket plus, on single-ticket responses (not lists), what the person may do next and its SLA clocks,
// and on lists a summary of its clocks.
type ticketDTO struct {
	records.Ticket
	// Moves says what the person may do next, and why anything is blocked.
	Moves []moveDTO `json:"moves,omitempty"`
	// SLAs are the ticket's SLA clocks (single-ticket responses).
	SLAs []records.SLAClock `json:"slas,omitempty"`
	// SLADueAt is when the ticket's soonest running SLA clock falls due, and SLABreached whether any of its clocks
	// has breached (lists).
	SLADueAt    *time.Time `json:"sla_due_at,omitempty"`
	SLABreached bool       `json:"sla_breached,omitempty"`
	// FieldStates says, for each field of the type's form, whether it is shown, required and writable now, and which
	// options a choice offers (single-ticket responses). A field with no entry is shown, optional and writable.
	FieldStates map[string]apps.FieldState `json:"field_states,omitempty"`
	// CIs are the configuration items linked to it (single-ticket responses, for those who may read the CMDB).
	CIs []ciSummaryDTO `json:"cis,omitempty"`
	// RoutedAway is set on the response to a save that took the record out of the caller's sight: it now belongs to
	// a restricted group they are not in. This response is the last they see of it.
	RoutedAway bool `json:"routed_away,omitempty"`
	// Articles are the knowledge articles linked to it that the reader may read (single-ticket responses).
	Articles []articleRefDTO `json:"articles,omitempty"`
	// Attachments are the files attached to it (single-ticket responses).
	Attachments []attachmentDTO `json:"attachments,omitempty"`
	// CanAttach says the reader may attach files to it (single-ticket responses).
	CanAttach bool `json:"can_attach,omitempty"`
	// Tasks are the records under it that the reader may see, oldest first: a request's tasks (single-ticket
	// responses).
	Tasks []childTaskDTO `json:"tasks,omitempty"`
	// Incidents are the incidents linked to a problem that the reader may see, oldest first (single-ticket
	// responses, problems only).
	Incidents []childTaskDTO `json:"incidents,omitempty"`
	// Changes are the changes made to fix a problem that the reader may see, oldest first (single-ticket responses,
	// problems only).
	Changes []childTaskDTO `json:"changes,omitempty"`
	// LinkedChanges is how many changes were made to fix a problem (lists, problems only).
	LinkedChanges int64 `json:"linked_changes,omitempty"`
}

// childTaskDTO is a record under another: one team's part of the work.
type childTaskDTO struct {
	ID                  uuid.UUID `json:"id"`
	Type                string    `json:"type"`
	Reference           string    `json:"reference"`
	Title               string    `json:"title"`
	Status              string    `json:"status"`
	StateCategory       string    `json:"state_category"`
	AssigneeName        *string   `json:"assignee_name"`
	AssignmentGroupName *string   `json:"assignment_group_name"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// childTasks are the records under a record that the reader may see.
func childTasks(ctx context.Context, q *dbgen.Queries, id uuid.UUID) ([]childTaskDTO, error) {
	rows, err := q.ListChildTasks(ctx, uuid.NullUUID{UUID: id, Valid: true})
	out := make([]childTaskDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, summaryOf(r.Task, r.Reference, r.AssigneeName, r.AssignmentGroupName))
	}
	return out, err
}

// problemIncidents are the incidents linked to a problem that the reader may see.
func problemIncidents(ctx context.Context, q *dbgen.Queries, id uuid.UUID) ([]childTaskDTO, error) {
	rows, err := q.ListProblemIncidents(ctx, uuid.NullUUID{UUID: id, Valid: true})
	out := make([]childTaskDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, summaryOf(r.Task, r.Reference, r.AssigneeName, r.AssignmentGroupName))
	}
	return out, err
}

// problemChanges are the changes made to fix a problem that the reader may see.
func problemChanges(ctx context.Context, q *dbgen.Queries, id uuid.UUID) ([]childTaskDTO, error) {
	rows, err := q.ListProblemChanges(ctx, uuid.NullUUID{UUID: id, Valid: true})
	out := make([]childTaskDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, summaryOf(r.Task, r.Reference, r.AssigneeName, r.AssignmentGroupName))
	}
	return out, err
}

func summaryOf(t dbgen.Task, reference string, assignee, group pgtype.Text) childTaskDTO {
	c := childTaskDTO{ID: t.ID, Type: t.Type, Reference: reference, Title: t.Title,
		Status: t.Status, StateCategory: t.StateCategory, UpdatedAt: t.UpdatedAt.Time}
	if assignee.Valid {
		c.AssigneeName = &assignee.String
	}
	if group.Valid {
		c.AssignmentGroupName = &group.String
	}
	return c
}

// detail adds what a single-ticket response carries beyond the ticket: the moves, field states, linked items and the
// SLA clocks.
func (h *handlers) detail(ctx context.Context, q *dbgen.Queries, p auth.Principal, out *ticketDTO) error {
	out.Moves = h.movesFor(ctx, q, p, *out)
	out.FieldStates = h.svc.FieldStates(ctx, q, actorOf(p), out.Ticket)
	var err error
	out.CIs = h.ticketCIs(p, out.Ticket)
	if out.Articles, err = articlesOf(ctx, q, out.ID); err != nil {
		return err
	}
	if out.Attachments, err = attachmentsOf(ctx, q, p, out.ID, out.Type); err != nil {
		return err
	}
	out.CanAttach = out.RequesterID == p.UserID || p.Perms.CanRecord(authz.Update, out.Type)
	if out.Tasks, err = childTasks(ctx, q, out.ID); err != nil {
		return err
	}
	if out.Type == "problem" {
		if out.Incidents, err = problemIncidents(ctx, q, out.ID); err != nil {
			return err
		}
		if out.Changes, err = problemChanges(ctx, q, out.ID); err != nil {
			return err
		}
	}
	if out.SLAs, err = records.LoadSLAs(ctx, q, out.ID); err != nil {
		return err
	}
	still, err := q.StillSees(ctx, out.ID)
	out.RoutedAway = !still
	return err
}

func toTicket(t dbgen.Task, prefix, reference, requesterName string, assigneeName, groupName pgtype.Text, resolution string) ticketDTO {
	return ticketDTO{Ticket: records.FromParts(t, prefix, reference, requesterName, assigneeName, groupName, resolution)}
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

// likeEscape escapes LIKE wildcards so a search is a plain substring match.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

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

// queueCountsDTO is how many records each view of the queue holds for the caller.
type queueCountsDTO struct {
	// Mine: open work assigned to the caller.
	Mine int64 `json:"mine"`
	// Groups: open work routed to a group the caller is in.
	Groups int64 `json:"groups"`
	// Unassigned: open work nobody has taken.
	Unassigned int64 `json:"unassigned"`
	// Due: open work with an SLA clock running or breached, which the Due view lists first.
	Due int64 `json:"due"`
	// Open: all open and waiting work the caller may see.
	Open int64 `json:"open"`
}

// GET /tickets/counts: how many records each view of the queue holds, counted as the caller (an end user's are
// their own). Only open and waiting work is counted; there is no count of everything.
func (h *handlers) countTickets(w http.ResponseWriter, r *http.Request) {
	var out queueCountsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		c, err := q.CountQueueViews(r.Context())
		out = queueCountsDTO{Mine: c.Mine, Groups: c.MyGroups, Unassigned: c.Unassigned, Due: c.OnAClock, Open: c.Open}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /tickets?type=&status=&priority=&assignee=&requester=&assignment_group=&q=&breached=&sort=&limit=&offset=
// assignee and requester accept a user id or "me"; assignee also accepts "none". assignment_group accepts a group id,
// "mine" (any group the caller is in) or "none". open=true hides resolved and closed. breached=true keeps tickets with
// a breached SLA; sort=due lists those first, then the soonest SLA due time (newest first otherwise).
// fields.<name>=<value> keeps records whose extra field has that value: true and false match a yes/no field, anything
// else matches text or a choice exactly (fields.known_error=true lists known errors).
func (h *handlers) listTickets(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	var args dbgen.ListTicketsParams
	args.Limit, args.Offset = defaultLimit, 0
	if match, msg := fieldFilter(qs); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	} else if match != nil {
		args.Fields = match
	}

	if s := qs.Get("status"); s != "" {
		if !h.knownStatus(s) {
			writeError(w, http.StatusBadRequest, "unknown status")
			return
		}
		args.Status = pgtype.Text{String: s, Valid: true}
	}
	if s := qs.Get("type"); s != "" {
		if s == "task" || !h.apps.KnownType(s) {
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
		args.Search = pgtype.Text{String: likeEscape(s), Valid: true}
	}
	args.OpenOnly = qs.Get("open") == "true"
	args.Breached = qs.Get("breached") == "true"
	switch qs.Get("sort") {
	case "", "created":
	case "due":
		args.ByDue = true
	default:
		writeError(w, http.StatusBadRequest, `sort must be "due" or "created"`)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if qs.Get("assignee") == "none" {
		args.Unassigned = true
		qs.Del("assignee")
	}
	switch g := qs.Get("assignment_group"); g {
	case "":
	case "mine":
		args.MyGroups = true
	case "none":
		args.NoGroup = true
	default:
		id, err := uuid.Parse(g)
		if err != nil {
			writeError(w, http.StatusBadRequest, `assignment_group must be a group id, "mine" or "none"`)
			return
		}
		args.AssignmentGroupID = uuid.NullUUID{UUID: id, Valid: true}
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
		items[i] = toTicket(t.Task, t.TypePrefix, t.Reference, t.RequesterName, t.AssigneeName, t.AssignmentGroupName, "")
		if t.SlaDueAt.Valid {
			due := t.SlaDueAt.Time
			items[i].SLADueAt = &due
		}
		items[i].SLABreached = t.SlaBreached
		items[i].OpenIncidents, items[i].LinkedChanges = t.OpenIncidents, t.LinkedChanges
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": args.Offset, "has_more": more})
}

var fieldNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// fieldFilter is the fields.<name> parameters as the JSON object a record's extra fields must contain, or nil.
func fieldFilter(qs url.Values) ([]byte, string) {
	match := map[string]any{}
	for key, values := range qs {
		name, ok := strings.CutPrefix(key, "fields.")
		if !ok {
			continue
		}
		if !fieldNameRE.MatchString(name) || len(values) != 1 {
			return nil, "fields." + name + " must name an extra field once"
		}
		switch v := values[0]; v {
		case "true", "false":
			match[name] = v == "true"
		default:
			match[name] = v
		}
	}
	if len(match) == 0 {
		return nil, ""
	}
	raw, _ := json.Marshal(match)
	return raw, ""
}

// POST /tickets: file a record of any type in use (GET /record-types says which the caller may). Anyone may file an
// incident; other types need records:create:<type>, and naming another requester needs records:update:<type> too.
func (h *handlers) createTicket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		// Type is the record type; incident when absent.
		Type        string     `json:"type"`
		Title       string     `json:"title"`
		Description string     `json:"description"`
		Priority    *int16     `json:"priority"`
		RequesterID *uuid.UUID `json:"requester_id"`
		// Change fills in a change's own fields; only for type change.
		Change *changeFields `json:"change"`
		// Fields fills in extra fields, by name, as the type's form declares them (GET /forms/{type}).
		Fields map[string]any `json:"fields"`
		// CIs links configuration items to it, by id (needs cmdb:read).
		CIs []uuid.UUID `json:"cis"`
		// ParentID files it under another record (a task under a request).
		ParentID *uuid.UUID `json:"parent_id"`
		// AssignmentGroupID routes it to a team as it is filed.
		AssignmentGroupID *uuid.UUID `json:"assignment_group_id"`
		// ProblemID links a new incident to the problem behind it, or a new change to the problem it fixes.
		ProblemID *uuid.UUID `json:"problem_id"`
		// Incidents links these incidents to a new problem, each an edit of that incident (as PATCH problem_id
		// would be), in the same transaction: a problem raised from an incident starts out linked to it.
		Incidents []uuid.UUID `json:"incidents"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Incidents) > 0 && req.Type != "problem" {
		writeError(w, http.StatusUnprocessableEntity, "only a problem has incidents linked to it")
		return
	}
	if len(req.Incidents) > maxLinkedOnCreate {
		writeError(w, http.StatusUnprocessableEntity, "link at most 100 incidents as a problem is filed")
		return
	}
	if req.Type == "" {
		req.Type = "incident"
	}
	var out ticketDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		saved, err := h.svc.Create(r.Context(), q, actorOf(p), ruleruns.Cause{}, records.CreateInput{Type: req.Type,
			Title: req.Title, Description: req.Description, Priority: req.Priority, RequesterID: req.RequesterID,
			Change: req.Change.patch(), Fields: req.Fields, CIs: req.CIs, ParentID: req.ParentID, AssignmentGroupID: req.AssignmentGroupID,
			ProblemID: req.ProblemID})
		if err != nil {
			return err
		}
		if len(req.Incidents) > 0 {
			for _, id := range req.Incidents {
				if _, err := h.svc.Update(r.Context(), q, actorOf(p), ruleruns.Cause{}, id, records.Patch{SetProblem: true, ProblemID: &saved.ID}); err != nil {
					return err
				}
			}
			if saved, err = records.Load(r.Context(), q, saved.ID); err != nil {
				return err
			}
		}
		out = ticketDTO{Ticket: saved}
		return h.detail(r.Context(), q, p, &out)
	}) {
		return
	}
	w.Header().Set("Location", "/api/v1/tickets/"+out.ID.String())
	writeJSON(w, http.StatusCreated, out)
}

// maxLinkedOnCreate is how many incidents a new problem may be linked to as it is filed.
const maxLinkedOnCreate = 100

func actorOf(p auth.Principal) records.Actor { return records.Actor{ID: p.UserID, Perms: p.Perms} }

// GET /tickets/{id}: one ticket, by id, reference (INC42) or incident number, with the moves the caller may make.
func (h *handlers) getTicket(w http.ResponseWriter, r *http.Request) {
	var out ticketDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		var err error
		if out, err = loadTicketView(r, q); err != nil {
			return err
		}
		return h.detail(r.Context(), q, p, &out)
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PATCH /tickets/{id} (staff). Fields that are absent are left alone; "assignee_id": null unassigns and
// "assignment_group_id": null takes the ticket out of its group. Moving a ticket to a group its assignee is not in
// also unassigns it; naming an assignee outside the group is refused.
func (h *handlers) updateTicket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title             *string             `json:"title"`
		Description       *string             `json:"description"`
		Priority          *int16              `json:"priority"`
		AssigneeID        nullable[uuid.UUID] `json:"assignee_id"`
		AssignmentGroupID nullable[uuid.UUID] `json:"assignment_group_id"`
		Resolution        *string             `json:"resolution"`
		// Change edits a change's own fields; only for a change.
		Change *changeFields `json:"change"`
		// Fields sets extra fields by name; null clears one. Fields not named are left alone.
		Fields map[string]any `json:"fields"`
		// ProblemID links an incident to the problem behind it, or a change to the problem it fixes; null unlinks it.
		ProblemID nullable[uuid.UUID] `json:"problem_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	patch := records.Patch{SetProblem: req.ProblemID.Set, ProblemID: req.ProblemID.Value, Title: req.Title, Description: req.Description, Priority: req.Priority, Resolution: req.Resolution,
		Change: req.Change.patch(), Fields: req.Fields,
		SetAssignee: req.AssigneeID.Set, AssigneeID: req.AssigneeID.Value,
		SetAssignmentGroup: req.AssignmentGroupID.Set, AssignmentGroupID: req.AssignmentGroupID.Value}
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
		return h.detail(r.Context(), q, p, &out) // an edit can change which moves are allowed
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
		return h.detail(r.Context(), q, p, &out)
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
