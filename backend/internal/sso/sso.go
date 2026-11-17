// Package sso signs people in with OpenID Connect (docs/auth.md): Okta, Microsoft Entra ID, Google Workspace,
// Keycloak and any other provider that speaks it. The flow is the authorization code flow with PKCE and a nonce; what
// it needs between leaving for the provider and coming back travels in a short-lived signed cookie, so nothing is
// stored for sign-ins in progress. Coming back, the person is found by the provider's subject, else linked by a
// verified email, else (if the provider allows it) given an account; their mapped groups are then brought in line.
package sso

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
)

// StateCookie carries a sign-in in progress from Start to Finish.
const StateCookie = "sl_sso"

// stateTTL is how long someone may spend at the provider.
const stateTTL = 10 * time.Minute

// discoveryTTL is how long a provider's published configuration (endpoints, keys) is kept before it is read again.
const discoveryTTL = time.Hour

// SecretName is the secret holding a provider's client secret. Secret names take no hyphens, and provider ids take no
// underscores, so swapping one for the other keeps names apart.
func SecretName(providerID string) string {
	return "sso." + strings.ReplaceAll(providerID, "-", "_") + ".client_secret"
}

// Error is a sign-in refused for a reason the person can be told.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func refuse(format string, args ...any) error { return &Error{Msg: fmt.Sprintf(format, args...)} }

// Manager runs sign-ins. Pool and Keys read providers and client secrets as the system; StateKey signs the state
// cookie (derive it from the server's secret: every api instance must share it).
type Manager struct {
	Pool     *pgxpool.Pool
	Keys     *secrets.Keyring
	StateKey []byte
	// Client is the HTTP client for talking to providers (tests point it at a fake one); nil means the default.
	Client *http.Client

	mu        sync.Mutex
	discovery map[string]discovered
}

type discovered struct {
	provider *oidc.Provider
	at       time.Time
}

type state struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x"`
	Expires  int64  `json:"e"`
}

func (m *Manager) ctx(ctx context.Context) context.Context {
	if m.Client != nil {
		return oidc.ClientContext(ctx, m.Client)
	}
	return ctx
}

func (m *Manager) discover(ctx context.Context, issuer string) (*oidc.Provider, error) {
	m.mu.Lock()
	d, ok := m.discovery[issuer]
	m.mu.Unlock()
	if ok && time.Since(d.at) < discoveryTTL {
		return d.provider, nil
	}
	p, err := oidc.NewProvider(m.ctx(ctx), issuer)
	if err != nil {
		return nil, fmt.Errorf("reading the provider's configuration from %s: %w", issuer, err)
	}
	m.mu.Lock()
	if m.discovery == nil {
		m.discovery = map[string]discovered{}
	}
	m.discovery[issuer] = discovered{provider: p, at: time.Now()}
	m.mu.Unlock()
	return p, nil
}

// Forget drops what was read from providers, after their settings change.
func (m *Manager) Forget() {
	m.mu.Lock()
	m.discovery = nil
	m.mu.Unlock()
}

// Provider is the part of a provider's settings the flow needs.
type Provider struct {
	ID, Label, Issuer, ClientID string
	AllowedDomains              []string
	CreateUsers                 bool
	GroupsClaim                 string
}

func (m *Manager) load(ctx context.Context, id string) (Provider, string, error) {
	var p Provider
	var secret string
	err := db.WithSystem(ctx, m.Pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, err := q.GetEnabledSSOProvider(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("That sign-in option is not available.")
		} else if err != nil {
			return err
		}
		p = Provider{ID: row.ID, Label: row.Label, Issuer: row.Issuer, ClientID: row.ClientID, AllowedDomains: row.AllowedDomains,
			CreateUsers: row.CreateUsers, GroupsClaim: row.GroupsClaim}
		secret, err = secrets.Get(ctx, q, m.Keys, SecretName(id))
		if errors.Is(err, secrets.ErrNotSet) {
			secret, err = "", nil // a public client, with PKCE
		}
		return err
	})
	return p, secret, err
}

