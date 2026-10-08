package notify

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
)

// Message is a notification as its recipient reads it.
type Message struct {
	ID         uuid.UUID
	Kind       string
	ApprovalID *uuid.UUID
	TaskID     *uuid.UUID
	Reference  string
	Title      string
	Body       string
	// Link is where it leads in the web app, as a path (/tickets/INC42).
	Link      string
	CreatedAt time.Time
	ReadAt    *time.Time
}

// Renderer fills in notifications for one reader, looking each ticket and person up once. q must run as the
// recipient (db.WithUser), so what a template reads is what they may see.
type Renderer struct {
	apps    *apps.Registry
	q       *dbgen.Queries
	tickets map[uuid.UUID]*records.Ticket // nil: not visible to this person
	names   map[uuid.UUID]string
}

func NewRenderer(reg *apps.Registry, q *dbgen.Queries) *Renderer {
	return &Renderer{apps: reg, q: q, tickets: map[uuid.UUID]*records.Ticket{}, names: map[uuid.UUID]string{}}
}

// Render returns the notification as its recipient sees it, and false for one about a ticket they can no longer
// see (it was sent when they could).
func (rd *Renderer) Render(ctx context.Context, n dbgen.Notification) (Message, bool, error) {
	out := Message{ID: n.ID, Kind: n.Kind, CreatedAt: n.CreatedAt.Time}
	if n.ReadAt.Valid {
		out.ReadAt = &n.ReadAt.Time
	}
	switch {
	case n.Kind == "approval_pending":
		// Still hard-coded: this kind predates templates. It should become a template the approvals app ships (see
		// docs/apps-platform.md, "Notifications").
		id := n.ApprovalID.UUID
		out.ApprovalID = &id
		out.Title, out.Link = "Something needs your approval", "/approvals"
		return out, true, nil
	case n.TaskID.Valid:
		t, err := rd.ticket(ctx, n.TaskID.UUID)
		if err != nil || t == nil {
			return out, false, err
		}
		id := t.ID
		out.TaskID, out.Reference, out.Link = &id, t.Reference, "/tickets/"+t.Reference
		tpl := rd.apps.Notification(n.App.String, n.Template.String)
		if tpl == nil {
			// The app that sent it is gone; the record is still worth pointing at.
			out.Title = "Update on " + t.Reference
			return out, true, nil
		}
		actor := ""
		if n.ActorID.Valid {
			if actor, err = rd.name(ctx, n.ActorID.UUID); err != nil {
				return out, false, err
			}
		}
		out.Title, out.Body = tpl.Render(rd.values(t, actor))
		return out, true, nil
	}
	out.Title, out.Link = "Something changed", "/tickets"
	return out, true, nil
}

func (rd *Renderer) ticket(ctx context.Context, id uuid.UUID) (*records.Ticket, error) {
	if t, seen := rd.tickets[id]; seen {
		return t, nil
	}
	t, err := records.Load(ctx, rd.q, id)
	if errors.Is(err, pgx.ErrNoRows) {
		rd.tickets[id] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rd.tickets[id] = &t
	return &t, nil
}

func (rd *Renderer) name(ctx context.Context, id uuid.UUID) (string, error) {
	if n, seen := rd.names[id]; seen {
		return n, nil
	}
	u, err := rd.q.GetUser(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		u.Name, err = "Someone", nil
	}
	rd.names[id] = u.Name
	return u.Name, err
}

// values are what a template's placeholders read (apps.Placeholders).
func (rd *Renderer) values(t *records.Ticket, actor string) map[string]string {
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	return map[string]string{
		"record.reference": t.Reference, "record.title": t.Title, "record.type": t.Type,
		"record.status": rd.apps.Lifecycle(t.Type).Label(t.Status), "record.priority": strconv.Itoa(int(t.Priority)),
		"record.requester_name": t.Requester, "record.assignee_name": deref(t.Assignee),
		"record.assignment_group": deref(t.AssignmentGroup), "actor.name": actor,
	}
}
