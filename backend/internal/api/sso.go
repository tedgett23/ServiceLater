package api

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
	"github.com/servicelater/servicelater/backend/internal/sso"
)

const ssoPath = "/api/v1/auth/sso"

// signInOptionsDTO is what the sign-in page offers.
type signInOptionsDTO struct {
	// Providers are the single sign-on buttons, in order.
	Providers []ssoButtonDTO `json:"providers"`
	// Passwords is who may sign in with a password: everyone, or admins only (the rest use single sign-on).
	Passwords string `json:"passwords"`
}

type ssoButtonDTO struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// GET /auth/sso (no sign-in): the single sign-on providers to offer on the sign-in page, and whether passwords are for
// everyone or for admins only.
func (h *handlers) signInOptions(w http.ResponseWriter, r *http.Request) {
	out := signInOptionsDTO{Providers: []ssoButtonDTO{}, Passwords: "everyone"}
	err := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		rows, err := q.ListEnabledSSOProviders(r.Context())
		for _, p := range rows {
			out.Providers = append(out.Providers, ssoButtonDTO{ID: p.ID, Label: p.Label})
		}
		if err != nil {
			return err
		}
		if adminsOnly, err := auth.PasswordsForAdminsOnly(r.Context(), q); err != nil {
			return err
		} else if adminsOnly {
			out.Passwords = "admins"
		}
		return nil
	})
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// baseURL is the address people reach ServiceLater at: the base URL setting when it is set (behind a proxy that
// rewrites the host), else this request's.
func (h *handlers) baseURL(r *http.Request) string {
	var base string
	_ = db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		rows, err := dbgen.New(tx).GetSystemSettings(r.Context(), settings.EmailPrefix)
		stored := map[string][]byte{}
		for _, s := range rows {
			stored[s.Name] = s.Value
		}
		base = settings.EmailFrom(stored).BaseURL
		return err
	})
	if base != "" {
		return base
	}
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (h *handlers) callbackURL(r *http.Request, providerID string) string {
	return h.baseURL(r) + ssoPath + "/" + providerID + "/callback"
}

// localPath keeps where to go after signing in to a path on this site, never another site.
func localPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") || strings.HasPrefix(next, "/api/") {
		return "/"
	}
	return next
}

func ssoCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	// Lax, not Strict: the provider sends the browser back with a top-level GET from its own site.
	return &http.Cookie{Name: sso.StateCookie, Value: value, Path: ssoPath, MaxAge: maxAge, HttpOnly: true, //nolint:gosec // Secure follows the request, as for the session cookies
		Secure: secure(r), SameSite: http.SameSiteLaxMode}
}

// failSignIn sends the person back to the sign-in page with a message they can act on.
func failSignIn(w http.ResponseWriter, r *http.Request, label string, err error) {
	var se *sso.Error
	msg := "Signing in with " + label + " did not work. Try again, or ask an admin to check its settings."
	if errors.As(err, &se) {
		msg = se.Msg
	} else {
		slog.Error("single sign-on", "provider", label, "err", err)
	}
	http.Redirect(w, r, "/login?sso_error="+url.QueryEscape(msg), http.StatusFound)
}

// GET /auth/sso/{id}/start?next= (no sign-in): go to the provider to sign in; it sends the browser back to
// /auth/sso/{id}/callback. next is the page to open afterwards.
func (h *handlers) ssoStart(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	authURL, cookie, err := h.sso.Start(r.Context(), id, h.callbackURL(r, id), localPath(r.URL.Query().Get("next")))
	if err != nil {
		failSignIn(w, r, id, err)
		return
	}
	http.SetCookie(w, ssoCookie(r, cookie, 600))
	http.Redirect(w, r, authURL, http.StatusFound) //nolint:gosec // the provider's own address, from settings an admin made
}

// GET /auth/sso/{id}/callback?code=&state= (no sign-in): where the provider sends the browser back. On success it signs
// the person in (the same cookies as a password sign-in) and opens the page they asked for; otherwise it opens the
// sign-in page with a message (?sso_error=).
func (h *handlers) ssoCallback(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	qs := r.URL.Query()
	http.SetCookie(w, ssoCookie(r, "", -1))
	if e := qs.Get("error"); e != "" {
		failSignIn(w, r, id, &sso.Error{Msg: "The sign-in was cancelled or refused (" + e + ")."})
		return
	}
	c, err := r.Cookie(sso.StateCookie)
	if err != nil {
		failSignIn(w, r, id, &sso.Error{Msg: "The sign-in took too long, or was started somewhere else. Try again."})
		return
	}
	userID, next, err := h.sso.Finish(r.Context(), id, qs.Get("code"), qs.Get("state"), c.Value, h.callbackURL(r, id))
	if err != nil {
		failSignIn(w, r, id, err)
		return
	}
	h.perms.Forget() // group sync may have changed what they may do
	sess, err := h.auth.SignedInBySSO(r.Context(), userID)
	if err != nil {
		failSignIn(w, r, id, err)
		return
	}
	h.setCookies(w, r, sess)
	http.Redirect(w, r, next, http.StatusFound) //nolint:gosec // next went through localPath at the start and is signed into the state cookie
}