func (m *Manager) config(ctx context.Context, p Provider, secret, redirectURL string) (*oidc.Provider, *oauth2.Config, error) {
	op, err := m.discover(ctx, p.Issuer)
	if err != nil {
		return nil, nil, err
	}
	scopes := []string{oidc.ScopeOpenID, "email", "profile"}
	return op, &oauth2.Config{ClientID: p.ClientID, ClientSecret: secret, Endpoint: op.Endpoint(), RedirectURL: redirectURL, Scopes: scopes}, nil
}

func random() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (m *Manager) sign(payload []byte) string {
	mac := hmac.New(sha256.New, m.StateKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (m *Manager) open(cookie string) (state, bool) {
	body, sig, ok := strings.Cut(cookie, ".")
	if !ok {
		return state{}, false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(body)
	got, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return state{}, false
	}
	mac := hmac.New(sha256.New, m.StateKey)
	mac.Write(payload)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return state{}, false
	}
	var s state
	if json.Unmarshal(payload, &s) != nil || time.Now().Unix() > s.Expires {
		return state{}, false
	}
	return s, true
}

// Start begins a sign-in: the provider's address to send the person to, and the state cookie to set. next is where
// to go afterwards (a path on this site).
func (m *Manager) Start(ctx context.Context, providerID, redirectURL, next string) (authURL, cookie string, err error) {
	p, secret, err := m.load(ctx, providerID)
	if err != nil {
		return "", "", err
	}
	_, cfg, err := m.config(ctx, p, secret, redirectURL)
	if err != nil {
		return "", "", err
	}
	s := state{Provider: p.ID, State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(), Next: next,
		Expires: time.Now().Add(stateTTL).Unix()}
	payload, _ := json.Marshal(s)
	url := cfg.AuthCodeURL(s.State, oidc.Nonce(s.Nonce), oauth2.S256ChallengeOption(s.Verifier))
	return url, m.sign(payload), nil
}

// Claims are what the provider said about the person.
type Claims struct {
	Subject, Email, Name string
	EmailVerified        bool
	Groups               []string
}

// Finish completes a sign-in when the provider sends the person back: it checks the state, swaps the code for an ID
// token, checks the token, and works out who the person is. It returns their user id and where to go next.
func (m *Manager) Finish(ctx context.Context, providerID, code, stateParam, cookie, redirectURL string) (uuid.UUID, string, error) {
	s, ok := m.open(cookie)
	if !ok || s.Provider != providerID || subtle.ConstantTimeCompare([]byte(s.State), []byte(stateParam)) != 1 {
		return uuid.Nil, "", refuse("The sign-in took too long, or was started somewhere else. Try again.")
	}
	p, secret, err := m.load(ctx, providerID)
	if err != nil {
		return uuid.Nil, "", err
	}
	op, cfg, err := m.config(ctx, p, secret, redirectURL)
	if err != nil {
		return uuid.Nil, "", err
	}
	tok, err := cfg.Exchange(m.ctx(ctx), code, oauth2.VerifierOption(s.Verifier))
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("swapping the code with %s: %w", p.Issuer, err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return uuid.Nil, "", errors.New("the provider sent no ID token")
	}
	idt, err := op.Verifier(&oidc.Config{ClientID: p.ClientID}).Verify(m.ctx(ctx), raw)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("checking the ID token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(s.Nonce)) != 1 {
		return uuid.Nil, "", errors.New("the ID token's nonce does not match")
	}
	c, err := claimsOf(idt, p.GroupsClaim)
	if err != nil {
		return uuid.Nil, "", err
	}
	id, err := m.Resolve(ctx, p, c)
	return id, s.Next, err
}

func claimsOf(idt *oidc.IDToken, groupsClaim string) (Claims, error) {
	var all map[string]any
	if err := idt.Claims(&all); err != nil {
		return Claims{}, err
	}
	c := Claims{Subject: idt.Subject}
	c.Email, _ = all["email"].(string)
	c.Email = strings.ToLower(strings.TrimSpace(c.Email))
	switch v := all["email_verified"].(type) {
	case bool:
		c.EmailVerified = v
	case string: // some providers send "true"
		c.EmailVerified = v == "true"
	}
	c.Name, _ = all["name"].(string)
	if c.Name == "" {
		c.Name, _ = all["preferred_username"].(string)
	}
	if groupsClaim != "" {
		switch v := all[groupsClaim].(type) {
		case []any:
			for _, g := range v {
				if s, ok := g.(string); ok {
					c.Groups = append(c.Groups, s)
				}
			}
		case string:
			c.Groups = []string{v}
		}
	}
	return c, nil
}

// Resolve works out who signed in, as the system: by the provider's subject, else by a verified email (a person,
// never an app's or a service account), else a new account when the provider allows it. Then their mapped groups
// are brought in line with the groups the provider listed.
func (m *Manager) Resolve(ctx context.Context, p Provider, c Claims) (uuid.UUID, error) {
	if c.Subject == "" {
		return uuid.Nil, errors.New("the ID token names no subject")
	}
	if len(p.AllowedDomains) > 0 {
		_, domain, _ := strings.Cut(c.Email, "@")
		if !slices.ContainsFunc(p.AllowedDomains, func(d string) bool { return strings.EqualFold(d, domain) }) {
			return uuid.Nil, refuse("%s accounts cannot sign in here.", orSomething(domain))
		}
	}
	var userID uuid.UUID
	err := db.WithSystem(ctx, m.Pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var err error
		userID, err = q.GetIdentityUser(ctx, dbgen.GetIdentityUserParams{ProviderID: p.ID, Subject: c.Subject})
		switch {
		case err == nil:
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		case c.Email == "":
			return refuse("%s did not say who you are (no email address). Ask an admin to check its settings.", p.Label)
		case !c.EmailVerified:
			return refuse("%s has not verified your email address, so it cannot be matched to an account.", p.Label)
		default:
			person, err := q.GetPersonByEmail(ctx, c.Email)
			switch {
			case err == nil:
				userID = person.ID
			case !errors.Is(err, pgx.ErrNoRows):
				return err
			case !p.CreateUsers:
				return refuse("There is no ServiceLater account for %s. Ask an admin to add you.", c.Email)
			default:
				name := strings.TrimSpace(c.Name)
				if name == "" {
					name = c.Email
				}
				if len(name) > 200 {
					name = name[:200]
				}
				u, err := q.CreateUser(ctx, dbgen.CreateUserParams{Email: c.Email, Name: name, Role: "end_user"})
				if err != nil {
					return err
				}
				userID = u.ID
			}
		}
		u, err := q.GetSignInUser(ctx, userID)
		if err != nil {
			return err
		}
		if u.DeactivatedAt.Valid {
			return refuse("Your ServiceLater account has been deactivated.")
		}
		if err := q.LinkIdentity(ctx, dbgen.LinkIdentityParams{ProviderID: p.ID, Subject: c.Subject, UserID: userID, Email: c.Email}); err != nil {
			return err
		}
		if p.GroupsClaim != "" {
			return syncGroups(ctx, q, p.ID, userID, c.Groups)
		}
		return nil
	})
	return userID, err
}

func orSomething(domain string) string {
	if domain == "" {
		return "Accounts without an email address"
	}
	return "Accounts at " + domain
}

// syncGroups puts someone in each mapped group one of their claim values maps to, and takes them out of each mapped
// group none does. Groups no mapping names are not touched.
func syncGroups(ctx context.Context, q *dbgen.Queries, providerID string, userID uuid.UUID, have []string) error {
	maps, err := q.SystemSSOGroupMaps(ctx, providerID)
	if err != nil {
		return err
	}
	want := map[uuid.UUID]bool{}
	for _, mp := range maps {
		want[mp.GroupID] = want[mp.GroupID] || slices.Contains(have, mp.ClaimValue)
	}
	for g, in := range want {
		if in {
			err = q.AddGroupMember(ctx, dbgen.AddGroupMemberParams{GroupID: g, UserID: userID})
		} else {
			_, err = q.RemoveGroupMember(ctx, dbgen.RemoveGroupMemberParams{GroupID: g, UserID: userID})
		}
		if err != nil {
			return err
		}
	}
	return nil
}
