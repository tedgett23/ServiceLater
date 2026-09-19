package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/google/uuid"
)

const (
	AccessCookie  = "sl_access"
	RefreshCookie = "sl_refresh"
)

type ctxKey struct{}

// Principal is the authenticated caller, taken from a verified access token.
type Principal struct {
	UserID uuid.UUID
	Role   string
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Authenticate rejects requests without a valid access cookie and stores the Principal in the context.
func (t *Tokens) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(AccessCookie)
		if err != nil {
			deny(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		id, role, err := t.Parse(c.Value)
		if err != nil {
			deny(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, Principal{UserID: id, Role: role})))
	})
}

// RequireRole is a fast pre-check before the database. RLS remains the authoritative check.
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok || !slices.Contains(roles, p.Role) {
				deny(w, http.StatusForbidden, "forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func deny(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
