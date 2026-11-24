package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/query"
)

// SQL queries people write themselves (query:run; internal/query, docs/query.md): read-only, over the views in the
// query schema, each of which shows only what the person may see.

type runQueryRequest struct {
	// SQL is one SELECT over the query views.
	SQL string `json:"sql"`
	// Params are the values of $1, $2, ... in order, as text; Postgres converts them to the types the query needs.
	Params []string `json:"params"`
}

// queryProblem is why a query was not run or did not work.
type queryProblem struct {
	Error string `json:"error"`
	// Line and Column are where in the query the problem is, counting from 1; 0 when it is the query as a whole.
	Line   int `json:"line"`
	Column int `json:"column"`
	// Timeout is true when the query ran out of time.
	Timeout bool `json:"timeout"`
}

// QUERY /api/v1/query (query:run): runs a read-only SQL query over the query views as the caller. QUERY is the HTTP
// method for a read with a body (RFC 10008); POST does exactly the same, for clients and networks that do not pass
// QUERY. At most 5,000 rows come back (truncated says there were more) and a query may take 30 seconds; it stops if
// the client goes away. A query that is not allowed, or fails, is a 422 saying why and where; too many at once or in
// an hour is a 429.
func (h *handlers) runQuery(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var req runQueryRequest
	if !decode(w, r, &req) {
		return
	}
	if h.query == nil {
		writeError(w, http.StatusServiceUnavailable, "queries are not available on this server")
		return
	}
	res, err := h.query.Run(r.Context(), query.Request{User: p.UserID, KeyID: p.KeyID, Scopes: p.Scopes, SQL: req.SQL, Params: req.Params})
	if err != nil {
		h.writeQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeQueryError answers for a query (or a view being saved) that was not run or did not work: a 422 saying why and
// where, or a 429 for too many.
func (h *handlers) writeQueryError(w http.ResponseWriter, err error) {
	var refused *query.Refused
	var failed *query.Failed
	var busy *query.Busy
	switch {
	case errors.As(err, &refused):
		writeJSON(w, http.StatusUnprocessableEntity, queryProblem{Error: refused.Message, Line: refused.Line, Column: refused.Column})
	case errors.As(err, &failed):
		writeJSON(w, http.StatusUnprocessableEntity, queryProblem{Error: failed.Message, Line: failed.Line, Column: failed.Column, Timeout: failed.Timeout})
	case errors.As(err, &busy):
		writeError(w, http.StatusTooManyRequests, busy.Message)
	default:
		writeDBError(w, err)
	}
}

// querySchemaDTO is everything a query can read.
type querySchemaDTO struct {
	// Views are the built-in views, with what each column holds.
	Views []query.View `json:"views"`
	// SavedViews are the saved views the caller may read, read by name like the built-in ones.
	SavedViews []savedViewDTO `json:"saved_views"`
}

// GET /api/v1/query/schema (query:run): the views queries can read, with their columns and what each holds, and the
// saved views the caller may read.
func (h *handlers) querySchema(w http.ResponseWriter, r *http.Request) {
	if h.query == nil {
		writeError(w, http.StatusServiceUnavailable, "queries are not available on this server")
		return
	}
	views, err := h.query.Schema(r.Context())
	if err != nil {
		slog.Error("query schema", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	saved := []savedViewDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		rows, err := q.ListSavedViews(r.Context())
		for _, v := range rows {
			saved = append(saved, toSavedView(p, v.Name, v.Description, v.Sql, v.Shared, v.Columns, v.DependsOn, v.Chart, v.OwnerID,
				v.OwnerName, v.CreatedAt.Time, v.UpdatedAt.Time))
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, querySchemaDTO{Views: views, SavedViews: saved})
}

// queryRunDTO is one query the caller ran, or tried to.
type queryRunDTO struct {
	ID     uuid.UUID `json:"id"`
	SQL    string    `json:"sql"`
	Params []string  `json:"params"`
	// Outcome is ok, refused, failed or timeout; Error says why when it is not ok.
	Outcome    string    `json:"outcome"`
	Error      string    `json:"error"`
	RowCount   int       `json:"row_count"`
	Truncated  bool      `json:"truncated"`
	DurationMS int       `json:"duration_ms"`
	StartedAt  time.Time `json:"started_at"`
}

// GET /api/v1/query/runs (query:run): the caller's 50 most recent queries, newest first.
func (h *handlers) listQueryRuns(w http.ResponseWriter, r *http.Request) {
	out := []queryRunDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListMyQueryRuns(r.Context())
		if err != nil {
			return err
		}
		for _, x := range rows {
			d := queryRunDTO{ID: x.ID, SQL: x.Sql, Outcome: x.Outcome, Error: x.Error, RowCount: int(x.RowCount),
				Truncated: x.Truncated, DurationMS: int(x.DurationMs), StartedAt: x.StartedAt.Time, Params: []string{}}
			_ = json.Unmarshal(x.Params, &d.Params)
			out = append(out, d)
		}
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
