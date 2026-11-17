package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/cmdb"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
)

const (
	maxCIName        = 200
	maxCIDescription = 10000
	maxTicketCIs     = 100
)

// cmdbModelDTO is what configuration items can be: the installed classes, how items can relate, and the statuses
// every item has. All of it comes from apps except the statuses.
type cmdbModelDTO struct {
	Classes       []classDTO        `json:"classes"`
	Relationships []relationshipDTO `json:"relationships"`
	Statuses      []optionDTO       `json:"statuses"`
}

// classDTO is a kind of configuration item.
type classDTO struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	// Extends is the class it builds on; ancestors are every class it builds on, nearest first. Filtering items by a
	// class finds those of the classes that extend it too.
	Extends   string   `json:"extends,omitempty"`
	Ancestors []string `json:"ancestors"`
	// Fields are its fields, those of the classes it extends first.
	Fields []formFieldDTO `json:"fields"`
	App    string         `json:"app"`
}

// relationshipDTO is a way items can relate: label reads from the first item to the second ("depends on"),
// reverse from the second to the first ("used by"). With impact, trouble with the second item affects the first.
type relationshipDTO struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Reverse string `json:"reverse"`
	Impact  bool   `json:"impact"`
}

// ciSummaryDTO is an item as lists show it.
type ciSummaryDTO struct {
	ID    uuid.UUID `json:"id"`
	Class string    `json:"class"`
	// ClassLabel is the class's label; the class name itself when no installed app defines the class any more.
	ClassLabel string `json:"class_label"`
	Name       string `json:"name"`
	// Status is planned, active, maintenance or retired.
	Status           string  `json:"status"`
	SupportGroupName *string `json:"support_group_name"`
}

