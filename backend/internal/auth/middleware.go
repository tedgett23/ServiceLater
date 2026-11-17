package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db"
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
	// KeyID is the API key the request came with (uuid.Nil for a signed-in person), and Scopes what that key is
	// narrowed to (nil: everything its account may do). PermissionCache.Load and db.WithUser both apply them.
	KeyID  uuid.UUID
	Scopes []string
}

// Can reports whether the caller has a permission. It is the fast check in Go; RLS checks again.
func (p Principal) Can(perm string) bool { return p.Perms.Has(perm) }

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Authenticate rejects requests without a valid access cookie or API key (`Authorization: Bearer slk_...`, when
// t.Keys is set) and stores the Principal in the context. A key narrowed to some permissions also narrows the
// request's database transactions (db.WithScopes).
func (t *Tokens) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			key, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || t.Keys == nil {
				deny(w, http.StatusUnauthorized, "not authenticated")
				return
			}
			p, err := t.Keys.Verify(r.Context(), strings.TrimSpace(key))
			if err != nil {
				if !errors.Is(err, ErrBadKey) && r.Context().Err() == nil {
					slog.Error("checking an API key", "err", err)
				}
				deny(w, http.StatusUnauthorized, "not authenticated")
				return
			}
			ctx := context.WithValue(r.Context(), ctxKey{}, p)
			if p.Scopes != nil {
				ctx = db.WithScopes(ctx, p.Scopes)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
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

// RequireRecordPermission is the same pre-check for a record route, where the type is known only once the record is
// loaded: the caller needs verb on some record type. The handler (records.Service) then checks the record's own type.
func RequireRecordPermission(verb string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok || !p.Perms.CanRecordAny(verb) {
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
