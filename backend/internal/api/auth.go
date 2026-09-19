package api

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

const refreshPath = "/api/v1/auth"

func (h *handlers) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	// RemoteAddr, not forwarded headers: those are client-controlled and would let attackers dodge the limit.
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	sess, err := h.auth.Login(r.Context(), req.Email, req.Password, ip)
	if err != nil {
		h.authError(w, err)
		return
	}
	h.setCookies(w, r, sess)
	writeJSON(w, http.StatusOK, sess.User)
}

func (h *handlers) refresh(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(auth.RefreshCookie)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	sess, err := h.auth.Refresh(r.Context(), c.Value)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidSession) {
			clearCookies(w, r)
		}
		h.authError(w, err)
		return
	}
	h.setCookies(w, r, sess)
	writeJSON(w, http.StatusOK, sess.User)
}

func (h *handlers) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.RefreshCookie); err == nil {
		if err := h.auth.Logout(r.Context(), c.Value); err != nil {
			slog.Error("logout", "err", err)
		}
	}
	clearCookies(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) me(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var u dbgen.User
	err := db.WithUser(r.Context(), h.pool, p.UserID, func(tx pgx.Tx) error {
		var err error
		u, err = dbgen.New(tx).GetUser(r.Context(), p.UserID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	if err != nil {
		slog.Error("me", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, auth.User{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role})
}

func (h *handlers) authError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrInvalidSession):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, auth.ErrRateLimited):
		w.Header().Set("Retry-After", "900")
		writeError(w, http.StatusTooManyRequests, err.Error())
	default:
		slog.Error("auth", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (h *handlers) setCookies(w http.ResponseWriter, r *http.Request, s *auth.Session) {
	http.SetCookie(w, cookie(r, auth.AccessCookie, s.AccessToken, "/", s.AccessExpires, 0))
	// The refresh token is only ever sent to the auth endpoints.
	http.SetCookie(w, cookie(r, auth.RefreshCookie, s.RefreshToken, refreshPath, s.RefreshExpires, 0))
}

func clearCookies(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, cookie(r, auth.AccessCookie, "", "/", time.Time{}, -1))
	http.SetCookie(w, cookie(r, auth.RefreshCookie, "", refreshPath, time.Time{}, -1))
}

// cookie builds an httpOnly, SameSite=Strict cookie. Secure follows the request scheme so plain-HTTP dev and
// TLS-terminating proxies (X-Forwarded-Proto) both work; production runs behind HTTPS.
func cookie(r *http.Request, name, value, path string, expires time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // Secure is deliberately request-dependent, see above
		Name: name, Value: value, Path: path, Expires: expires, MaxAge: maxAge,
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteStrictMode,
	}
}
