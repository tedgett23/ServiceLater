package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
)

// apiError is returned from transaction bodies to produce a specific HTTP response.
type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func invalid(msg string) error     { return &apiError{http.StatusUnprocessableEntity, msg} }
func forbidden(msg string) error   { return &apiError{http.StatusForbidden, msg} }
func notFound() error              { return &apiError{http.StatusNotFound, "not found"} }
func unavailable(msg string) error { return &apiError{http.StatusServiceUnavailable, msg} }

// tx runs fn as the authenticated user under RLS and writes the error response itself on failure.
// It reports whether fn succeeded; on false the caller must just return.
func (h *handlers) tx(w http.ResponseWriter, r *http.Request, fn func(q *dbgen.Queries, p auth.Principal) error) bool {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return false
	}
	err := db.WithUser(r.Context(), h.pool, p.UserID, func(tx pgx.Tx) error {
		return fn(dbgen.New(tx), p)
	})
	if err == nil {
		return true
	}
	writeDBError(w, err)
	return false
}

func writeDBError(w http.ResponseWriter, err error) {
	var ae *apiError
	var re *records.Error
	var pe *pgconn.PgError
	switch {
	case errors.Is(err, context.Canceled):
		// The client went away (closed the tab, navigated on). Nobody is left to answer, and it is not a fault.
		return
	case errors.As(err, &ae):
		writeError(w, ae.code, ae.msg)
	case errors.As(err, &re):
		writeError(w, map[records.Kind]int{records.KindInvalid: http.StatusUnprocessableEntity, records.KindForbidden: http.StatusForbidden,
			records.KindConflict: http.StatusConflict, records.KindBad: http.StatusBadRequest}[re.Kind], re.Msg)
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not found") // missing or hidden by RLS: indistinguishable on purpose
	case errors.As(err, &pe) && pe.Code == "42501": // insufficient_privilege, incl. RLS WITH CHECK failures
		writeError(w, http.StatusForbidden, "forbidden")
	case errors.As(err, &pe) && pe.Code == "23503": // foreign_key_violation
		writeError(w, http.StatusUnprocessableEntity, "referenced record does not exist")
	case errors.As(err, &pe) && pe.Code == "23514": // check_violation
		writeError(w, http.StatusUnprocessableEntity, "value out of range")
	default:
		slog.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