// ciDTO is one configuration item.
type ciDTO struct {
	ciSummaryDTO
	Description    string     `json:"description"`
	OwnerGroupID   *uuid.UUID `json:"owner_group_id"`
	OwnerGroupName *string    `json:"owner_group_name"`
	// SupportGroupID is the group that fixes it when it breaks.
	SupportGroupID *uuid.UUID `json:"support_group_id"`
	// Fields are its class's fields that have a value, by name.
	Fields        map[string]any `json:"fields"`
	CreatedByName *string        `json:"created_by_name"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	// Relationships are every relationship it is in, either way round (GET /cis/{id}, at most 500; null in the
	// answer to a change).
	Relationships []ciRelationDTO `json:"relationships"`
	// Tickets are the records linked to it that the reader may see, open ones first (GET /cis/{id}, at most 100;
	// null in the answer to a change).
	Tickets []ciTicketDTO `json:"tickets"`
}

// ciRelationDTO is one relationship as seen from an item.
type ciRelationDTO struct {
	ID   uuid.UUID `json:"id"`
	Type string    `json:"type"`
	// Label is how it reads from this item: the type's label when this item comes first, its reverse otherwise.
	Label string `json:"label"`
	// Outgoing says this item comes first ("this depends on item").
	Outgoing bool `json:"outgoing"`
	// Impact says trouble spreads along it (towards this item when outgoing, away from it otherwise).
	Impact bool         `json:"impact"`
	Item   ciSummaryDTO `json:"item"`
}

// ciTicketDTO is a record linked to an item.
type ciTicketDTO struct {
	ID            uuid.UUID `json:"id"`
	Reference     string    `json:"reference"`
	Type          string    `json:"type"`
	Title         string    `json:"title"`
	Status        string    `json:"status"`
	StateCategory string    `json:"state_category"`
	Priority      int16     `json:"priority"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// affectedDTO is an item trouble reaches.
type affectedDTO struct {
	ciSummaryDTO
	// Depth is how many relationships away it is.
	Depth int `json:"depth"`
	// ViaID and ViaName are the item it is reached through: one of those asked about when depth is 1.
	ViaID   uuid.UUID `json:"via_id"`
	ViaName string    `json:"via_name"`
}

// impactDTO is what trouble with some items reaches.
type impactDTO struct {
	Items []affectedDTO `json:"items"`
	// Truncated says the walk stopped early (more than 10 relationships away, or more than 1000 items).
	Truncated bool `json:"truncated"`
}

func (h *handlers) classLabel(class string) string {
	if c := h.apps.Class(class); c != nil {
		return c.Label
	}
	return class
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func uuidPtr(u uuid.NullUUID) *uuid.UUID {
	if !u.Valid {
		return nil
	}
	return &u.UUID
}

func (h *handlers) toCISummary(id uuid.UUID, class, name, status string, supportGroup pgtype.Text) ciSummaryDTO {
	return ciSummaryDTO{ID: id, Class: class, ClassLabel: h.classLabel(class), Name: name, Status: status, SupportGroupName: textPtr(supportGroup)}
}

func (h *handlers) toCI(c dbgen.GetCIRow) ciDTO {
	return ciDTO{ciSummaryDTO: h.toCISummary(c.ID, c.Class, c.Name, c.Status, c.SupportGroupName), Description: c.Description,
		OwnerGroupID: uuidPtr(c.OwnerGroupID), OwnerGroupName: textPtr(c.OwnerGroupName), SupportGroupID: uuidPtr(c.SupportGroupID),
		Fields: records.DecodeFields(c.Fields), CreatedByName: textPtr(c.CreatedByName), CreatedAt: c.CreatedAt.Time, UpdatedAt: c.UpdatedAt.Time}
}

func nullUUID(id *uuid.UUID) uuid.NullUUID {
	if id == nil {
		return uuid.NullUUID{}
	}
	return uuid.NullUUID{UUID: *id, Valid: true}
}

func validCIStatus(s string) bool {
	for _, o := range apps.CIStatuses {
		if o.Value == s {
			return true
		}
	}
	return false
}

// GET /cmdb/model (cmdb:read): the classes items can be, how they can relate, and their statuses.
func (h *handlers) getCMDBModel(w http.ResponseWriter, _ *http.Request) {
	out := cmdbModelDTO{Classes: []classDTO{}, Relationships: []relationshipDTO{}, Statuses: []optionDTO{}}
	for _, c := range h.apps.Classes() {
		d := classDTO{Name: c.Name, Label: c.Label, Description: c.Description, Extends: c.Extends, Ancestors: c.Ancestors,
			Fields: []formFieldDTO{}, App: c.App}
		if d.Ancestors == nil {
			d.Ancestors = []string{}
		}
		for _, fd := range c.All {
			d.Fields = append(d.Fields, toFieldDTO(fd))
		}
		out.Classes = append(out.Classes, d)
	}
	for _, rel := range h.apps.Relationships() {
		out.Relationships = append(out.Relationships, relationshipDTO{Name: rel.Name, Label: rel.Label, Reverse: rel.Reverse, Impact: rel.Impact})
	}
	for _, o := range apps.CIStatuses {
		out.Statuses = append(out.Statuses, optionDTO{Value: o.Value, Label: o.Label})
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /cis?q=&class=&status=&support_group=&include_retired=&limit=&offset= (cmdb:read): configuration items by name.
// q matches part of a name; class finds that class and every class that extends it; retired items are left out
// unless include_retired=true or status=retired. more says there are more after this page.
func (h *handlers) listCIs(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	limit, offset := defaultLimit, 0
	if !intParam(w, qs.Get("limit"), "limit", 1, maxLimit, &limit) || !intParam(w, qs.Get("offset"), "offset", 0, 1<<30, &offset) {
		return
	}
	args := dbgen.ListCIsParams{Limit: int32(limit + 1), Offset: int32(offset), InUse: qs.Get("include_retired") != "true"}
	if s := strings.TrimSpace(qs.Get("q")); s != "" {
		args.Search = pgtype.Text{String: likeEscape(s), Valid: true}
	}
	if c := qs.Get("class"); c != "" {
		args.Classes = h.apps.ClassAndDescendants(c)
	}
	if s := qs.Get("status"); s != "" {
		if !validCIStatus(s) {
			writeError(w, http.StatusBadRequest, "status must be planned, active, maintenance or retired")
			return
		}
		args.Status = pgtype.Text{String: s, Valid: true}
		args.InUse = false
	}
	if g := qs.Get("support_group"); g != "" {
		id, err := uuid.Parse(g)
		if err != nil {
			writeError(w, http.StatusBadRequest, "support_group must be a group id")
			return
		}
		args.SupportGroupID = uuid.NullUUID{UUID: id, Valid: true}
	}
	out := []ciSummaryDTO{}
	more := false
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListCIs(r.Context(), args)
		if len(rows) > limit {
			rows, more = rows[:limit], true
		}
		for _, c := range rows {
			out = append(out, h.toCISummary(c.ID, c.Class, c.Name, c.Status, c.SupportGroupName))
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "more": more})
}

// ciRequest is an item as it is added or changed. On PATCH, a field left out keeps its value; owner_group_id and
// support_group_id take null to clear them, and fields sets the class's fields by name (null clears one).
type ciRequest struct {
	Class          *string             `json:"class"`
	Name           *string             `json:"name"`
	Status         *string             `json:"status"`
	Description    *string             `json:"description"`
	OwnerGroupID   nullable[uuid.UUID] `json:"owner_group_id"`
	SupportGroupID nullable[uuid.UUID] `json:"support_group_id"`
	Fields         map[string]any      `json:"fields"`
}

// apply checks the request against the item as it is (zero on create) and returns the result.
func (h *handlers) applyCI(req ciRequest, cur dbgen.UpdateCIParams, creating bool) (dbgen.UpdateCIParams, error) {
	if req.Class != nil {
		cur.Class = *req.Class
	}
	if req.Name != nil {
		cur.Name = strings.TrimSpace(*req.Name)
	}
	if req.Status != nil {
		cur.Status = *req.Status
	}
	if req.Description != nil {
		cur.Description = strings.TrimSpace(*req.Description)
	}
	if req.OwnerGroupID.Set {
		cur.OwnerGroupID = nullUUID(req.OwnerGroupID.Value)
	}
	if req.SupportGroupID.Set {
		cur.SupportGroupID = nullUUID(req.SupportGroupID.Value)
	}
	switch n := utf8.RuneCountInString(cur.Name); {
	case n < 1 || n > maxCIName:
		return cur, invalid(fmt.Sprintf("name must be 1 to %d characters", maxCIName))
	case utf8.RuneCountInString(cur.Description) > maxCIDescription:
		return cur, invalid(fmt.Sprintf("description must be at most %d characters", maxCIDescription))
	case !validCIStatus(cur.Status):
		return cur, invalid("status must be planned, active, maintenance or retired")
	}
	class := h.apps.Class(cur.Class)
	if class == nil {
		if creating || req.Class != nil {
			return cur, invalid(fmt.Sprintf("there is no class %q: see GET /cmdb/model", cur.Class))
		}
		if len(req.Fields) > 0 {
			return cur, invalid(fmt.Sprintf("no installed app defines the class %s any more, so its fields cannot be changed", cur.Class))
		}
		return cur, nil
	}
	stored := records.DecodeFields(cur.Fields)
	changes := req.Fields
	if creating {
		for k, v := range class.Defaults() {
			if _, given := changes[k]; !given {
				if changes == nil {
					changes = map[string]any{}
				}
				changes[k] = v
			}
		}
	}
	fields, msg := cmdb.ApplyFields(class, stored, changes)
	if msg != "" {
		return cur, invalid(msg)
	}
	cur.Fields = records.EncodeFields(fields)
	return cur, nil
}

// ciWriteError explains the database's refusals of an item that people can act on.
func (h *handlers) ciWriteError(err error, class, name string) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return &apiError{http.StatusConflict, fmt.Sprintf("there is already a %s called %s", strings.ToLower(h.classLabel(class)), name)}
	}
	return err
}

// POST /cis (cmdb:manage) with {"class", "name", "status"?, "description"?, "owner_group_id"?, "support_group_id"?,
// "fields"?}: add a configuration item. Status defaults to active; fields left out take the class's defaults.
func (h *handlers) createCI(w http.ResponseWriter, r *http.Request) {
	var req ciRequest
	if !decode(w, r, &req) {
		return
	}
	var out ciDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		c, err := h.applyCI(req, dbgen.UpdateCIParams{Status: "active", Fields: []byte("{}")}, true)
		if err != nil {
			return err
		}
		id, err := q.CreateCI(r.Context(), dbgen.CreateCIParams{Class: c.Class, Name: c.Name, Status: c.Status, Description: c.Description,
			OwnerGroupID: c.OwnerGroupID, SupportGroupID: c.SupportGroupID, Fields: c.Fields})
		if err != nil {
			return h.ciWriteError(err, c.Class, c.Name)
		}
		got, err := q.GetCI(r.Context(), id)
		out = h.toCI(got)
		return err
	}) {
		return
	}
	w.Header().Set("Location", "/api/v1/cis/"+out.ID.String())
	writeJSON(w, http.StatusCreated, out)
}

