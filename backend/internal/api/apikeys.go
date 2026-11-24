package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// serviceAccountDomain is the made-up mail domain of service accounts' addresses: they have no mailbox.
const serviceAccountDomain = "service.servicelater.invalid"

const maxKeysPerAccount = 20

// apiKeyDTO is an API key as it is listed: never the key itself.
type apiKeyDTO struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Prefix is the key's first characters (slk_ and eight more), to tell keys apart.
	Prefix string `json:"prefix"`
	// Scopes are the permissions the key is narrowed to; absent when it may do everything its account may.
	Scopes        []string   `json:"scopes,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	CreatedByName *string    `json:"created_by_name"`
	ExpiresAt     *time.Time `json:"expires_at"`
	LastUsedAt    *time.Time `json:"last_used_at"`
	RevokedAt     *time.Time `json:"revoked_at"`
	// RateLimit is the key's own limit, requests a minute (0: none); null follows the setting for keys.
	RateLimit *int32 `json:"rate_limit"`
}

func tsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// POST /service-accounts (admin) with {"name", "description"?, "role"?}: an account for an integration. It cannot sign
// in; it calls the API with API keys (POST /users/{id}/api-keys). Give it roles and groups like a person; role is its
// base role (end_user when left out; admin needs roles:manage).
func (h *handlers) createServiceAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		// Description says what it is for and who looks after it.
		Description string `json:"description"`
		Role        string `json:"role"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Role == "" {
		req.Role = "end_user"
	}
	name, err := cleanName(req.Name)
	if err == nil && !validRole(req.Role) {
		err = invalid("role must be admin, agent or end_user")
	}
	if err == nil && utf8.RuneCountInString(req.Description) > 500 {
		err = invalid("description must be at most 500 characters")
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	slug := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	email := strings.TrimLeft(slug+"-"+hex.EncodeToString(b), "-") + "@" + serviceAccountDomain
	var out personDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		if req.Role == "admin" && !p.Can(authz.RolesManage) {
			return forbidden("making an admin needs the roles:manage permission")
		}
		u, err := q.CreateUser(r.Context(), dbgen.CreateUserParams{Email: email, Name: name, Role: req.Role})
		if err != nil {
			return userConflict(err)
		}
		out = toPerson(u)
		out.ServiceAccount = true
		return q.CreateServiceAccount(r.Context(), dbgen.CreateServiceAccountParams{UserID: u.ID, Description: strings.TrimSpace(req.Description)})
	}) {
		return
	}
	w.Header().Set("Location", "/api/v1/users/"+out.ID.String())
	writeJSON(w, http.StatusCreated, out)
}

