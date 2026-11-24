package api_test

import (
	"strings"
	"testing"
)

// Admin → Rate limits: only those who manage settings read or change them; out-of-range values are refused, and what
// a change leaves out stays as it was. The API limit then holds: a person, or a key, over it gets 429 with
// Retry-After, every counted response says where they stand, and a key may have its own limit (0: none). Signing in
// and webhooks follow their settings too.
func TestRateLimitsAreSetInTheAdminPages(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	expect(t, agent.do(h, "GET", "/api/v1/settings/limits", ""), 403, "an agent reading them")
	expect(t, agent.do(h, "PUT", "/api/v1/settings/limits", `{"api_people_per_minute":0}`), 403, "an agent lifting them")
	type limits struct {
		APIPeoplePerMinute int `json:"api_people_per_minute"`
		APIKeysPerMinute   int `json:"api_keys_per_minute"`
		SignInAttempts     int `json:"sign_in_attempts"`
		Defaults           struct {
			APIPeoplePerMinute int `json:"api_people_per_minute"`
		} `json:"defaults"`
	}
	got := decodeBody[limits](t, admin.do(h, "GET", "/api/v1/settings/limits", ""))
	if got.APIPeoplePerMinute != 1200 || got.Defaults.APIPeoplePerMinute != 1200 {
		t.Fatalf("the defaults: %+v", got)
	}
	refusal(t, admin.do(h, "PUT", "/api/v1/settings/limits", `{"sign_in_attempts":0}`), 422, "sign_in_attempts", "no sign-in attempts at all")
	refusal(t, admin.do(h, "PUT", "/api/v1/settings/limits", `{"api_keys_per_minute":5}`), 422, "or 0 for no limit", "a key limit too low")
	w := admin.do(h, "PUT", "/api/v1/settings/limits", `{"api_people_per_minute":12,"sign_in_attempts":3}`)
	expect(t, w, 200, "changing two")
	if got = decodeBody[limits](t, w); got.APIPeoplePerMinute != 12 || got.SignInAttempts != 3 || got.APIKeysPerMinute != 1200 {
		t.Fatalf("after the change: %+v", got)
	}

	// A person who has made no requests yet this minute.
	fresh := newActor(t, h, pool, "fresh@x.test", "agent")
	for i := range 12 {
		w := fresh.do(h, "GET", "/api/v1/tickets", "")
		expect(t, w, 200, "within the limit")
		if left := w.Header().Get("RateLimit-Remaining"); i == 11 && left != "0" {
			t.Errorf("the twelfth leaves %q", left)
		}
		if w.Header().Get("RateLimit-Limit") != "12" {
			t.Fatalf("RateLimit-Limit %q", w.Header().Get("RateLimit-Limit"))
		}
	}
	over := fresh.do(h, "GET", "/api/v1/tickets", "")
	refusal(t, over, 429, "too many requests this minute", "the thirteenth")
	if over.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}

	// A key follows the setting for keys unless it has its own.
	w = admin.do(h, "POST", "/api/v1/service-accounts", `{"name":"Monitoring","role":"agent"}`)
	expect(t, w, 201, "a service account")
	sa := decodeBody[struct{ ID string }](t, w)
	w = admin.do(h, "POST", "/api/v1/users/"+sa.ID+"/api-keys", `{"name":"Alerts"}`)
	expect(t, w, 201, "a key")
	key := decodeBody[keyBody](t, w)
	if w := withKey(h, key.Key, "GET", "/api/v1/tickets", ""); w.Code != 200 || w.Header().Get("RateLimit-Limit") != "1200" {
		t.Fatalf("a key under the setting: %d, limit %q", w.Code, w.Header().Get("RateLimit-Limit"))
	}
	path := "/api/v1/users/" + sa.ID + "/api-keys/" + key.ID
	expect(t, agent.do(h, "PATCH", path, `{"rate_limit":10}`), 403, "an agent changing a key")
	refusal(t, admin.do(h, "PATCH", path, `{"rate_limit":3}`), 422, "between 10 and 1000000", "too low")
	expect(t, admin.do(h, "PATCH", path, `{"rate_limit":0}`), 204, "no limit for this key")
	if w := withKey(h, key.Key, "GET", "/api/v1/tickets", ""); w.Code != 200 || w.Header().Get("RateLimit-Limit") != "" {
		t.Fatalf("a key with no limit: %d, limit %q", w.Code, w.Header().Get("RateLimit-Limit"))
	}
	list := admin.do(h, "GET", "/api/v1/users/"+sa.ID+"/api-keys", "").Body.String()
	if !strings.Contains(list, `"rate_limit":0`) {
		t.Errorf("the key list: %s", list)
	}
	expect(t, admin.do(h, "PATCH", path, `{"rate_limit":null}`), 204, "back to the setting")

	// Signing in follows its setting: three tries.
	for range 3 {
		expect(t, do(h, "POST", "/api/v1/auth/login", `{"email":"agent@x.test","password":"wrong-password"}`, nil), 401, "a wrong password")
	}
	refusal(t, do(h, "POST", "/api/v1/auth/login", `{"email":"agent@x.test","password":"password-agent@x.test"}`, nil), 429, "", "a fourth try, right or not")
}
