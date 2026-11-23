package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/query"
)

// Saved views (query:run; docs/query.md "Saved views"): queries saved under a name, read by other queries like a
// table and by GET /views/{name}/rows. Whoever reads one sees their own rows: a view is SQL, not a copy of data.

// savedViewDTO is a saved view.
type savedViewDTO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// SQL is its query: one SELECT over the query views and other saved views, with no parameters.
	SQL string `json:"sql"`
	// Shared views can be read by everyone who may run queries; the rest only by their owner.
	Shared bool `json:"shared"`
	// Columns are what it gives, worked out when it was saved.
	Columns []query.Column `json:"columns"`
	// Uses are the saved views it reads directly.
	Uses []string `json:"uses"`
	// Mine is true when the caller owns it, and so may change it.
	Mine      bool      `json:"mine"`
	OwnerName string    `json:"owner_name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toSavedView(p auth.Principal, name, description, sql string, shared bool, columns []byte, uses []string,
	owner uuid.UUID, ownerName string, created, updated time.Time) savedViewDTO {
	d := savedViewDTO{Name: name, Description: description, SQL: sql, Shared: shared, Columns: []query.Column{}, Uses: uses,
		Mine: owner == p.UserID, OwnerName: ownerName, CreatedAt: created, UpdatedAt: updated}
	_ = json.Unmarshal(columns, &d.Columns)
	if d.Uses == nil {
		d.Uses = []string{}
	}
	return d
}

// GET /api/v1/views (query:run): the saved views the caller may read, their own and the shared ones, by name.
func (h *handlers) listSavedViews(w http.ResponseWriter, r *http.Request) {
	out := []savedViewDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		rows, err := q.ListSavedViews(r.Context())
		for _, v := range rows {
			out = append(out, toSavedView(p, v.Name, v.Description, v.Sql, v.Shared, v.Columns, v.DependsOn, v.OwnerID,
				v.OwnerName, v.CreatedAt.Time, v.UpdatedAt.Time))
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// GET /api/v1/views/{name} (query:run): one saved view, with its SQL.
func (h *handlers) getSavedView(w http.ResponseWriter, r *http.Request) {
	var out savedViewDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		v, err := q.GetSavedView(r.Context(), chi.URLParam(r, "name"))
		if err != nil {
			return err
		}
		out = toSavedView(p, v.Name, v.Description, v.Sql, v.Shared, v.Columns, v.DependsOn, v.OwnerID, v.OwnerName,
			v.CreatedAt.Time, v.UpdatedAt.Time)
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type putSavedViewRequest struct {
	// SQL is one SELECT over the query views and other saved views, with no parameters ($1...).
	SQL         string `json:"sql"`
	Description string `json:"description"`
	// Shared makes it readable by everyone who may run queries (each still sees only their own rows).
	Shared bool `json:"shared"`
}

// PUT /api/v1/views/{name} (query:run): saves a view, new (201) or the caller's own changed (200). The SQL is checked
// and run once with no rows, as the caller, to learn its columns; a problem is a 422 saying why and where, as for a
// query. A name someone else has is a 409. A view other views read may not change its columns or, while a shared
// view reads it, become private (409, naming those the caller can see).
func (h *handlers) putSavedView(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var req putSavedViewRequest
	if !decode(w, r, &req) {
		return
	}
	if h.query == nil {
		writeError(w, http.StatusServiceUnavailable, "queries are not available on this server")
		return
	}
	if len(req.Description) > 2000 {
		writeError(w, http.StatusUnprocessableEntity, "a description may be at most 2,000 characters")
		return
	}
	name := chi.URLParam(r, "name")
	checked, err := h.query.CheckView(r.Context(), query.ViewRequest{User: p.UserID, Scopes: p.Scopes, Name: name, SQL: req.SQL, Shared: req.Shared})
	if err != nil {
		h.writeQueryError(w, err)
		return
	}
	columns, err := json.Marshal(checked.Columns)
	if err != nil {
		writeDBError(w, err)
		return
	}
	uses := checked.Uses
	if uses == nil {
		uses = []string{}
	}
	created := false
	var out savedViewDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		if err := q.LockSavedViews(r.Context()); err != nil {
			return err
		}
		old, err := q.GetSavedView(r.Context(), name)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			created = true
			err = q.InsertSavedView(r.Context(), dbgen.InsertSavedViewParams{Name: name, Description: req.Description,
				Sql: checked.SQL, Shared: req.Shared, Columns: columns, DependsOn: uses})
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23505" {
				return &apiError{http.StatusConflict, fmt.Sprintf("someone already has a view named %s: choose another name", name)}
			}
			if err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if old.OwnerID != p.UserID {
				return forbidden(fmt.Sprintf("%s is %s's view: only they may change it (save your own under another name)", name, old.OwnerName))
			}
			if err := h.mayChangeView(r, q, name, req.Shared, old.Columns, columns); err != nil {
				return err
			}
			if _, err := q.UpdateSavedView(r.Context(), dbgen.UpdateSavedViewParams{Name: name, Description: req.Description,
				Sql: checked.SQL, Shared: req.Shared, Columns: columns, DependsOn: uses}); err != nil {
				return err
			}
		}
		v, err := q.GetSavedView(r.Context(), name)
		if err != nil {
			return err
		}
		out = toSavedView(p, v.Name, v.Description, v.Sql, v.Shared, v.Columns, v.DependsOn, v.OwnerID, v.OwnerName,
			v.CreatedAt.Time, v.UpdatedAt.Time)
		return nil
	}) {
		return
	}
	if created {
		writeJSON(w, http.StatusCreated, out)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// mayChangeView refuses a change to a view that would break the views reading it: other columns, or private while a
// shared one reads it.
func (h *handlers) mayChangeView(r *http.Request, q *dbgen.Queries, name string, shared bool, oldColumns, newColumns []byte) error {
	deps, err := q.SavedViewDependents(r.Context(), name)
	if err != nil || len(deps) == 0 {
		return err
	}
	var a, b []query.Column
	_ = json.Unmarshal(oldColumns, &a)
	_ = json.Unmarshal(newColumns, &b)
	if !slices.Equal(a, b) {
		return &apiError{http.StatusConflict, "other views read this one, so its columns cannot change: " + dependentNames(deps, false)}
	}
	if !shared && slices.ContainsFunc(deps, func(d dbgen.SavedViewDependentsRow) bool { return d.Shared }) {
		return &apiError{http.StatusConflict, "shared views read this one, so it must stay shared: " + dependentNames(deps, true)}
	}
	return nil
}

// dependentNames lists the views the caller can see by name and counts the rest.
func dependentNames(deps []dbgen.SavedViewDependentsRow, sharedOnly bool) string {
	var named []string
	hidden := 0
	for _, d := range deps {
		switch {
		case sharedOnly && !d.Shared:
		case d.Visible:
			named = append(named, d.Name)
		default:
			hidden++
		}
	}
	s := strings.Join(named, ", ")
	if hidden > 0 {
		if s != "" {
			s += ", and "
		}
		s += fmt.Sprintf("%d private to someone else", hidden)
	}
	return s
}

// DELETE /api/v1/views/{name} (query:run): removes a view, the caller's own (anyone's with settings:manage). One that
// other views read cannot be removed (409).
func (h *handlers) deleteSavedView(w http.ResponseWriter, r *http.Request) {
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		name := chi.URLParam(r, "name")
		if err := q.LockSavedViews(r.Context()); err != nil {
			return err
		}
		v, err := q.GetSavedView(r.Context(), name)
		if err != nil {
			return err
		}
		if v.OwnerID != p.UserID && !p.Can(authz.SettingsManage) {
			return forbidden(fmt.Sprintf("%s is %s's view: only they may remove it", name, v.OwnerName))
		}
		deps, err := q.SavedViewDependents(r.Context(), name)
		if err != nil {
			return err
		}
		if len(deps) > 0 {
			return &apiError{http.StatusConflict, "other views read this one, so it cannot be removed: " + dependentNames(deps, false)}
		}
		n, err := q.DeleteSavedView(r.Context(), name)
		if err == nil && n == 0 {
			return notFound()
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// viewRowsDTO is a page of a saved view's rows.
type viewRowsDTO struct {
	query.Result
	// Limit and Offset are the page given; ask for the next with offset + limit while truncated is true.
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// GET /api/v1/views/{name}/rows?limit=&offset=&order=&<column>=<value> (query:run): a saved view's rows, as the caller
// sees them, read like a table. Any other parameter named after one of its columns keeps the rows where that column
// equals the value (several are ANDed; Postgres converts the value to the column's type). order is a column, or
// -column for descending. limit is 1 to 5,000 (default 100); truncated says there are more. A column named limit,
// offset or order cannot be filtered on here (use QUERY). The read is a query like any other: limited and recorded.
func (h *handlers) readSavedView(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	rd := query.Read{Limit: 100}
	if !intParam(w, qs.Get("limit"), "limit", 1, query.DefaultMaxRows, &rd.Limit) || !intParam(w, qs.Get("offset"), "offset", 0, 1<<30, &rd.Offset) {
		return
	}
	if order := qs.Get("order"); order != "" {
		rd.Order = []query.OrderBy{{Column: strings.TrimPrefix(order, "-"), Desc: strings.HasPrefix(order, "-")}}
	}
	keys := make([]string, 0, len(qs))
	for k := range qs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if k == "limit" || k == "offset" || k == "order" {
			continue
		}
		for _, v := range qs[k] {
			rd.Equal = append(rd.Equal, query.Condition{Column: k, Value: v})
		}
	}
	if out, ok := h.readView(w, r, rd); ok {
		writeJSON(w, http.StatusOK, out)
	}
}

type viewQueryRequest struct {
	// Where keeps the rows it matches: {"and": [...]}, {"or": [...]}, {"not": {...}}, or {"<column>": {"<op>": value}}
	// with op one of eq, ne, lt, lte, gt, gte, in and not_in (a list), between (two values), like and ilike (text
	// columns), is_null (true or false) and contains (a JSON value, for jsonb columns). Several operators on one
	// column are ANDed. Values are converted to the column's type. Up to 10 deep, 200 conditions, 1,000 values.
	Where json.RawMessage `json:"where"`
	// Order is the columns to sort by, in turn, each {"<column>": "asc" | "desc"}.
	Order []map[string]string `json:"order"`
	// Columns are the columns to return, in order; none means all.
	Columns []string `json:"columns"`
	// Limit is 1 to 5,000 (default 100).
	Limit int `json:"limit"`
	// Offset is how many matching rows to skip.
	Offset int `json:"offset"`
}

// QUERY /api/v1/views/{name}/rows (query:run): a saved view's rows, as the caller sees them, filtered, sorted and
// paged as the body says (see its fields). Like the GET form, but with every operator, and and/or/not. A filter that
// cannot be read is a 422 saying where in it (where.and[1].title: ...); truncated says there are more rows.
func (h *handlers) queryViewRows(w http.ResponseWriter, r *http.Request) {
	var req viewQueryRequest
	if !decode(w, r, &req) {
		return
	}
	rd := query.Read{Where: req.Where, Columns: req.Columns, Limit: req.Limit, Offset: req.Offset}
	if rd.Limit == 0 {
		rd.Limit = 100
	}
	if rd.Limit < 1 || rd.Limit > query.DefaultMaxRows || rd.Offset < 0 {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("limit must be 1 to %d, and offset 0 or more", query.DefaultMaxRows))
		return
	}
	for i, o := range req.Order {
		if len(o) != 1 {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(`order[%d]: give one column, as {"title": "asc"}`, i))
			return
		}
		for col, dir := range o {
			if dir != "asc" && dir != "desc" {
				writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(`order[%d]: %s is "asc" or "desc"`, i, col))
				return
			}
			rd.Order = append(rd.Order, query.OrderBy{Column: col, Desc: dir == "desc"})
		}
	}
	if out, ok := h.readView(w, r, rd); ok {
		writeJSON(w, http.StatusOK, out)
	}
}

// readView reads a saved view's rows as the caller: the view's columns, the SELECT for the read, then the query. On
// false it has written the error and the caller just returns.
func (h *handlers) readView(w http.ResponseWriter, r *http.Request, rd query.Read) (viewRowsDTO, bool) {
	p, _ := auth.PrincipalFrom(r.Context())
	if h.query == nil {
		writeError(w, http.StatusServiceUnavailable, "queries are not available on this server")
		return viewRowsDTO{}, false
	}
	name := chi.URLParam(r, "name")
	var columns []query.Column
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		v, err := q.GetSavedView(r.Context(), name)
		if err != nil {
			return err
		}
		return json.Unmarshal(v.Columns, &columns)
	}) {
		return viewRowsDTO{}, false
	}
	sql, params, err := query.ReadSQL(name, columns, rd)
	var fe *query.FilterError
	if errors.As(err, &fe) {
		code := http.StatusUnprocessableEntity
		if r.Method == http.MethodGet {
			code = http.StatusBadRequest
		}
		writeError(w, code, fe.Error())
		return viewRowsDTO{}, false
	}
	if err != nil {
		writeDBError(w, err)
		return viewRowsDTO{}, false
	}
	res, err := h.query.Run(r.Context(), query.Request{User: p.UserID, KeyID: p.KeyID, Scopes: p.Scopes, SQL: sql, Params: params})
	if err != nil {
		h.writeQueryError(w, err)
		return viewRowsDTO{}, false
	}
	out := viewRowsDTO{Result: *res, Limit: rd.Limit, Offset: rd.Offset}
	if len(out.Rows) > rd.Limit {
		out.Rows, out.Truncated = out.Rows[:rd.Limit], true
	}
	return out, true
}
