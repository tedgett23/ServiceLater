package api

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

const maxUserName = 200

// personDTO is someone in the user directory.
type personDTO struct {
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
	Name  string    `json:"name"`
	// Role is admin, agent or end_user.
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	// DeactivatedAt is set when an admin deactivated them: they cannot sign in, and pickers leave them out.
	DeactivatedAt *time.Time `json:"deactivated_at"`
}

func toPerson(u dbgen.User) personDTO {
	p := personDTO{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role, CreatedAt: u.CreatedAt.Time}
	if u.DeactivatedAt.Valid {
		p.DeactivatedAt = &u.DeactivatedAt.Time
	}
	return p
}

// groupRefDTO names a group someone is in.
type groupRefDTO struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// userDetailDTO is someone, the groups they are in, and the roles they hold on top of their base role.
type userDetailDTO struct {
	personDTO
	Groups []groupRefDTO `json:"groups"`
	Roles  []heldRoleDTO `json:"roles"`
}

// GET /users/staff (staff): active agents and admins, for assignee pickers.
func (h *handlers) listStaff(w http.ResponseWriter, r *http.Request) {
	var out []personDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListStaff(r.Context())
		out = make([]personDTO, len(rows))
		for i, u := range rows {
			out[i] = toPerson(u)
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// GET /users?q=&role=&include_deactivated=&limit=&offset= (staff): the directory, for pickers such as group
// membership and the admin's user list. q matches a name or an email; deactivated people are left out unless
// include_deactivated=true.
func (h *handlers) listUsers(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	limit, offset := defaultLimit, 0
	if !intParam(w, qs.Get("limit"), "limit", 1, maxLimit, &limit) || !intParam(w, qs.Get("offset"), "offset", 0, 1<<30, &offset) {
		return
	}
	args := dbgen.ListUsersParams{Limit: int32(limit), Offset: int32(offset), IncludeDeactivated: qs.Get("include_deactivated") == "true"}
	if s := strings.TrimSpace(qs.Get("q")); s != "" {
		args.Q = pgtype.Text{String: likeEscape(s), Valid: true}
	}
	if role := qs.Get("role"); role != "" {
		if !validRole(role) {
			writeError(w, http.StatusBadRequest, "role must be admin, agent or end_user")
			return
		}
		args.Role = pgtype.Text{String: role, Valid: true}
	}
	out := []personDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListUsers(r.Context(), args)
		for _, u := range rows {
			out = append(out, toPerson(u))
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func validRole(s string) bool { return s == "admin" || s == "agent" || s == "end_user" }

func cleanEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if at := strings.IndexByte(s, '@'); at < 1 || at == len(s)-1 || len(s) > 254 || strings.ContainsAny(s, " \t\r\n") {
		return "", invalid("email must be an email address")
	}
	return s, nil
}

func cleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < 1 || n > maxUserName {
		return "", invalid("name must be 1 to 200 characters")
	}
	return s, nil
}

func hashPassword(pw string) (string, error) {
	hash, err := auth.HashPassword(pw)
	if errors.Is(err, auth.ErrPasswordPolicy) {
		return "", invalid(err.Error())
	}
	return hash, err
}

// notAnApp refuses changes to an app's principal: it is managed by installing and removing its app.
func notAnApp(q *dbgen.Queries, r *http.Request, id uuid.UUID) error {
	u, err := q.GetUser(r.Context(), id)
	if err != nil {
		return err
	}
	if u.AppName.Valid {
		return invalid("this is the " + u.AppName.String + " app's identity, which installing and removing the app manages")
	}
	return nil
}

// userConflict turns the database's refusals of a user change into answers an admin can act on.
func userConflict(err error) error {
	var pe *pgconn.PgError
	switch {
	case !errors.As(err, &pe):
		return err
	case pe.Code == "23505":
		return &apiError{http.StatusConflict, "someone already has that email"}
	case pe.ConstraintName == "users_last_admin":
		return &apiError{http.StatusConflict, "there must be at least one active admin"}
	}
	return err
}

// POST /users (admin) with {"email", "name", "role", "password"}: add someone who can sign in straight away.
func (h *handlers) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Name  string `json:"name"`
		// Role is admin, agent or end_user; end_user if left out.
		Role string `json:"role"`
		// Password is 8 to 72 bytes. Share it with them some other way; there is no email yet.
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Role == "" {
		req.Role = "end_user"
	}
	email, err := cleanEmail(req.Email)
	if err == nil {
		req.Name, err = cleanName(req.Name)
	}
	if err == nil && !validRole(req.Role) {
		err = invalid("role must be admin, agent or end_user")
	}
	var hash string
	if err == nil {
		hash, err = hashPassword(req.Password)
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	var out personDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		if req.Role == "admin" && !p.Can(authz.RolesManage) {
			return forbidden("making someone an admin needs the roles:manage permission")
		}
		u, err := q.CreateUser(r.Context(), dbgen.CreateUserParams{Email: email, Name: req.Name, Role: req.Role})
		if err != nil {
			return userConflict(err)
		}
		out = toPerson(u)
		return q.SetUserPassword(r.Context(), dbgen.SetUserPasswordParams{UserID: u.ID, PasswordHash: hash})
	}) {
		return
	}
	w.Header().Set("Location", "/api/v1/users/"+out.ID.String())
	writeJSON(w, http.StatusCreated, out)
}

