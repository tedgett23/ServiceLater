// Package records is the one place a ticket is saved: created, edited or moved. The api role calls it for a
// person's request and the worker role calls it for an async rule's update, so the same validation, before and after
// rules, audit and queuing happen whoever asks. Nothing else may write to tasks.
package records

import (
	"context"
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
		CreatedAt: t.CreatedAt.Time, UpdatedAt: t.UpdatedAt.Time}
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
	return d
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
	return t, nil
}

// ExprRecord is the ticket as expressions see it. It is the only converter: a field added to expr.Record is filled
// here, and TestExprRecordSetsEveryField fails until it is.
func (d Ticket) ExprRecord() *expr.Record {
	r := &expr.Record{
		ID: d.ID.String(), Type: d.Type, Number: d.Number, Reference: d.Reference, Title: d.Title, Description: d.Description,
		Resolution: d.Resolution, Status: d.Status,
		StateCategory: d.StateCategory, Priority: int64(d.Priority), RequesterID: d.RequesterID.String(),
		RequesterName: d.Requester, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, AssigneeName: d.Assignee,
		AssignmentGroup: d.AssignmentGroup,
	}
	if d.AssigneeID != nil {
		id := d.AssigneeID.String()
		r.AssigneeID = &id
	}
	if d.AssignmentGroupID != nil {
		id := d.AssignmentGroupID.String()
		r.AssignmentGroupID = &id
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
