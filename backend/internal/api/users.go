package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

type staffDTO struct {
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
	Name  string    `json:"name"`
	Role  string    `json:"role"`
}

// GET /users/staff (staff): agents and admins, for assignee pickers.
func (h *handlers) listStaff(w http.ResponseWriter, r *http.Request) {
	var out []staffDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListStaff(r.Context())
		out = make([]staffDTO, len(rows))
		for i, u := range rows {
			out[i] = staffDTO{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role}
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