// GET /users/{id}/api-keys (admin): a service account's keys, live ones first. The keys themselves are never shown
// again after they are made.
func (h *handlers) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	out := []apiKeyDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListAPIKeys(r.Context(), id)
		for _, k := range rows {
			d := apiKeyDTO{ID: k.ID, Name: k.Name, Prefix: k.Prefix, Scopes: k.Scopes, CreatedAt: k.CreatedAt.Time,
				ExpiresAt: tsPtr(k.ExpiresAt), LastUsedAt: tsPtr(k.LastUsedAt), RevokedAt: tsPtr(k.RevokedAt)}
			if k.RateLimit.Valid {
				d.RateLimit = &k.RateLimit.Int32
			}
			if k.CreatedByName.Valid {
				d.CreatedByName = &k.CreatedByName.String
			}
			out = append(out, d)
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// POST /users/{id}/api-keys (admin) with {"name", "expires_at"?, "scopes"?}: a new key for a service account. The
// response's key is shown this once: send it as `Authorization: Bearer <key>`. scopes narrows the key to some of
// the account's permissions (each must be one the account has); without it the key may do whatever the account may,
// now and later. Nobody can make a key that could do something they cannot do themselves.
func (h *handlers) createAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
		// ExpiresAt is when the key stops working; never when left out.
		ExpiresAt *time.Time `json:"expires_at"`
		// Scopes are permission names, such as records:create:incident or records:read:*.
		Scopes []string `json:"scopes"`
	}
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	switch {
	case name == "" || utf8.RuneCountInString(name) > 100:
		writeError(w, http.StatusUnprocessableEntity, "name must be 1 to 100 characters")
		return
	case req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()):
		writeError(w, http.StatusUnprocessableEntity, "expires_at must be in the future")
		return
	case req.Scopes != nil && len(req.Scopes) == 0:
		writeError(w, http.StatusUnprocessableEntity, "scopes must name at least one permission, or be left out")
		return
	}
	key, shown, hash, err := auth.NewAPIKey()
	if err != nil {
		writeDBError(w, err)
		return
	}
	var out struct {
		apiKeyDTO
		// Key is the key itself, shown only in this response.
		Key string `json:"key"`
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if _, err := q.GetServiceAccount(r.Context(), id); err != nil {
			if _, uerr := q.GetUser(r.Context(), id); uerr == nil {
				return invalid("API keys belong to service accounts: make one for the integration")
			}
			return err
		}
		existing, err := q.ListAPIKeys(r.Context(), id)
		if err != nil {
			return err
		}
		live := 0
		for _, k := range existing {
			if !k.RevokedAt.Valid {
				live++
			}
		}
		if live >= maxKeysPerAccount {
			return invalid("an account may have at most 20 keys that are not revoked")
		}
		if req.Scopes != nil {
			perms, err := q.PermissionsOf(r.Context(), id)
			if err != nil {
				return err
			}
			for _, s := range req.Scopes {
				if len(authz.Narrow(perms, []string{s})) == 0 {
					return invalid("the account does not have " + s + ", so the key cannot be narrowed to it")
				}
			}
		}
		arg := dbgen.CreateAPIKeyParams{UserID: id, Name: name, Prefix: shown, SecretHash: hash, Scopes: req.Scopes}
		if req.ExpiresAt != nil {
			arg.ExpiresAt = pgtype.Timestamptz{Time: *req.ExpiresAt, Valid: true}
		}
		kid, err := q.CreateAPIKey(r.Context(), arg)
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "42501" && strings.HasPrefix(pe.Message, "the key could do") {
			return forbidden(pe.Message)
		} else if err != nil {
			return err
		}
		out.apiKeyDTO = apiKeyDTO{ID: kid, Name: name, Prefix: shown, Scopes: req.Scopes, CreatedAt: time.Now(), ExpiresAt: req.ExpiresAt}
		out.Key = key
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// DELETE /users/{id}/api-keys/{keyId} (admin): revoke a key. It stops working at once.
func (h *handlers) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	kid, ok := urlUUID(w, r, "keyId")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		revoked, err := q.RevokeAPIKey(r.Context(), dbgen.RevokeAPIKeyParams{UserID: id, ID: kid})
		if err == nil && !revoked {
			return notFound()
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiKeyPatch changes a key.
type apiKeyPatch struct {
	// RateLimit is the key's own limit, requests a minute: 0 for none, 10 to 1,000,000, or null to follow the
	// setting for keys (Admin → Rate limits).
	RateLimit nullable[int32] `json:"rate_limit"`
}

// PATCH /users/{id}/api-keys/{keyId} (admin): give a key its own rate limit, for an integration that needs more (or
// less) than the setting for keys. It applies within a second.
func (h *handlers) updateAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	kid, ok := urlUUID(w, r, "keyId")
	if !ok {
		return
	}
	var req apiKeyPatch
	if !decode(w, r, &req) {
		return
	}
	if !req.RateLimit.Set {
		writeError(w, http.StatusBadRequest, "nothing to change: send rate_limit")
		return
	}
	limit := pgtype.Int4{}
	if v := req.RateLimit.Value; v != nil {
		if *v != 0 && (*v < 10 || *v > 1_000_000) {
			writeError(w, http.StatusUnprocessableEntity, "rate_limit must be between 10 and 1000000 requests a minute, 0 for no limit, or null for the setting")
			return
		}
		limit = pgtype.Int4{Int32: *v, Valid: true}
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		found, err := q.SetAPIKeyRateLimit(r.Context(), dbgen.SetAPIKeyRateLimitParams{UserID: id, ID: kid, RateLimit: limit})
		if err == nil && !found {
			return notFound()
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
