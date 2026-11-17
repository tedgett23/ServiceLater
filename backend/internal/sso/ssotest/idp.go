// Package ssotest is a small OpenID Connect provider for tests: discovery, keys, an authorize endpoint and a token
// endpoint with PKCE, signing real RS256 ID tokens. Go tests set Next and are sent straight back; the e2e run serves it
// from cmd/fakeidp, where the authorize page is a form to type who signs in.
package ssotest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// User is who signs in at the provider.
type User struct {
	Subject, Email, Name string
	EmailVerified        bool
	Groups               []string
}

// IdP is the fake provider. Issuer must be the address it is served at.
type IdP struct {
	Issuer, ClientID, ClientSecret string
	// Next, when set, signs in straight away as this person (Go tests); otherwise /authorize shows a form.
	Next *User

	key   *rsa.PrivateKey
	mu    sync.Mutex
	codes map[string]grant
}

type grant struct {
	user                      User
	clientID, redirect, nonce string
	challenge                 string
	expires                   time.Time
}

// New makes a provider for an issuer address and a client.
func New(issuer, clientID, clientSecret string) *IdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return &IdP{Issuer: strings.TrimRight(issuer, "/"), ClientID: clientID, ClientSecret: clientSecret, key: key, codes: map[string]grant{}}
}

// ServeHTTP serves discovery, keys, authorize and token.
func (p *IdP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		writeJSON(w, map[string]any{
			"issuer": p.Issuer, "authorization_endpoint": p.Issuer + "/authorize", "token_endpoint": p.Issuer + "/token",
			"jwks_uri": p.Issuer + "/jwks", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"}, "code_challenge_methods_supported": []string{"S256"},
		})
	case "/jwks":
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	case "/authorize":
		p.authorize(w, r)
	case "/token":
		p.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

var form = template.Must(template.New("f").Parse(`<!doctype html><title>Fake IdP</title>
<h1>Fake identity provider</h1>
<form method="post">
{{range $k, $v := .}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
<label>Email <input name="email"></label>
<label>Name <input name="name"></label>
<label>Groups <input name="groups"></label>
<label><input type="checkbox" name="unverified"> Email not verified</label>
<button>Sign in</button>
</form>`))

func (p *IdP) authorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q := r.Form
	if q.Get("client_id") != p.ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "bad authorize request", http.StatusBadRequest)
		return
	}
	var u User
	switch {
	case p.Next != nil:
		u = *p.Next
	case r.Method == http.MethodPost:
		email := strings.TrimSpace(q.Get("email"))
		u = User{Subject: "sub-" + email, Email: email, Name: q.Get("name"), EmailVerified: q.Get("unverified") == ""}
		for _, g := range strings.Split(q.Get("groups"), ",") {
			if g = strings.TrimSpace(g); g != "" {
				u.Groups = append(u.Groups, g)
			}
		}
	default:
		keep := map[string]string{}
		for _, k := range []string{"client_id", "redirect_uri", "state", "nonce", "code_challenge", "code_challenge_method", "response_type", "scope"} {
			keep[k] = q.Get(k)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = form.Execute(w, keep)
		return
	}
	code := random()
	p.mu.Lock()
	p.codes[code] = grant{user: u, clientID: q.Get("client_id"), redirect: q.Get("redirect_uri"), nonce: q.Get("nonce"),
		challenge: q.Get("code_challenge"), expires: time.Now().Add(time.Minute)}
	p.mu.Unlock()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	v := back.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound) //nolint:gosec // a fake provider for tests: it sends the browser where the client asked
}

func (p *IdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id, err := url.QueryUnescape(id); err != nil || id != p.ClientID {
		tokenError(w, "invalid_client")
		return
	}
	if s, err := url.QueryUnescape(secret); err != nil || s != p.ClientSecret {
		tokenError(w, "invalid_client")
		return
	}
	code := r.PostForm.Get("code")
	p.mu.Lock()
	g, ok := p.codes[code]
	delete(p.codes, code)
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case !ok || time.Now().After(g.expires) || r.PostForm.Get("redirect_uri") != g.redirect:
		tokenError(w, "invalid_grant")
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge:
		tokenError(w, "invalid_grant") // PKCE
		return
	}
	claims := map[string]any{"iss": p.Issuer, "aud": p.ClientID, "sub": g.user.Subject, "nonce": g.nonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
		"email": g.user.Email, "email_verified": g.user.EmailVerified, "name": g.user.Name, "groups": g.user.Groups}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	payload, _ := json.Marshal(claims)
	jws, err := signer.Sign(payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	idToken, _ := jws.CompactSerialize()
	writeJSON(w, map[string]any{"access_token": random(), "token_type": "Bearer", "expires_in": 300, "id_token": idToken})
}

func tokenError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, code)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func random() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
