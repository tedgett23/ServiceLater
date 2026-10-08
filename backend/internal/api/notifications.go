package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/notify"
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

func toNotification(m notify.Message) notificationDTO {
	return notificationDTO{ID: m.ID, Kind: m.Kind, ApprovalID: m.ApprovalID, TaskID: m.TaskID, Reference: m.Reference,
		Title: m.Title, Body: m.Body, Link: m.Link, CreatedAt: m.CreatedAt, ReadAt: m.ReadAt}
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
		rd := notify.NewRenderer(h.apps, q)
		for _, row := range rows {
			n, ok, err := rd.Render(r.Context(), row)
			if err != nil {
				return err
			}
			if ok {
				out = append(out, toNotification(n))
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
		m, _, err := notify.NewRenderer(h.apps, q).Render(r.Context(), n)
		out = toNotification(m)
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
