package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// maxNotifications bounds the list to what a person could plausibly read in one sitting; older ones stay in the
// table (nothing deletes them yet) but drop off the end.
const maxNotifications = 20

// notificationDTO is one hint that something needs this person's attention. Like a realtime event it names what
// happened and lets the ids resolve through their own endpoint (here, /approvals): the row never carries ticket or
// approval content, so RLS on those stays the single authority.
type notificationDTO struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	ApprovalID *uuid.UUID `json:"approval_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ReadAt     *time.Time `json:"read_at,omitempty"`
}

func toNotification(n dbgen.Notification) notificationDTO {
	out := notificationDTO{ID: n.ID, Kind: n.Kind, CreatedAt: n.CreatedAt.Time}
	if n.ApprovalID.Valid {
		id := n.ApprovalID.UUID
		out.ApprovalID = &id
	}
	if n.ReadAt.Valid {
		out.ReadAt = &n.ReadAt.Time
	}
	return out
}

// GET /notifications: the signed-in person's own, newest first. Anyone can have one (a workflow can ask anyone to
// approve something), so this is not staff-only.
func (h *handlers) listMyNotifications(w http.ResponseWriter, r *http.Request) {
	out := []notificationDTO{}
	var unread int64
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		rows, err := q.ListMyNotifications(r.Context(), dbgen.ListMyNotificationsParams{UserID: p.UserID, Limit: maxNotifications})
		if err != nil {
			return err
		}
		for _, row := range rows {
			out = append(out, toNotification(row))
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
	var out dbgen.Notification
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		out, err = q.MarkNotificationRead(r.Context(), id)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, toNotification(out))
}