// ---- managing providers (settings:manage) ----

type ssoGroupMapDTO struct {
	// ClaimValue is a value of the provider's groups claim, such as an Entra group id or an Okta group name.
	ClaimValue string    `json:"claim_value"`
	GroupID    uuid.UUID `json:"group_id"`
	GroupName  string    `json:"group_name,omitempty"`
}

type ssoProviderDTO struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	// SecretSet says a client secret is stored (it is never shown).
	SecretSet      bool             `json:"secret_set"`
	AllowedDomains []string         `json:"allowed_domains"`
	CreateUsers    bool             `json:"create_users"`
	GroupsClaim    string           `json:"groups_claim"`
	GroupMaps      []ssoGroupMapDTO `json:"group_maps"`
	Enabled        bool             `json:"enabled"`
	// RedirectURI is the address to register at the provider as this client's sign-in redirect.
	RedirectURI string    `json:"redirect_uri"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (h *handlers) ssoProviderOut(r *http.Request, q *dbgen.Queries, p dbgen.SsoProvider, secretSet bool) (ssoProviderDTO, error) {
	out := ssoProviderDTO{ID: p.ID, Label: p.Label, Issuer: p.Issuer, ClientID: p.ClientID, SecretSet: secretSet,
		AllowedDomains: p.AllowedDomains, CreateUsers: p.CreateUsers, GroupsClaim: p.GroupsClaim, Enabled: p.Enabled,
		GroupMaps: []ssoGroupMapDTO{}, RedirectURI: h.callbackURL(r, p.ID), UpdatedAt: p.UpdatedAt.Time}
	maps, err := q.ListSSOGroupMaps(r.Context(), p.ID)
	for _, m := range maps {
		out.GroupMaps = append(out.GroupMaps, ssoGroupMapDTO{ClaimValue: m.ClaimValue, GroupID: m.GroupID, GroupName: m.GroupName})
	}
	return out, err
}

func secretNames(q *dbgen.Queries, r *http.Request) ([]string, error) {
	rows, err := q.SecretStatus(r.Context())
	out := make([]string, len(rows))
	for i, s := range rows {
		out[i] = s.Name
	}
	return out, err
}

// GET /sso-providers (settings:manage): the single sign-on providers, with the redirect address to register at each.
func (h *handlers) listSSOProviders(w http.ResponseWriter, r *http.Request) {
	out := []ssoProviderDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListSSOProviders(r.Context())
		if err != nil {
			return err
		}
		names, err := secretNames(q, r)
		if err != nil {
			return err
		}
		for _, p := range rows {
			d, err := h.ssoProviderOut(r, q, p, slices.Contains(names, sso.SecretName(p.ID)))
			if err != nil {
				return err
			}
			out = append(out, d)
		}
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

var (
	providerIDRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	domainRE      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	groupsClaimRE = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{0,100}$`)
)

