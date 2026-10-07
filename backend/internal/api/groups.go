package api

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

const (
	maxGroupName        = 100
	maxGroupDescription = 1000
)

// groupDTO is a team: what a ticket is routed to, and what group('name') in a workflow resolves to.
type groupDTO struct {
	ID uuid.UUID `json:"id"`
	// Name is unique whatever its case; expressions refer to a group by it.
	Name        string    `json:"name"`
	Description string    `json:"description"`
	MemberCount int64     `json:"member_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// memberDTO is one person in a group.
type memberDTO struct {
	ID      uuid.UUID `json:"id"`
	Email   string    `json:"email"`
	Name    string    `json:"name"`
	Role    string    `json:"role"`
	AddedAt time.Time `json:"added_at"`
	// DeactivatedAt is set for someone who can no longer sign in. They stay listed, but group() leaves them out.
	DeactivatedAt *time.Time `json:"deactivated_at"`
}

// groupDetailDTO is a group with everyone in it.
type groupDetailDTO struct {
	groupDTO
	Members []memberDTO `json:"members"`
}

func toGroup(g dbgen.Group, members int64) groupDTO {
	return groupDTO{ID: g.ID, Name: g.Name, Description: g.Description, MemberCount: members, CreatedAt: g.CreatedAt.Time, UpdatedAt: g.UpdatedAt.Time}
}

func validGroupName(s string) error {
	if n := utf8.RuneCountInString(s); n < 1 || n > maxGroupName {
		return invalid("name must be 1 to 100 characters")
	}
	return nil
}

// groupConflict turns a clash on the unique name into a 409 the person can act on.
func groupConflict(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return &apiError{http.StatusConflict, "a group with that name already exists"}
	}
	return err
}

// urlUUID reads a path parameter that must be an id. Anything else cannot name a row, so it is a 404 like a missing one.
func urlUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return uuid.Nil, false
	}
	return id, true
}

// GET /groups: every group, by name. Anyone signed in may read them, for pickers and to see who a ticket is with.
func (h *handlers) listGroups(w http.ResponseWriter, r *http.Request) {
	out := []groupDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListGroups(r.Context())
		for _, g := range rows {
			out = append(out, toGroup(dbgen.Group{ID: g.ID, Name: g.Name, Description: g.Description, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}, g.MemberCount))
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// POST /groups (admin) with {"name": "...", "description": "..."}.
func (h *handlers) createGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decode(w, r, &req) {
		return
	}
	var out groupDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		name := strings.TrimSpace(req.Name)
		if err := validGroupName(name); err != nil {
			return err
		}
		if len(req.Description) > maxGroupDescription {
			return invalid("description must be at most 1000 characters")
		}
		g, err := q.CreateGroup(r.Context(), dbgen.CreateGroupParams{Name: name, Description: req.Description})
		out = toGroup(g, 0)
		return groupConflict(err)
	}) {
		return
	}
	w.Header().Set("Location", "/api/v1/groups/"+out.ID.String())
	writeJSON(w, http.StatusCreated, out)
}

// GET /groups/{id}: a group and its members.
func (h *handlers) getGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var out groupDetailDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		g, err := q.GetGroup(r.Context(), id)
		if err != nil {
			return err
		}
		out.groupDTO = toGroup(dbgen.Group{ID: g.ID, Name: g.Name, Description: g.Description, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}, g.MemberCount)
		out.Members, err = groupMembers(r, q, id)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func groupMembers(r *http.Request, q *dbgen.Queries, id uuid.UUID) ([]memberDTO, error) {
	rows, err := q.ListGroupMembers(r.Context(), id)
	out := make([]memberDTO, len(rows))
	for i, m := range rows {
		out[i] = memberDTO{ID: m.ID, Email: m.Email, Name: m.Name, Role: m.Role, AddedAt: m.AddedAt.Time}
		if m.DeactivatedAt.Valid {
			out[i].DeactivatedAt = &m.DeactivatedAt.Time
		}
	}
	return out, err
}

// PATCH /groups/{id} (admin) with {"name"?, "description"?}. Renaming a group breaks any expression that names it.
func (h *handlers) updateGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if !decode(w, r, &req) {
		return
	}
	var out groupDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		args := dbgen.UpdateGroupParams{ID: id}
		if req.Name != nil {
			name := strings.TrimSpace(*req.Name)
			if err := validGroupName(name); err != nil {
				return err
			}
			args.Name = pgtype.Text{String: name, Valid: true}
		}
		if req.Description != nil {
			if len(*req.Description) > maxGroupDescription {
				return invalid("description must be at most 1000 characters")
			}
			args.Description = pgtype.Text{String: *req.Description, Valid: true}
		}
		g, err := q.UpdateGroup(r.Context(), args)
		if err != nil {
			return groupConflict(err)
		}
		cur, err := q.GetGroup(r.Context(), g.ID)
		out = toGroup(g, cur.MemberCount)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /groups/{id} (admin). A group that tickets are assigned to cannot be deleted: move them first.
func (h *handlers) deleteGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		n, err := q.DeleteGroup(r.Context(), id)
		var pe *pgconn.PgError
		switch {
		case errors.As(err, &pe) && pe.Code == "23503":
			return &apiError{http.StatusConflict, "tickets are assigned to this group; move them to another group first"}
		case err != nil:
			return err
		case n == 0:
			return notFound() // missing, or not an admin: RLS hides the row from the delete
		}
		return nil
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /groups/{id}/members: who is in a group.
func (h *handlers) listGroupMembers(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var out []memberDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if _, err := q.GetGroup(r.Context(), id); err != nil {
			return err
		}
		var err error
		out, err = groupMembers(r, q, id)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// PUT /groups/{id}/members/{userId} (admin): add someone. Adding someone already in the group changes nothing.
func (h *handlers) addGroupMember(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	userID, ok := urlUUID(w, r, "userId")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if _, err := q.GetGroup(r.Context(), id); err != nil {
			return err
		}
		if _, err := q.GetUser(r.Context(), userID); err != nil {
			return err
		}
		return q.AddGroupMember(r.Context(), dbgen.AddGroupMemberParams{GroupID: id, UserID: userID})
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /groups/{id}/members/{userId} (admin): take someone out. Tickets already assigned to them keep their
// assignee; the next change of group is what lets go of them.
func (h *handlers) removeGroupMember(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	userID, ok := urlUUID(w, r, "userId")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		n, err := q.RemoveGroupMember(r.Context(), dbgen.RemoveGroupMemberParams{GroupID: id, UserID: userID})
		if err == nil && n == 0 {
			return notFound()
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