// GET /cis/{id} (cmdb:read): one configuration item, with its relationships and the records linked to it.
func (h *handlers) getCI(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var out ciDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		got, err := q.GetCI(r.Context(), id)
		if err != nil {
			return err
		}
		out = h.toCI(got)
		rels, err := q.ListCIRelationships(r.Context(), id)
		if err != nil {
			return err
		}
		out.Relationships = []ciRelationDTO{}
		for _, rel := range rels {
			d := ciRelationDTO{ID: rel.ID, Type: rel.Type, Label: rel.Type, Outgoing: rel.Outgoing,
				Item: h.toCISummary(rel.OtherID, rel.OtherClass, rel.OtherName, rel.OtherStatus, pgtype.Text{})}
			if def := h.apps.Relationship(rel.Type); def != nil {
				d.Label, d.Impact = def.Reverse, def.Impact
				if rel.Outgoing {
					d.Label = def.Label
				}
			}
			out.Relationships = append(out.Relationships, d)
		}
		tickets, err := q.ListCITasks(r.Context(), id)
		out.Tickets = []ciTicketDTO{}
		for _, t := range tickets {
			out.Tickets = append(out.Tickets, ciTicketDTO{ID: t.ID, Reference: t.Reference, Type: t.Type,
				Title: t.Title, Status: t.Status, StateCategory: t.StateCategory, Priority: t.Priority, UpdatedAt: t.UpdatedAt.Time})
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PATCH /cis/{id} (cmdb:manage) with any of {"class", "name", "status", "description", "owner_group_id",
// "support_group_id", "fields"}. Changing the class keeps the fields the new class also has.
func (h *handlers) updateCI(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req ciRequest
	if !decode(w, r, &req) {
		return
	}
	var out ciDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		cur, err := q.GetCI(r.Context(), id)
		if err != nil {
			return err
		}
		c, err := h.applyCI(req, dbgen.UpdateCIParams{ID: id, Class: cur.Class, Name: cur.Name, Status: cur.Status, Description: cur.Description,
			OwnerGroupID: cur.OwnerGroupID, SupportGroupID: cur.SupportGroupID, Fields: cur.Fields}, false)
		if err != nil {
			return err
		}
		if req.Class != nil && *req.Class != cur.Class {
			c.Fields = h.keepClassFields(c.Class, c.Fields)
		}
		if _, err := q.UpdateCI(r.Context(), c); err != nil {
			return h.ciWriteError(err, c.Class, c.Name) // pgx.ErrNoRows: RLS hid the row, so they may not change items
		}
		got, err := q.GetCI(r.Context(), id)
		out = h.toCI(got)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// keepClassFields drops the stored fields a class does not have, when an item moves to it.
func (h *handlers) keepClassFields(class string, raw []byte) []byte {
	c := h.apps.Class(class)
	fields := records.DecodeFields(raw)
	for name := range fields {
		if _, ok := c.Field(name); !ok {
			delete(fields, name)
		}
	}
	return records.EncodeFields(fields)
}

// DELETE /cis/{id} (cmdb:manage): remove a configuration item and its relationships. One linked to any record cannot
// be removed (409): retire it instead, so the record keeps what it was about.
func (h *handlers) deleteCI(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		n, err := q.DeleteCI(r.Context(), id)
		var pe *pgconn.PgError
		switch {
		case errors.As(err, &pe) && pe.Code == "23503":
			return &apiError{http.StatusConflict, "records are linked to this item: retire it instead"}
		case err != nil:
			return err
		case n == 0:
			return notFound()
		}
		return nil
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// relationRequest relates an item to another: {"type", "to"} reads "this <type> to", {"type", "from"} reads
// "from <type> this". Give exactly one of to and from.
type relationRequest struct {
	Type string     `json:"type"`
	To   *uuid.UUID `json:"to"`
	From *uuid.UUID `json:"from"`
}

// POST /cis/{id}/relationships (cmdb:manage) with {"type", "to"} or {"type", "from"}: relate two items. The same
// relationship twice is a 409.
func (h *handlers) createCIRelationship(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req relationRequest
	if !decode(w, r, &req) {
		return
	}
	var out ciRelationDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		def := h.apps.Relationship(req.Type)
		switch {
		case def == nil:
			return invalid(fmt.Sprintf("there is no relationship type %q: see GET /cmdb/model", req.Type))
		case (req.To == nil) == (req.From == nil):
			return invalid("give exactly one of to and from: the other item's id")
		}
		// other is the item at the far end; this one comes first with to, second with from.
		var from, to, other uuid.UUID
		if req.To != nil {
			from, to, other = id, *req.To, *req.To
		} else {
			from, to, other = *req.From, id, *req.From
		}
		if other == id {
			return invalid("an item cannot be related to itself")
		}
		if _, err := q.GetCI(r.Context(), id); err != nil {
			return err
		}
		o, err := q.GetCI(r.Context(), other)
		if err != nil {
			return invalid("there is no item with that id")
		}
		relID, err := q.CreateCIRelationship(r.Context(), dbgen.CreateCIRelationshipParams{FromCi: from, ToCi: to, Type: req.Type})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return &apiError{http.StatusConflict, "these items are already related that way"}
		} else if err != nil {
			return err
		}
		out = ciRelationDTO{ID: relID, Type: def.Name, Label: def.Reverse, Outgoing: req.To != nil, Impact: def.Impact,
			Item: h.toCISummary(o.ID, o.Class, o.Name, o.Status, o.SupportGroupName)}
		if out.Outgoing {
			out.Label = def.Label
		}
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// DELETE /cis/{id}/relationships/{relId} (cmdb:manage): remove one of an item's relationships.
func (h *handlers) deleteCIRelationship(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	relID, ok := urlUUID(w, r, "relId")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		n, err := q.DeleteCIRelationship(r.Context(), dbgen.DeleteCIRelationshipParams{ID: relID, CiID: id})
		if err == nil && n == 0 {
			return notFound()
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) impactOf(r *http.Request, q *dbgen.Queries, start []uuid.UUID) (impactDTO, error) {
	out := impactDTO{Items: []affectedDTO{}}
	found, truncated, err := cmdb.Impact(r.Context(), q, start, h.apps.ImpactTypes())
	if err != nil {
		return out, err
	}
	out.Truncated = truncated
	names := map[uuid.UUID]string{}
	for _, a := range found {
		names[a.ID] = a.Name
	}
	var missing []uuid.UUID
	for _, a := range found {
		if _, ok := names[a.Via]; !ok && !slices.Contains(missing, a.Via) {
			missing = append(missing, a.Via)
		}
	}
	if len(missing) > 0 {
		rows, err := q.GetCIsByID(r.Context(), missing)
		if err != nil {
			return out, err
		}
		for _, c := range rows {
			names[c.ID] = c.Name
		}
	}
	for _, a := range found {
		out.Items = append(out.Items, affectedDTO{ciSummaryDTO: h.toCISummary(a.ID, a.Class, a.Name, a.Status, a.SupportGroupName),
			Depth: a.Depth, ViaID: a.Via, ViaName: names[a.Via]})
	}
	return out, nil
}

// GET /cis/{id}/impact (cmdb:read): the items trouble with this one reaches, following the relationships that carry
// impact backwards (what depends on it, what runs on it, and so on), nearest first.
func (h *handlers) getCIImpact(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var out impactDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if _, err := q.GetCI(r.Context(), id); err != nil {
			return err
		}
		var err error
		out, err = h.impactOf(r, q, []uuid.UUID{id})
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ticketCIs are the items linked to a record as the API shows them, for a reader who may see the CMDB (nil otherwise).
func (h *handlers) ticketCIs(p auth.Principal, t records.Ticket) []ciSummaryDTO {
	if !p.Can(authz.CMDBRead) {
		return nil
	}
	out := make([]ciSummaryDTO, 0, len(t.CIs))
	for _, c := range t.CIs {
		out = append(out, h.toCISummary(c.ID, c.Class, c.Name, c.Status, textOf(c.SupportGroup)))
	}
	return out
}

func textOf(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

// PUT /tickets/{id}/cis/{ciId} (cmdb:read, and update on the record's type): link an item to a record, such as a
// server an incident affects or a database a change touches. It is an update of the record, so its rules run (the
// cmdb app's route-to-support-group, say). Linking one already linked does nothing. Answers the record's linked items.
func (h *handlers) linkTicketCI(w http.ResponseWriter, r *http.Request) {
	if out, ok := h.changeTicketCI(w, r, true); ok {
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// DELETE /tickets/{id}/cis/{ciId} (cmdb:read, and update on the record's type): unlink an item from a record, as an
// update of the record. Answers the record's linked items.
func (h *handlers) unlinkTicketCI(w http.ResponseWriter, r *http.Request) {
	if out, ok := h.changeTicketCI(w, r, false); ok {
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// changeTicketCI links or unlinks the {ciId} item and returns the record's items; on false it has answered.
func (h *handlers) changeTicketCI(w http.ResponseWriter, r *http.Request, link bool) ([]ciSummaryDTO, bool) {
	ciID, ok := urlUUID(w, r, "ciId")
	if !ok {
		return nil, false
	}
	var out []ciSummaryDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		row, err := loadTicket(r, q)
		if err != nil {
			return err
		}
		cur, err := records.Load(r.Context(), q, row.ID)
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, 0, len(cur.CIs)+1)
		found := false
		for _, c := range cur.CIs {
			if c.ID == ciID {
				found = true
				if !link {
					continue
				}
			}
			ids = append(ids, c.ID)
		}
		switch {
		case !link && !found:
			return notFound()
		case link && found:
			out = h.ticketCIs(p, cur)
			return nil
		case link:
			ids = append(ids, ciID)
		}
		saved, err := h.svc.Update(r.Context(), q, actorOf(p), ruleruns.Cause{}, row.ID, records.Patch{SetCIs: true, CIs: ids})
		out = h.ticketCIs(p, saved)
		return err
	}) {
		return nil, false
	}
	return out, true
}

// GET /tickets/{id}/impact (cmdb:read): what trouble with the record's linked items reaches, as GET /cis/{id}/impact
// does for one item.
func (h *handlers) getTicketImpact(w http.ResponseWriter, r *http.Request) {
	var out impactDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		t, err := loadTicket(r, q)
		if err != nil {
			return err
		}
		linked, err := q.ListTaskCIs(r.Context(), t.ID)
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, len(linked))
		for i, c := range linked {
			ids[i] = c.ID
		}
		out, err = h.impactOf(r, q, ids)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
