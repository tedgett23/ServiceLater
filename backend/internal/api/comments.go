package api

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
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
	// OnBehalfOf names the person an app acted for, when an app wrote the comment (author_name is then the app).
	OnBehalfOf string `json:"on_behalf_of_name,omitempty"`
	// Mentions are the people the comment @mentions; the body names each as @ and their name.
	Mentions []mentionDTO `json:"mentions"`
}

// mentionDTO is a person a comment mentions.
type mentionDTO struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

func toComment(c dbgen.TaskComment, authorName string, behalf pgtype.Text) commentDTO {
	return commentDTO{ID: c.ID, TicketID: c.TaskID, AuthorID: c.AuthorID, Author: authorName, Body: c.Body,
		Internal: c.Internal, CreatedAt: c.CreatedAt.Time, OnBehalfOf: behalf.String, Mentions: []mentionDTO{}}
}

// withMentions fills in the people each comment mentions.
func withMentions(r *http.Request, q *dbgen.Queries, task uuid.UUID, out []commentDTO) error {
	rows, err := q.ListCommentMentions(r.Context(), task)
	if err != nil {
		return err
	}
	at := make(map[uuid.UUID]int, len(out))
	for i, c := range out {
		at[c.ID] = i
	}
	for _, m := range rows {
		if i, ok := at[m.CommentID]; ok {
			out[i].Mentions = append(out[i].Mentions, mentionDTO{ID: m.UserID, Name: m.Name})
		}
	}
	return nil
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
		if err != nil {
			return err
		}
		out = make([]commentDTO, len(rows))
		for i, c := range rows {
			out[i] = toComment(c.TaskComment, c.AuthorName, c.OnBehalfOfName)
		}
		return withMentions(r, q, t.ID, out)
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// POST /tickets/{id}/comments with {"body": "...", "internal": false, "mentions": [user ids]}. Only staff may post
// internal notes. Each person mentioned must be able to read the comment (see the record; for an internal note, hold
// comments:internal), and the body names them as @ and their name. The rules on: [comment] then tell people about it
// (core-itsm tells the people mentioned, the assignee and, for a public comment, the requester).
func (h *handlers) createComment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body     string      `json:"body"`
		Internal bool        `json:"internal"`
		Mentions []uuid.UUID `json:"mentions"`
	}
	if !decode(w, r, &req) {
		return
	}
	var out commentDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		if n := utf8.RuneCountInString(strings.TrimSpace(req.Body)); n < 1 || len(req.Body) > maxComment {
			return invalid("comment must be 1 to 10000 characters")
		}
		if req.Internal && !p.Can(authz.CommentsInternal) {
			return forbidden("internal notes need the comments:internal permission")
		}
		t, err := loadTicket(r, q)
		if err != nil {
			return err
		}
		c, err := h.svc.Comment(r.Context(), q, actorOf(p), ruleruns.Cause{}, t.ID,
			records.CommentInput{Body: req.Body, Internal: req.Internal, Mentions: req.Mentions})
		if err != nil {
			return err
		}
		v, err := q.GetCommentView(r.Context(), c.ID)
		if err != nil {
			return err
		}
		out = toComment(v.TaskComment, v.AuthorName, v.OnBehalfOfName)
		list := []commentDTO{out}
		err = withMentions(r, q, t.ID, list)
		out = list[0]
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// GET /tickets/{id}/mentionable?q=&internal= (users:read): up to six people whose name or email contains q and who
// may read a comment on this record (for an internal note, those with comments:internal): whom the comment box
// offers when someone types @.
func (h *handlers) listMentionable(w http.ResponseWriter, r *http.Request) {
	out := []mentionCandidateDTO{}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	internal := r.URL.Query().Get("internal") == "true"
	if !h.tx(w, r, func(qs *dbgen.Queries, _ auth.Principal) error {
		t, err := loadTicket(r, qs)
		if err != nil || q == "" {
			return err
		}
		rows, err := qs.MentionCandidates(r.Context(), dbgen.MentionCandidatesParams{TaskID: t.ID, Q: likeEscape(q), Internal: internal})
		for _, u := range rows {
			out = append(out, mentionCandidateDTO{ID: u.ID, Name: u.Name, Email: u.Email})
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// mentionCandidateDTO is someone a comment may mention.
type mentionCandidateDTO struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Email string    `json:"email"`
}