// GET /users/{id} (admin): someone, and the groups they are in.
func (h *handlers) getUser(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var out userDetailDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		u, err := q.GetUser(r.Context(), id)
		if err != nil {
			return err
		}
		out.personDTO = toPerson(u)
		if out.Groups, err = userGroups(r, q, id); err != nil {
			return err
		}
		out.Roles, err = userRoles(r, q, id)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func userGroups(r *http.Request, q *dbgen.Queries, id uuid.UUID) ([]groupRefDTO, error) {
	rows, err := q.ListUserGroups(r.Context(), id)
	out := make([]groupRefDTO, len(rows))
	for i, g := range rows {
		out[i] = groupRefDTO{ID: g.ID, Name: g.Name}
	}
	return out, err
}

// PATCH /users/{id} (admin) with {"email"?, "name"?, "role"?, "deactivated"?}. Deactivating signs them out
// everywhere and stops them signing in; their tickets and comments keep their name. Admins cannot demote or
// deactivate themselves, and there is always at least one active admin.
func (h *handlers) updateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Email *string `json:"email"`
		Name  *string `json:"name"`
		Role  *string `json:"role"`
		// Deactivated true deactivates them, false lets them sign in again.
		Deactivated *bool `json:"deactivated"`
	}
	if !decode(w, r, &req) {
		return
	}
	var out personDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		if err := notAnApp(q, r, id); err != nil {
			return err
		}
		args := dbgen.UpdateUserParams{ID: id}
		if req.Email != nil {
			email, err := cleanEmail(*req.Email)
			if err != nil {
				return err
			}
			args.Email = pgtype.Text{String: email, Valid: true}
		}
		if req.Name != nil {
			name, err := cleanName(*req.Name)
			if err != nil {
				return err
			}
			args.Name = pgtype.Text{String: name, Valid: true}
		}
		if req.Role != nil {
			if !validRole(*req.Role) {
				return invalid("role must be admin, agent or end_user")
			}
			if id == p.UserID && *req.Role != "admin" {
				return invalid("you cannot take away your own admin role")
			}
			if !p.Can(authz.RolesManage) {
				cur, err := q.GetUser(r.Context(), id)
				if err != nil {
					return err
				}
				if (*req.Role == "admin") != (cur.Role == "admin") {
					return forbidden("making someone an admin, or taking it away, needs the roles:manage permission")
				}
			}
			args.Role = pgtype.Text{String: *req.Role, Valid: true}
		}
		if req.Deactivated != nil {
			if id == p.UserID && *req.Deactivated {
				return invalid("you cannot deactivate yourself")
			}
			args.Deactivated = pgtype.Bool{Bool: *req.Deactivated, Valid: true}
		}
		u, err := q.UpdateUser(r.Context(), args)
		if err != nil {
			return userConflict(err)
		}
		out = toPerson(u)
		if u.DeactivatedAt.Valid {
			// Their refresh tokens go now; an access token they hold lapses within its short lifetime, and
			// meanwhile RLS no longer counts them as staff or admin.
			return q.RevokeUserSessions(r.Context(), id)
		}
		return nil
	}) {
		return
	}
	h.perms.Forget() // anyone's permissions may have changed
	writeJSON(w, http.StatusOK, out)
}

// PUT /users/{id}/password (admin) with {"password"}: set a new password. It signs them out everywhere (an admin
// setting their own keeps their sessions).
func (h *handlers) setUserPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		// Password is 8 to 72 bytes.
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	hash, err := hashPassword(req.Password)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		if err := notAnApp(q, r, id); err != nil {
			return err
		}
		if err := q.SetUserPassword(r.Context(), dbgen.SetUserPasswordParams{UserID: id, PasswordHash: hash}); err != nil || id == p.UserID {
			return err
		}
		return q.RevokeUserSessions(r.Context(), id)
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /users/{id} (admin): remove someone who has no history. Anyone who has filed, worked on or commented on a
// ticket, or been asked to approve something, is deactivated instead, so the record of what they did stays whole.
func (h *handlers) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		if id == p.UserID {
			return invalid("you cannot delete yourself")
		}
		if err := notAnApp(q, r, id); err != nil {
			return err
		}
		n, err := q.DeleteUser(r.Context(), id)
		var pe *pgconn.PgError
		switch {
		case errors.As(err, &pe) && pe.Code == "23503":
			return &apiError{http.StatusConflict, "this person has tickets, comments or approvals; deactivate them instead"}
		case err != nil:
			return userConflict(err)
		case n == 0:
			return notFound()
		}
		return nil
	}) {
		return
	}
	h.perms.Forget() // anyone's permissions may have changed
	w.WriteHeader(http.StatusNoContent)
}
