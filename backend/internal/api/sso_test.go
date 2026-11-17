package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/sso/ssotest"
)

// startIdP serves a fake OpenID Connect provider.
func startIdP(t *testing.T) *ssotest.IdP {
	t.Helper()
	var idp *ssotest.IdP
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { idp.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	idp = ssotest.New(srv.URL, "servicelater", "s3cret")
	return idp
}

// ssoSignIn goes through a sign-in as a browser would: start, the provider (which signs in idp.Next), the callback.
// It returns where the callback sent the browser and the cookies it set.
func ssoSignIn(t *testing.T, h http.Handler, provider, next string, mangle ...func(*url.Values, *[]*http.Cookie)) (string, []*http.Cookie) {
	t.Helper()
	w := do(h, "GET", "/api/v1/auth/sso/"+provider+"/start?next="+url.QueryEscape(next), "", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	state := respCookie(w, "sl_sso")
	if state == nil || state.SameSite != http.SameSiteLaxMode || !state.HttpOnly {
		t.Fatalf("the state cookie: %+v", state)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", w.Header().Get("Location"), nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusFound || back.Path != "/api/v1/auth/sso/"+provider+"/callback" || back.Host != "sl.test" {
		t.Fatalf("the provider sent the browser to %q (%d)", resp.Header.Get("Location"), resp.StatusCode)
	}
	q := back.Query()
	cookies := []*http.Cookie{state}
	for _, m := range mangle {
		m(&q, &cookies)
	}
	w = do(h, "GET", back.Path+"?"+q.Encode(), "", cookies)
	if w.Code != http.StatusFound {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
	return w.Header().Get("Location"), w.Result().Cookies()
}

func ssoError(location string) string {
	u, _ := url.Parse(location)
	if u.Path != "/login" {
		return ""
	}
	return u.Query().Get("sso_error")
}

func providerJSON(issuer, extra string) string {
	return `{"label":"Okta","issuer":"` + issuer + `","client_id":"servicelater","client_secret":"s3cret","enabled":true,"create_users":true` + extra + `}`
}

// Signing in with a provider: a new person gets an account and their mapped groups, later sign-ins find them by the
// provider's subject and bring their groups in line, an existing person is matched by a verified email, and the
// provider's settings decide who may come in.
func TestSingleSignOnWithOpenIDConnect(t *testing.T) {
	h, pool, _, _ := setupEmail(t, testKey(3))
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@corp.test", "agent")
	idp := startIdP(t)
	desk := decodeBody[struct {
		ID string `json:"id"`
	}](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Service Desk"}`))
	other := decodeBody[struct {
		ID string `json:"id"`
	}](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Facilities"}`))

	refusal(t, agent.do(h, "PUT", "/api/v1/sso-providers/corp-okta", providerJSON(idp.Issuer, "")), 403, "", "an agent adding a provider")
	refusal(t, admin.do(h, "PUT", "/api/v1/sso-providers/corp-okta", providerJSON("http://idp.example.com", "")), 422, "https", "an http issuer elsewhere")
	w := admin.do(h, "PUT", "/api/v1/sso-providers/corp-okta", providerJSON(idp.Issuer,
		`,"allowed_domains":["corp.test","@Corp.Test"],"groups_claim":"groups","group_maps":[{"claim_value":"sl-desk","group_id":"`+desk.ID+`"}]`))
	expect(t, w, 200, "adding a provider")
	if body := w.Body.String(); strings.Contains(body, "s3cret") || !strings.Contains(body, `"secret_set":true`) ||
		!strings.Contains(body, `"redirect_uri":"http://sl.test/api/v1/auth/sso/corp-okta/callback"`) || !strings.Contains(body, `"allowed_domains":["corp.test"]`) {
		t.Errorf("the provider: %s", body)
	}
	opts := do(h, "GET", "/api/v1/auth/sso", "", nil).Body.String()
	if !strings.Contains(opts, `{"id":"corp-okta","label":"Okta"}`) || !strings.Contains(opts, `"passwords":"everyone"`) {
		t.Errorf("the sign-in options: %s", opts)
	}

	// A new person: an account, an end user, in the desk group the claim maps to.
	idp.Next = &ssotest.User{Subject: "00u1", Email: "Mia@corp.test", Name: "Mia Chen", EmailVerified: true, Groups: []string{"sl-desk", "unmapped"}}
	loc, cookies := ssoSignIn(t, h, "corp-okta", "/tickets/INC1")
	if loc != "/tickets/INC1" {
		t.Fatalf("after signing in: %s", loc)
	}
	mia := actor{cookies: cookies}
	me := decodeBody[struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
		Role  string `json:"role"`
	}](t, mia.do(h, "GET", "/api/v1/auth/me", ""))
	if me.Email != "mia@corp.test" || me.Name != "Mia Chen" || me.Role != "end_user" {
		t.Fatalf("the new account: %+v", me)
	}
	member := func(group, who string) bool {
		return countRows(t, pool, "SELECT count(*) FROM group_members WHERE group_id = $1 AND user_id = $2", group, who) == 1
	}
	if !member(desk.ID, me.ID) {
		t.Error("not put in the mapped group")
	}
	// An admin puts her in a group no mapping names; her provider drops sl-desk. Next sign-in: out of the desk group
	// only, and found by subject though her email changed.
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+other.ID+"/members/"+me.ID, ""), 204, "a group by hand")
	idp.Next = &ssotest.User{Subject: "00u1", Email: "mia.chen@corp.test", Name: "Mia Chen", EmailVerified: true}
	ssoSignIn(t, h, "corp-okta", "/")
	if member(desk.ID, me.ID) || !member(other.ID, me.ID) {
		t.Errorf("after the groups changed: desk %v, facilities %v", member(desk.ID, me.ID), member(other.ID, me.ID))
	}
	if n := countRows(t, pool, "SELECT count(*) FROM users WHERE email LIKE 'mia%'"); n != 1 {
		t.Errorf("%d accounts for Mia", n)
	}

	// An existing person is matched by a verified email, and keeps their role.
	idp.Next = &ssotest.User{Subject: "00u2", Email: "agent@corp.test", EmailVerified: true}
	_, cookies = ssoSignIn(t, h, "corp-okta", "/")
	if got := decodeBody[struct{ ID string }](t, actor{cookies: cookies}.do(h, "GET", "/api/v1/auth/me", "")); got.ID != agent.id {
		t.Errorf("matched to %s, not the agent", got.ID)
	}

	// Refused: an unverified email, another domain, someone deactivated, a tampered or missing state, a foreign next.
	idp.Next = &ssotest.User{Subject: "00u3", Email: "new@corp.test", EmailVerified: false}
	if loc, _ := ssoSignIn(t, h, "corp-okta", "/"); !strings.Contains(ssoError(loc), "not verified") {
		t.Errorf("an unverified email: %s", loc)
	}
	idp.Next = &ssotest.User{Subject: "00u4", Email: "x@elsewhere.test", EmailVerified: true}
	if loc, _ := ssoSignIn(t, h, "corp-okta", "/"); !strings.Contains(ssoError(loc), "elsewhere.test") {
		t.Errorf("another domain: %s", loc)
	}
	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+me.ID, `{"deactivated":true}`), 200, "deactivating Mia")
	idp.Next = &ssotest.User{Subject: "00u1", Email: "mia.chen@corp.test", EmailVerified: true}
	if loc, _ := ssoSignIn(t, h, "corp-okta", "/"); !strings.Contains(ssoError(loc), "deactivated") {
		t.Errorf("a deactivated person: %s", loc)
	}
	idp.Next = &ssotest.User{Subject: "00u2", Email: "agent@corp.test", EmailVerified: true}
	if loc, _ := ssoSignIn(t, h, "corp-okta", "/", func(q *url.Values, _ *[]*http.Cookie) { q.Set("state", "forged") }); !strings.Contains(ssoError(loc), "took too long") {
		t.Errorf("a forged state: %s", loc)
	}
	if loc, _ := ssoSignIn(t, h, "corp-okta", "/", func(_ *url.Values, c *[]*http.Cookie) { *c = nil }); !strings.Contains(ssoError(loc), "took too long") {
		t.Errorf("no state cookie: %s", loc)
	}
	if loc, _ := ssoSignIn(t, h, "corp-okta", "//evil.example/steal"); loc != "/" {
		t.Errorf("a next on another site: %s", loc)
	}
	if loc, _ := ssoSignIn(t, h, "corp-okta", "/", func(q *url.Values, _ *[]*http.Cookie) { q.Set("code", "made-up") }); ssoError(loc) == "" {
		t.Errorf("a made-up code: %s", loc)
	}

	// A provider that does not create accounts lets in only people who have one.
	expect(t, admin.do(h, "PUT", "/api/v1/sso-providers/corp-okta", strings.Replace(providerJSON(idp.Issuer, ""), `"create_users":true`, `"create_users":false`, 1)), 200, "no new accounts")
	idp.Next = &ssotest.User{Subject: "00u9", Email: "stranger@corp.test", EmailVerified: true}
	if loc, _ := ssoSignIn(t, h, "corp-okta", "/"); !strings.Contains(ssoError(loc), "no ServiceLater account for stranger@corp.test") {
		t.Errorf("an unknown person: %s", loc)
	}
}