// PUT /sso-providers/{id} (settings:manage): add or change a provider. client_secret sets the secret (null clears it,
// leaving it out keeps it; a provider without one is a public client, which PKCE still protects). group_maps
// replaces the group mappings. The id appears in the redirect address and cannot be changed.
func (h *handlers) putSSOProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Label    string `json:"label"`
		Issuer   string `json:"issuer"`
		ClientID string `json:"client_id"`
		// ClientSecret is encrypted before it is stored, which needs the server's secrets key.
		ClientSecret   nullable[string] `json:"client_secret"`
		AllowedDomains []string         `json:"allowed_domains"`
		CreateUsers    bool             `json:"create_users"`
		GroupsClaim    string           `json:"groups_claim"`
		GroupMaps      []ssoGroupMapDTO `json:"group_maps"`
		Enabled        bool             `json:"enabled"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Label, req.Issuer, req.ClientID = strings.TrimSpace(req.Label), strings.TrimRight(strings.TrimSpace(req.Issuer), "/"), strings.TrimSpace(req.ClientID)
	req.GroupsClaim = strings.TrimSpace(req.GroupsClaim)
	domains := []string{}
	for _, d := range req.AllowedDomains {
		if d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@")); d != "" && !slices.Contains(domains, d) {
			domains = append(domains, d)
		}
	}
	u, uerr := url.Parse(req.Issuer)
	var msg string
	switch {
	case !providerIDRE.MatchString(id):
		msg = "the id must be 2-31 lowercase letters, digits and hyphens, starting with a letter"
	case req.Label == "" || len(req.Label) > 60:
		msg = "label must be 1 to 60 characters"
	case uerr != nil || (u.Scheme != "https" && (u.Scheme != "http" || !settings.Local(u.Hostname()))) || u.Host == "":
		msg = "issuer must be the provider's https address (http only for a provider on this machine)"
	case req.ClientID == "" || len(req.ClientID) > 500:
		msg = "client_id is required"
	case !groupsClaimRE.MatchString(req.GroupsClaim):
		msg = "groups_claim must be a claim name such as groups"
	case len(req.GroupMaps) > 0 && req.GroupsClaim == "":
		msg = "set groups_claim to map groups"
	case req.ClientSecret.Set && req.ClientSecret.Value != nil && !h.keys.Configured():
		msg = "the server has no secrets key, so it cannot store a client secret: set SL_SECRETS_KEY or SL_SECRETS_KEY_FILE and restart"
	}
	for _, d := range domains {
		if !domainRE.MatchString(d) {
			msg = d + " is not a domain (such as example.com)"
		}
	}
	for _, m := range req.GroupMaps {
		if strings.TrimSpace(m.ClaimValue) == "" {
			msg = "each group mapping needs a claim value"
		}
	}
	if msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	var out ssoProviderDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		p, err := q.UpsertSSOProvider(r.Context(), dbgen.UpsertSSOProviderParams{ID: id, Label: req.Label, Issuer: req.Issuer,
			ClientID: req.ClientID, AllowedDomains: domains, CreateUsers: req.CreateUsers, GroupsClaim: req.GroupsClaim, Enabled: req.Enabled})
		if err != nil {
			return err
		}
		if err := q.ClearSSOGroupMaps(r.Context(), id); err != nil {
			return err
		}
		for _, m := range req.GroupMaps {
			if err := q.AddSSOGroupMap(r.Context(), dbgen.AddSSOGroupMapParams{ProviderID: id, ClaimValue: strings.TrimSpace(m.ClaimValue), GroupID: m.GroupID}); err != nil {
				return err
			}
		}
		if req.ClientSecret.Set {
			if req.ClientSecret.Value == nil || *req.ClientSecret.Value == "" {
				err = secrets.Clear(r.Context(), q, sso.SecretName(id))
			} else {
				err = secrets.Set(r.Context(), q, h.keys, sso.SecretName(id), *req.ClientSecret.Value)
			}
			if err != nil {
				return err
			}
		}
		names, err := secretNames(q, r)
		if err != nil {
			return err
		}
		out, err = h.ssoProviderOut(r, q, p, slices.Contains(names, sso.SecretName(id)))
		return err
	}) {
		return
	}
	h.sso.Forget()
	writeJSON(w, http.StatusOK, out)
}

// DELETE /sso-providers/{id} (settings:manage): remove a provider. People who signed in with it keep their accounts;
// they sign in another way.
func (h *handlers) deleteSSOProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		n, err := q.DeleteSSOProvider(r.Context(), id)
		if err == nil && n == 0 {
			return notFound()
		} else if err != nil {
			return err
		}
		return secrets.Clear(r.Context(), q, sso.SecretName(id))
	}) {
		return
	}
	h.sso.Forget()
	w.WriteHeader(http.StatusNoContent)
}

// GET /settings/sign-in (settings:manage): who may sign in with a password.
func (h *handlers) getSignInSettings(w http.ResponseWriter, r *http.Request) {
	out := map[string]string{"passwords": "everyone"}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListSettings(r.Context(), "sign_in.")
		for _, s := range rows {
			if s.Name == "sign_in.passwords" && string(s.Value) == `"admins"` {
				out["passwords"] = "admins"
			}
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT /settings/sign-in (settings:manage) with {"passwords": "everyone"|"admins"}: admins only sends everyone else to
// single sign-on (admins keep a password, so a broken provider cannot lock everyone out). It needs a provider that is
// switched on.
func (h *handlers) putSignInSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Passwords string `json:"passwords"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Passwords != "everyone" && req.Passwords != "admins" {
		writeError(w, http.StatusUnprocessableEntity, `passwords must be "everyone" or "admins"`)
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		if req.Passwords == "admins" {
			rows, err := q.ListSSOProviders(r.Context())
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(rows, func(p dbgen.SsoProvider) bool { return p.Enabled }) {
				return invalid("switch a single sign-on provider on first, or nobody but admins could sign in")
			}
		}
		return q.UpsertSetting(r.Context(), dbgen.UpsertSettingParams{Name: "sign_in.passwords", Value: []byte(`"` + req.Passwords + `"`)})
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"passwords": req.Passwords})
}
