package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/api"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
)

func setup(t *testing.T) (http.Handler, *pgxpool.Pool) {
	pool := dbtest.Pool(t)
	tk, _ := auth.NewTokens("0123456789abcdef0123456789abcdef", time.Minute)
	svc := auth.NewService(pool, tk, time.Hour)
	if _, err := svc.Bootstrap(context.Background(), "admin@example.com", "Admin", "hunter2hunter2"); err != nil {
		t.Fatal(err)
	}
	return api.NewRouter(api.Deps{Pool: pool, Auth: svc, Tokens: tk}), pool
}

func do(h http.Handler, method, path, body string, cookies []*http.Cookie, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	req.Host = "sl.test"
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func respCookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

//nolint:gosec // test fixture, not a real credential
const creds = `{"email":"admin@example.com","password":"hunter2hunter2"}`

func TestLoginMeRefreshLogout(t *testing.T) {
	h, _ := setup(t)

	if w := do(h, "GET", "/api/v1/auth/me", "", nil); w.Code != 401 {
		t.Errorf("me without cookie: %d", w.Code)
	}
	if w := do(h, "POST", "/api/v1/auth/login", `{"email":"admin@example.com","password":"bad"}`, nil); w.Code != 401 {
		t.Errorf("bad login: %d", w.Code)
	}

	w := do(h, "POST", "/api/v1/auth/login", creds, nil)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	access, refresh := respCookie(w, auth.AccessCookie), respCookie(w, auth.RefreshCookie)
	for _, c := range []*http.Cookie{access, refresh} {
		if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
			t.Fatalf("cookie flags wrong: %+v", c)
		}
	}
	if refresh.Path != "/api/v1/auth" {
		t.Errorf("refresh cookie path %q", refresh.Path)
	}
	if strings.Contains(w.Body.String(), "password") {
		t.Error("login response leaks credentials")
	}

	w = do(h, "GET", "/api/v1/auth/me", "", []*http.Cookie{access})
	var me auth.User
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || me.Email != "admin@example.com" || me.Role != "admin" {
		t.Fatalf("me: %d %s", w.Code, w.Body)
	}

	w = do(h, "POST", "/api/v1/auth/refresh", "", []*http.Cookie{refresh})
	if w.Code != 200 || respCookie(w, auth.RefreshCookie).Value == refresh.Value {
		t.Fatalf("refresh: %d", w.Code)
	}
	newRefresh := respCookie(w, auth.RefreshCookie)

	w = do(h, "POST", "/api/v1/auth/logout", "", []*http.Cookie{newRefresh})
	if w.Code != 204 || respCookie(w, auth.AccessCookie).MaxAge >= 0 {
		t.Errorf("logout: %d", w.Code)
	}
	if w := do(h, "POST", "/api/v1/auth/refresh", "", []*http.Cookie{newRefresh}); w.Code != 401 {
		t.Errorf("refresh after logout: %d", w.Code)
	}
}

func TestCrossOriginRejected(t *testing.T) {
	h, _ := setup(t)
	if w := do(h, "POST", "/api/v1/auth/login", creds, nil, "Origin", "https://evil.example"); w.Code != 403 {
		t.Errorf("cross-origin POST: %d", w.Code)
	}
	if w := do(h, "POST", "/api/v1/auth/login", creds, nil, "Origin", "https://sl.test"); w.Code != 200 {
		t.Errorf("same-origin POST: %d", w.Code)
	}
}

func TestBadBodies(t *testing.T) {
	h, _ := setup(t)
	for _, body := range []string{``, `{`, `{"email":"a","password":"b","admin":true}`} {
		if w := do(h, "POST", "/api/v1/auth/login", body, nil); w.Code != 400 {
			t.Errorf("body %q: %d", body, w.Code)
		}
	}
}

func TestDeletedUserLosesAccess(t *testing.T) {
	h, pool := setup(t)
	w := do(h, "POST", "/api/v1/auth/login", creds, nil)
	access, refresh := respCookie(w, auth.AccessCookie), respCookie(w, auth.RefreshCookie)

	if _, err := pool.Exec(context.Background(), "DELETE FROM users"); err != nil { // sessions cascade
		t.Fatal(err)
	}
	if w := do(h, "POST", "/api/v1/auth/refresh", "", []*http.Cookie{refresh}); w.Code != 401 {
		t.Errorf("refresh for deleted user: %d", w.Code)
	}
	// The access JWT is still cryptographically valid, but /me goes through RLS-scoped DB access and finds no user.
	if w := do(h, "GET", "/api/v1/auth/me", "", []*http.Cookie{access}); w.Code != 401 {
		t.Errorf("me for deleted user: %d", w.Code)
	}
}
