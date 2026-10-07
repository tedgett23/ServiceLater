package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/authz"
)

const (
	AccessCookie  = "sl_access"
	RefreshCookie = "sl_refresh"
)

type ctxKey struct{}

// Principal is the authenticated caller, taken from a verified access token. Perms is filled by
// PermissionCache.Load from the database; Role (the base role, from the token) is for display only.
type Principal struct {
	UserID  uuid.UUID
	Role    string
	Perms   authz.Set
	Expires time.Time // when the access token stops being valid
}

// Can reports whether the caller has a permission. It is the fast check in Go; RLS checks again.
func (p Principal) Can(perm string) bool { return p.Perms.Has(perm) }

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
		p, err := t.Parse(c.Value)
		if err != nil {
			deny(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

// RequirePermission is a fast pre-check before the database: the caller needs at least one of perms. RLS remains
// the authoritative check. It needs PermissionCache.Load earlier in the chain.
func RequirePermission(perms ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok || !p.Perms.Any(perms...) {
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
