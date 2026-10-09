// Package records is the one place a ticket is saved: created, edited or moved. The api role calls it for a
// person's request and the worker role calls it for an async rule's update, so the same validation, before and after
// rules, audit and queuing happen whoever asks. Nothing else may write to tasks.
package records

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
)

const (
	MaxTitle       = 200
	MaxDescription = 10000
	MaxResolution  = 10000
	MaxComment     = 10000
	MaxPlan        = 10000
	// MaxCIs is how many configuration items one record can be linked to.
	MaxCIs = 100
)

// Ticket is a ticket as clients see it: the task, display names, the type's prefix and the type's own columns.
type Ticket struct {
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
	// AssignmentGroupID is the team the ticket is routed to. An assignee, if there is one, is a member of it.
	AssignmentGroupID *uuid.UUID `json:"assignment_group_id"`
	AssignmentGroup   *string    `json:"assignment_group_name"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	// Resolution is how an incident was fixed. It lives in the incident's own table; other types have none.
	Resolution string `json:"resolution"`
	// Change is a change's own fields; absent for other types.
	Change *Change `json:"change,omitempty"`
	// Fields are the record's extra fields, the ones its type's form declares, by name: text, a whole number, true or
	// false, a date as YYYY-MM-DD or a time as RFC 3339 text. A field that is not set is absent.
	Fields map[string]any `json:"fields"`
	// ParentID is the record this one is part of (a request, for its tasks), and ParentReference how people name it.
	ParentID        *uuid.UUID `json:"parent_id"`
	ParentReference *string    `json:"parent_reference,omitempty"`
	// OpenTasks is how many of the tasks under it (that the reader may see) are still open or waiting (single
	// tickets only).
	OpenTasks int64 `json:"open_tasks"`
	// CatalogItem is the catalog item a request was ordered from (empty for anything else): its form is the record's.
	CatalogItem string `json:"catalog_item,omitempty"`
	// RequesterManagerID is the requester's manager, for expressions (approval by the manager).
	RequesterManagerID *uuid.UUID `json:"-"`
	// CIs are the configuration items linked to it, by name, as the reader may see them (none without cmdb:read).
	// The API sends them in its own shape, with class labels, on single tickets only.
	CIs []CI `json:"-"`
}

// CI is a configuration item linked to a record.
type CI struct {
	ID                           uuid.UUID
	Class, Name, Status          string
	SupportGroupID, OwnerGroupID *uuid.UUID
	SupportGroup, OwnerGroup     *string
}

func ciFrom(r dbgen.GetCIsByIDRow) CI {
	c := CI{ID: r.ID, Class: r.Class, Name: r.Name, Status: r.Status}
	if r.SupportGroupID.Valid {
		c.SupportGroupID, c.SupportGroup = &r.SupportGroupID.UUID, &r.SupportGroupName.String
	}
	if r.OwnerGroupID.Valid {
		c.OwnerGroupID, c.OwnerGroup = &r.OwnerGroupID.UUID, &r.OwnerGroupName.String
	}
	return c
}

func sameCIs(a, b []CI) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

// Change is what a change record has beyond the base: its kind, assessment, planned window and plans.
type Change struct {
	// ChangeType is standard (pre-approved), normal (goes to the change advisory board) or emergency.
	ChangeType string `json:"change_type"`
	// Risk and Impact are low, medium or high; empty until assessed.
	Risk   string `json:"risk"`
	Impact string `json:"impact"`
	// PlannedStart and PlannedEnd are the window the change is done in; the end is after the start.
	PlannedStart       *time.Time `json:"planned_start"`
	PlannedEnd         *time.Time `json:"planned_end"`
	ImplementationPlan string     `json:"implementation_plan"`
	BackoutPlan        string     `json:"backout_plan"`
	TestPlan           string     `json:"test_plan"`
}

func (c *Change) clone() *Change {
	if c == nil {
		return nil
	}
	out := *c
	return &out
}

func sameChange(a, b *Change) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ChangeType == b.ChangeType && a.Risk == b.Risk && a.Impact == b.Impact &&
		sameTime(a.PlannedStart, b.PlannedStart) && sameTime(a.PlannedEnd, b.PlannedEnd) &&
		a.ImplementationPlan == b.ImplementationPlan && a.BackoutPlan == b.BackoutPlan && a.TestPlan == b.TestPlan
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// FromParts builds a ticket from a task row and the names around it.
func FromParts(t dbgen.Task, prefix, requesterName string, assigneeName, groupName pgtype.Text, resolution string) Ticket {
	d := Ticket{ID: t.ID, Type: t.Type, Number: t.Number, Reference: fmt.Sprintf("%s%d", prefix, t.Number), Resolution: resolution,
		Title: t.Title, Description: t.Description, Status: t.Status, StateCategory: t.StateCategory,
		Priority: t.Priority, RequesterID: t.RequesterID, Requester: requesterName,
		CreatedAt: t.CreatedAt.Time, UpdatedAt: t.UpdatedAt.Time, Fields: DecodeFields(t.Fields)}
	if t.AssigneeID.Valid {
		id := t.AssigneeID.UUID
		d.AssigneeID = &id
	}
	if assigneeName.Valid {
		d.Assignee = &assigneeName.String
	}
	if t.AssignmentGroupID.Valid {
		id := t.AssignmentGroupID.UUID
		d.AssignmentGroupID = &id
	}
	if groupName.Valid {
		d.AssignmentGroup = &groupName.String
	}
	if t.ParentID.Valid {
		id := t.ParentID.UUID
		d.ParentID = &id
	}
	return d
}

// DecodeFields reads the stored extra fields. Whole numbers come back as int64, as a rule or a form check gives them.
func DecodeFields(raw []byte) map[string]any {
	out := map[string]any{}
	if len(raw) == 0 {
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return map[string]any{}
	}
	for k, v := range out {
		if n, ok := v.(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				out[k] = i
			} else if f, err := n.Float64(); err == nil {
				out[k] = f
			}
		}
	}
	return out
}

// EncodeFields is DecodeFields backwards: the object as it is stored.
func EncodeFields(f map[string]any) []byte {
	if f == nil {
		return []byte("{}")
	}
	raw, _ := json.Marshal(f)
	return raw
}

// sameFields reports whether two sets of extra fields hold the same values.
func sameFields(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || !sameValue(v, w) {
			return false
		}
	}
	return true
}

func sameValue(a, b any) bool {
	ai, aok := wholeNumber(a)
	bi, bok := wholeNumber(b)
	if aok || bok {
		return aok && bok && ai == bi
	}
	return a == b
}

func wholeNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

func cloneFields(f map[string]any) map[string]any {
	out := make(map[string]any, len(f))
	for k, v := range f {
		out[k] = v
	}
	return out
}

// Load reads a ticket by id as one RLS-scoped query. A ticket the caller cannot see is pgx.ErrNoRows.
func Load(ctx context.Context, q *dbgen.Queries, id uuid.UUID) (Ticket, error) {
	v, err := q.GetTicketView(ctx, id)
	if err != nil {
		return Ticket{}, err
	}
	t := FromParts(v.Task, v.TypePrefix, v.RequesterName, v.AssigneeName, v.AssignmentGroupName, v.Resolution)
	if v.ChangeType.Valid {
		t.Change = &Change{ChangeType: v.ChangeType.String, Risk: v.Risk, Impact: v.Impact,
			PlannedStart: timePtr(v.PlannedStart), PlannedEnd: timePtr(v.PlannedEnd),
			ImplementationPlan: v.ImplementationPlan, BackoutPlan: v.BackoutPlan, TestPlan: v.TestPlan}
	}
	if v.ParentReference != "" {
		t.ParentReference = &v.ParentReference
	}
	if v.RequesterManagerID.Valid {
		id := v.RequesterManagerID.UUID
		t.RequesterManagerID = &id
	}
	t.OpenTasks, t.CatalogItem = v.OpenTasks, v.CatalogItem
	cis, err := q.ListTaskCIs(ctx, id)
	for _, c := range cis {
		t.CIs = append(t.CIs, ciFrom(dbgen.GetCIsByIDRow(c)))
	}
	return t, err
}

// ExprRecord is the ticket as expressions see it. It is the only converter: a field added to expr.Record is filled
// here, and TestExprRecordSetsEveryField fails until it is.
func (d Ticket) ExprRecord() *expr.Record {
	r := &expr.Record{
		ID: d.ID.String(), Type: d.Type, Number: d.Number, Reference: d.Reference, Title: d.Title, Description: d.Description,
		Resolution: d.Resolution, Status: d.Status,
		StateCategory: d.StateCategory, Priority: int64(d.Priority), RequesterID: d.RequesterID.String(),
		RequesterName: d.Requester, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, AssigneeName: d.Assignee,
		AssignmentGroup: d.AssignmentGroup, Fields: d.Fields, OpenTasks: d.OpenTasks, CatalogItem: d.CatalogItem,
	}
	if d.ParentID != nil {
		id := d.ParentID.String()
		r.ParentID = &id
	}
	if d.RequesterManagerID != nil {
		id := d.RequesterManagerID.String()
		r.RequesterManagerID = &id
	}
	if d.AssigneeID != nil {
		id := d.AssigneeID.String()
		r.AssigneeID = &id
	}
	if d.AssignmentGroupID != nil {
		id := d.AssignmentGroupID.String()
		r.AssignmentGroupID = &id
	}
	if len(d.CIs) > 0 {
		r.CIs = make([]expr.CI, len(d.CIs))
	}
	for i, c := range d.CIs {
		r.CIs[i] = expr.CI{ID: c.ID.String(), Name: c.Name, Class: c.Class, Status: c.Status, SupportGroup: c.SupportGroup, OwnerGroup: c.OwnerGroup}
		if c.SupportGroupID != nil {
			id := c.SupportGroupID.String()
			r.CIs[i].SupportGroupID = &id
		}
		if c.OwnerGroupID != nil {
			id := c.OwnerGroupID.String()
			r.CIs[i].OwnerGroupID = &id
		}
	}
	if c := d.Change; c != nil {
		r.ChangeType, r.Risk, r.Impact = c.ChangeType, c.Risk, c.Impact
		r.ImplementationPlan, r.BackoutPlan, r.TestPlan = c.ImplementationPlan, c.BackoutPlan, c.TestPlan
		if c.PlannedStart != nil {
			r.PlannedStart = *c.PlannedStart
		}
		if c.PlannedEnd != nil {
			r.PlannedEnd = *c.PlannedEnd
		}
	}
	return r
}

// validChange checks a change's fields as a person or a rule left them.
func validChange(c *Change) error {
	switch {
	case !slices.Contains(apps.ChangeTypes, c.ChangeType):
		return Invalid("change_type must be standard, normal or emergency")
	case !slices.Contains(apps.Levels, c.Risk):
		return Invalid("risk must be low, medium or high (or empty: not assessed)")
	case !slices.Contains(apps.Levels, c.Impact):
		return Invalid("impact must be low, medium or high (or empty: not assessed)")
	case c.PlannedStart != nil && c.PlannedEnd != nil && !c.PlannedEnd.After(*c.PlannedStart):
		return Invalid("the planned end must be after the planned start")
	case len(c.ImplementationPlan) > MaxPlan || len(c.BackoutPlan) > MaxPlan || len(c.TestPlan) > MaxPlan:
		return Invalid("a plan is too long")
	}
	return nil
}

func validTitle(s string) error {
	if n := utf8.RuneCountInString(strings.TrimSpace(s)); n < 1 || n > MaxTitle {
		return Invalid("title must be 1 to 200 characters")
	}
	return nil
}

func validPriority(p int16) error {
	if p < 1 || p > 4 {
		return Invalid("priority must be 1 (critical) to 4 (low)")
	}
	return nil
}
