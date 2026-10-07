package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
)

// maxNotifications bounds the list to what a person could plausibly read in one sitting; older ones stay in the
// table (nothing deletes them yet) but drop off the end.
const maxNotifications = 20

// notificationDTO is one hint that something needs this person's attention. The stored row names what happened and
// which record; the text is filled in now, from the record as this person may see it, so a notification never
// carries more than its recipient could read for themselves.
type notificationDTO struct {
	ID uuid.UUID `json:"id"`
	// Kind is approval_pending (a workflow asks this person to decide) or template (an app's notify rule).
	Kind       string     `json:"kind"`
	ApprovalID *uuid.UUID `json:"approval_id,omitempty"`
	// TaskID is the ticket a template notification is about.
	TaskID *uuid.UUID `json:"task_id,omitempty"`
	// Reference is that ticket's reference, such as INC42.
	Reference string `json:"reference,omitempty"`
	// Title is the line to show.
	Title string `json:"title"`
	// Body is more detail, when the template has any.
	Body string `json:"body,omitempty"`
	// Link is where the notification leads in the web app.
	Link      string     `json:"link"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
}

// renderer fills in notifications for one request, looking each ticket and person up once.
type renderer struct {
	h       *handlers
	q       *dbgen.Queries
	tickets map[uuid.UUID]*records.Ticket // nil: not visible to this person
	names   map[uuid.UUID]string
}

func (h *handlers) renderer(q *dbgen.Queries) *renderer {
	return &renderer{h: h, q: q, tickets: map[uuid.UUID]*records.Ticket{}, names: map[uuid.UUID]string{}}
}

// render returns the notification as its recipient sees it, and false for one about a ticket they can no longer
// see (it was filed when they could).
func (rd *renderer) render(ctx context.Context, n dbgen.Notification) (notificationDTO, bool, error) {
	out := notificationDTO{ID: n.ID, Kind: n.Kind, CreatedAt: n.CreatedAt.Time}
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
		tpl := rd.h.apps.Notification(n.App.String, n.Template.String)
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

func (rd *renderer) ticket(ctx context.Context, id uuid.UUID) (*records.Ticket, error) {
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

func (rd *renderer) name(ctx context.Context, id uuid.UUID) (string, error) {
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
func (rd *renderer) values(t *records.Ticket, actor string) map[string]string {
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	return map[string]string{
		"record.reference": t.Reference, "record.title": t.Title, "record.type": t.Type,
		"record.status": rd.h.lifecycleFor(t.Type).Label(t.Status), "record.priority": strconv.Itoa(int(t.Priority)),
		"record.requester_name": t.Requester, "record.assignee_name": deref(t.Assignee),
		"record.assignment_group": deref(t.AssignmentGroup), "actor.name": actor,
	}
}

// GET /notifications: the signed-in person's own, newest first, with unread_count. Anyone can have one (a workflow
// can ask anyone to approve something, and a requester hears about their own tickets), so this is not staff-only.
func (h *handlers) listMyNotifications(w http.ResponseWriter, r *http.Request) {
	out := []notificationDTO{}
	var unread int64
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		rows, err := q.ListMyNotifications(r.Context(), dbgen.ListMyNotificationsParams{UserID: p.UserID, Limit: maxNotifications})
		if err != nil {
			return err
		}
		rd := h.renderer(q)
		for _, row := range rows {
			n, ok, err := rd.render(r.Context(), row)
			if err != nil {
				return err
			}
			if ok {
				out = append(out, n)
			}
		}
		unread, err = q.CountMyUnreadNotifications(r.Context(), p.UserID)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "unread_count": unread})
}

// POST /notifications/{id}/read: only the recipient may mark their own read, and only once; RLS and the query's own
// WHERE both say so, so anyone else, or marking twice, looks like the notification does not exist.
func (h *handlers) markNotificationRead(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var out notificationDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		n, err := q.MarkNotificationRead(r.Context(), id)
		if err != nil {
			return err
		}
		out, _, err = h.renderer(q).render(r.Context(), n)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /notifications/read with {"task_id"}: mark everything this person was told about a ticket as read, which
// is what opening the ticket means. It answers with how many were marked.
func (h *handlers) markTaskNotificationsRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID uuid.UUID `json:"task_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	var marked int64
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		var err error
		marked, err = q.MarkTaskNotificationsRead(r.Context(), dbgen.MarkTaskNotificationsReadParams{UserID: p.UserID, TaskID: uuid.NullUUID{UUID: req.TaskID, Valid: true}})
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"marked": marked})
}
