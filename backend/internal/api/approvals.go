package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// maxApprovalComment bounds an approval decision's comment, the same as a task comment's limit in spirit but a
// decision's reason is normally short.
const maxApprovalComment = 2000

// approvalDTO is one pending approval request, with enough of the ticket to show in a list. Anyone can be asked to
// approve something, staff or not, so this is not a staff-only endpoint.
type approvalDTO struct {
	ID          uuid.UUID `json:"id"`
	ExecutionID uuid.UUID `json:"execution_id"`
	StepID      string    `json:"step_id"`
	Workflow    string    `json:"workflow"`
	Reference   string    `json:"reference,omitempty"`
	Title       string    `json:"title,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// GET /approvals: the signed-in person's own pending requests, oldest first.
func (h *handlers) listMyApprovals(w http.ResponseWriter, r *http.Request) {
	out := []approvalDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		rows, err := q.ListMyPendingApprovals(r.Context(), p.UserID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			out = append(out, approvalDTO{ID: row.ID, ExecutionID: row.ExecutionID, StepID: row.StepID, Workflow: row.App + "/" + row.Workflow,
				Reference: row.Reference, Title: row.Title, CreatedAt: row.CreatedAt.Time})
		}
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// POST /approvals/{id}/decide {"decision": "approved"|"rejected", "comment": "..."}. Only the approver this request
// was sent to may answer it, and only once: RLS and the query's own WHERE both say so, so anyone else, or answering
// twice, looks like the request does not exist.
func (h *handlers) decideApproval(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var req struct {
		Decision string `json:"decision"`
		Comment  string `json:"comment"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Decision != "approved" && req.Decision != "rejected" {
		writeError(w, http.StatusUnprocessableEntity, `decision must be "approved" or "rejected"`)
		return
	}
	comment := strings.TrimSpace(req.Comment)
	if len(comment) > maxApprovalComment {
		writeError(w, http.StatusUnprocessableEntity, "the comment is too long")
		return
	}
	var out dbgen.WorkflowApproval
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		out, err = q.DecideApproval(r.Context(), dbgen.DecideApprovalParams{ID: id, Decision: pgtype.Text{String: req.Decision, Valid: true}, Comment: comment})
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": out.ID, "decision": out.Decision.String, "comment": out.Comment, "decided_at": out.DecidedAt.Time})
}
