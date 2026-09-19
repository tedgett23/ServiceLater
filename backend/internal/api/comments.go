package api

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

const maxComment = 10000

type commentDTO struct {
	ID        uuid.UUID `json:"id"`
	TicketID  uuid.UUID `json:"ticket_id"`
	AuthorID  uuid.UUID `json:"author_id"`
	Author    string    `json:"author_name"`
	Body      string    `json:"body"`
	Internal  bool      `json:"internal"`
	CreatedAt time.Time `json:"created_at"`
}

func toComment(c dbgen.TicketComment, authorName string) commentDTO {
	return commentDTO{ID: c.ID, TicketID: c.TicketID, AuthorID: c.AuthorID, Author: authorName, Body: c.Body,
		Internal: c.Internal, CreatedAt: c.CreatedAt.Time}
}

// GET /tickets/{id}/comments. Internal notes are filtered out by RLS for non-staff.
func (h *handlers) listComments(w http.ResponseWriter, r *http.Request) {
	var out []commentDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		t, err := loadTicket(r, q)
		if err != nil {
			return err
		}
		rows, err := q.ListComments(r.Context(), t.ID)
		out = make([]commentDTO, len(rows))
		for i, c := range rows {
			out[i] = toComment(c.TicketComment, c.AuthorName)
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// POST /tickets/{id}/comments with {"body": "...", "internal": false}. Only staff may post internal notes.
func (h *handlers) createComment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body     string `json:"body"`
		Internal bool   `json:"internal"`
	}
	if !decode(w, r, &req) {
		return
	}
	var out commentDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		if n := utf8.RuneCountInString(strings.TrimSpace(req.Body)); n < 1 || len(req.Body) > maxComment {
			return invalid("comment must be 1 to 10000 characters")
		}
		if req.Internal && !isStaff(p.Role) {
			return forbidden("only staff can post internal notes")
		}
		t, err := loadTicket(r, q)
		if err != nil {
			return err
		}
		c, err := q.CreateComment(r.Context(), dbgen.CreateCommentParams{
			TicketID: t.ID, AuthorID: p.UserID, Body: strings.TrimSpace(req.Body), Internal: req.Internal})
		if err != nil {
			return err
		}
		v, err := q.GetCommentView(r.Context(), c.ID)
		out = toComment(v.TicketComment, v.AuthorName)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusCreated, out)
}
