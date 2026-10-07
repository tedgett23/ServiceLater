package api

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// roleDTO is a named set of permissions: a built-in base role, or one an app defines in roles/.
type roleDTO struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
	// App is the app that defines the role; empty for the built-in base roles.
	App string `json:"app"`
	// BuiltIn roles are base roles: set on a person, never granted.
	BuiltIn bool `json:"built_in"`
	// Grants is how many people and groups the role is granted to.
	Grants int64 `json:"grants"`
}

// grantDTO is a role given to a person or to a group (everyone in it).
type grantDTO struct {
	ID uuid.UUID `json:"id"`
	// UserID is set for a grant to a person, GroupID for one to a group.
	UserID    *uuid.UUID `json:"user_id,omitempty"`
	UserName  string     `json:"user_name,omitempty"`
	UserEmail string     `json:"user_email,omitempty"`
	GroupID   *uuid.UUID `json:"group_id,omitempty"`
	GroupName string     `json:"group_name,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// roleDetailDTO is a role and who has it.
type roleDetailDTO struct {
	roleDTO
	GrantList []grantDTO `json:"granted_to"`
}

// heldRoleDTO is a role someone or a group holds, and through what.
type heldRoleDTO struct {
	// GrantID is the grant to revoke to take it away.
	GrantID uuid.UUID `json:"grant_id"`
	Role    string    `json:"role"`
	Label   string    `json:"label"`
	// GroupID and GroupName say which group a person holds it through; empty when it was granted to them.
	GroupID   *uuid.UUID `json:"group_id,omitempty"`
	GroupName string     `json:"group_name,omitempty"`
}

func nullID(id uuid.NullUUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	v := id.UUID
	return &v
}

// GET /permissions: every permission the platform checks, for showing what a role allows.
func (h *handlers) listPermissions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": authz.Catalog})
}

// GET /roles: every role, the built-in base roles first.
func (h *handlers) listRoles(w http.ResponseWriter, r *http.Request) {
	out := []roleDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListRoles(r.Context())
		for _, x := range rows {
			out = append(out, roleDTO{Name: x.Name, Label: x.Label, Description: x.Description, Permissions: x.Permissions,
				App: x.AppName.String, BuiltIn: slices.Contains(apps.BuiltInRoles, x.Name), Grants: x.Grants})
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// GET /roles/{name}: a role and everyone it is granted to.
func (h *handlers) getRole(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var out roleDetailDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		x, err := q.GetRole(r.Context(), name)
		if err != nil {
			return err
		}
		out.roleDTO = roleDTO{Name: x.Name, Label: x.Label, Description: x.Description, Permissions: x.Permissions,
			App: x.AppName.String, BuiltIn: slices.Contains(apps.BuiltInRoles, x.Name)}
		grants, err := q.ListRoleGrants(r.Context(), name)
		out.GrantList = make([]grantDTO, len(grants))
		for i, g := range grants {
			out.GrantList[i] = grantDTO{ID: g.ID, UserID: nullID(g.UserID), UserName: g.UserName.String, UserEmail: g.UserEmail.String,
				GroupID: nullID(g.GroupID), GroupName: g.GroupName.String, CreatedAt: g.CreatedAt.Time}
		}
		out.Grants = int64(len(grants))
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /roles/{name}/grants with {"user_id"} or {"group_id"}: give the role to a person, or to everyone in a group.
// Base roles (admin, agent, end_user) are set on the person instead.
func (h *handlers) grantRole(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var req struct {
		UserID  *uuid.UUID `json:"user_id"`
		GroupID *uuid.UUID `json:"group_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	var out grantDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if (req.UserID == nil) == (req.GroupID == nil) {
			return invalid("give either user_id or group_id")
		}
		if slices.Contains(apps.BuiltInRoles, name) {
			return invalid("a base role is set on the person, not granted")
		}
		if _, err := q.GetRole(r.Context(), name); err != nil {
			return err
		}
		args := dbgen.GrantRoleParams{Role: name}
		if req.UserID != nil {
			u, err := q.GetUser(r.Context(), *req.UserID)
			if err != nil {
				return invalid("that person does not exist")
			}
			if u.AppName.Valid {
				return invalid("an app's identity holds what its manifest declares, not roles")
			}
			args.UserID = uuid.NullUUID{UUID: u.ID, Valid: true}
			out.UserID, out.UserName, out.UserEmail = req.UserID, u.Name, u.Email
		} else {
			g, err := q.GetGroup(r.Context(), *req.GroupID)
			if err != nil {
				return invalid("that group does not exist")
			}
			args.GroupID = uuid.NullUUID{UUID: g.ID, Valid: true}
			out.GroupID, out.GroupName = req.GroupID, g.Name
		}
		row, err := q.GrantRole(r.Context(), args)
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return &apiError{http.StatusConflict, "they already have this role"}
		}
		out.ID, out.CreatedAt = row.ID, row.CreatedAt.Time
		return err
	}) {
		return
	}
	h.perms.Forget() // anyone's permissions may have changed
	writeJSON(w, http.StatusCreated, out)
}

// DELETE /roles/{name}/grants/{id}: take a role back from the person or group it was granted to.
func (h *handlers) revokeRole(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		n, err := q.RevokeRoleGrant(r.Context(), dbgen.RevokeRoleGrantParams{ID: id, Role: chi.URLParam(r, "name")})
		if err == nil && n == 0 {
			return notFound()
		}
		return err
	}) {
		return
	}
	h.perms.Forget() // anyone's permissions may have changed
	w.WriteHeader(http.StatusNoContent)
}

func userRoles(r *http.Request, q *dbgen.Queries, id uuid.UUID) ([]heldRoleDTO, error) {
	rows, err := q.ListUserRoles(r.Context(), uuid.NullUUID{UUID: id, Valid: true})
	out := make([]heldRoleDTO, len(rows))
	for i, x := range rows {
		out[i] = heldRoleDTO{GrantID: x.ID, Role: x.Role, Label: x.Label, GroupID: nullID(x.GroupID), GroupName: x.GroupName.String}
	}
	return out, err
}

func groupRoles(r *http.Request, q *dbgen.Queries, id uuid.UUID) ([]heldRoleDTO, error) {
	rows, err := q.ListGroupRoles(r.Context(), uuid.NullUUID{UUID: id, Valid: true})
	out := make([]heldRoleDTO, len(rows))
	for i, x := range rows {
		out[i] = heldRoleDTO{GrantID: x.ID, Role: x.Role, Label: x.Label}
	}
	return out, err
}