// Passwords can be kept for admins only, once a provider is on: everyone else is sent to single sign-on.
func TestPasswordsCanBeForAdminsOnly(t *testing.T) {
	h, pool, _, _ := setupEmail(t, testKey(4))
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	newActor(t, h, pool, "agent@x.test", "agent")
	idp := startIdP(t)
	refusal(t, admin.do(h, "PUT", "/api/v1/settings/sign-in", `{"passwords":"admins"}`), 422, "switch a single sign-on provider on first", "with no provider")
	expect(t, admin.do(h, "PUT", "/api/v1/sso-providers/corp-okta", providerJSON(idp.Issuer, "")), 200, "a provider")
	expect(t, admin.do(h, "PUT", "/api/v1/settings/sign-in", `{"passwords":"admins"}`), 200, "admins only")

	refusal(t, do(h, "POST", "/api/v1/auth/login", `{"email":"agent@x.test","password":"password-agent@x.test"}`, nil), 403, "single sign-on", "an agent's password")
	refusal(t, do(h, "POST", "/api/v1/auth/login", `{"email":"agent@x.test","password":"wrong-password"}`, nil), 401, "invalid email or password", "a wrong password says nothing more")
	expect(t, do(h, "POST", "/api/v1/auth/login", `{"email":"boss@x.test","password":"password-boss@x.test"}`, nil), 200, "an admin's password")
	if opts := do(h, "GET", "/api/v1/auth/sso", "", nil).Body.String(); !strings.Contains(opts, `"passwords":"admins"`) {
		t.Errorf("the sign-in options: %s", opts)
	}
	expect(t, admin.do(h, "PUT", "/api/v1/settings/sign-in", `{"passwords":"everyone"}`), 200, "everyone again")
	expect(t, do(h, "POST", "/api/v1/auth/login", `{"email":"agent@x.test","password":"password-agent@x.test"}`, nil), 200, "an agent's password again")
}
