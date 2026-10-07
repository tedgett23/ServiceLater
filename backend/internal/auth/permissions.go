package auth

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// permissionTTL is how stale the Go-side check may be on another instance after a role or grant changes. RLS loads
// permissions afresh for every transaction, so this only delays a refusal or a menu, never grants access.
const permissionTTL = 5 * time.Second

// PermissionCache looks up what each person may do, for the fast checks in Go, and keeps it briefly.
type PermissionCache struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	m    map[uuid.UUID]cachedPerms
}

type cachedPerms struct {
	perms authz.Set
	at    time.Time
}

func NewPermissionCache(pool *pgxpool.Pool) *PermissionCache {
	return &PermissionCache{pool: pool, m: map[uuid.UUID]cachedPerms{}}
}

// Get returns a person's permissions: none when deactivated or unknown.
func (c *PermissionCache) Get(ctx context.Context, id uuid.UUID) (authz.Set, error) {
	c.mu.Lock()
	e, ok := c.m[id]
	c.mu.Unlock()
	if ok && time.Since(e.at) < permissionTTL {
		return e.perms, nil
	}
	var perms []string
	err := db.WithSystem(ctx, c.pool, func(tx pgx.Tx) error {
		var err error
		perms, err = dbgen.New(tx).PermissionsOf(ctx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.m[id] = cachedPerms{perms: perms, at: time.Now()}
	c.mu.Unlock()
	return perms, nil
}

// Forget drops everything cached, after a change that can alter anyone's permissions (a role, a grant, a group
// membership, someone's base role). Other instances catch up within permissionTTL.
func (c *PermissionCache) Forget() {
	c.mu.Lock()
	c.m = map[uuid.UUID]cachedPerms{}
	c.mu.Unlock()
}

// Load fills the authenticated caller's permissions in. It goes after Authenticate.
func (c *PermissionCache) Load(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			deny(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		perms, err := c.Get(r.Context(), p.UserID)
		if err != nil {
			if r.Context().Err() == nil {
				slog.Error("loading permissions", "user", p.UserID, "err", err)
			}
			deny(w, http.StatusInternalServerError, "internal error")
			return
		}
		p.Perms = perms
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}
