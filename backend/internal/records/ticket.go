// Package records is the one place a ticket is saved: created, edited or moved. The api role calls it for a
// person's request and the worker role calls it for an async rule's update, so the same validation, before and after
// rules, audit and queuing happen whoever asks. Nothing else may write to tasks.
package records

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
)

const (
	MaxTitle       = 200
	MaxDescription = 10000
	MaxResolution  = 10000
	MaxComment     = 10000
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
	return FromParts(v.Task, v.TypePrefix, v.RequesterName, v.AssigneeName, v.AssignmentGroupName, v.Resolution), nil
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
	return r
}

func IsStaff(role string) bool { return role == "admin" || role == "agent" }

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
