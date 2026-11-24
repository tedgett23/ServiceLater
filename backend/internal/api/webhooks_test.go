package api_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

const webhookApp = "apiVersion: 1\nname: hooks\nversion: 1.0.0\nauthor: t\ndescription: \"Hooks.\"\n" +
	"permissions: [records:read:incident]\nconfig:\n  - { key: alert_token, label: Alert token, type: secret }\n  - { key: signing, label: Signing secret, type: secret }\n"

const tokenHook = "kind: workflow\nname: alert\nrecord_type: incident\nstart: e\n" +
	"webhook:\n  secret: alert_token\n  auth: token\nsteps:\n  e:\n    type: end\n"

const signedHook = "kind: workflow\nname: push\nrecord_type: incident\nstart: e\n" +
	"webhook:\n  secret: signing\n  auth: hmac\n  signature_header: X-Hub-Signature-256\n  dedupe_header: X-GitHub-Delivery\nsteps:\n  e:\n    type: end\n"

const plainFlow = "kind: workflow\nname: plain\nrecord_type: incident\nstart: e\nsteps:\n  e:\n    type: end\n"

// call sends a request with no session, as another system would.
func call(h http.Handler, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(context.Background(), "POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func runOf(t *testing.T, w *httptest.ResponseRecorder) (uuid.UUID, bool) {
	t.Helper()
	var out struct {
		RunID     uuid.UUID `json:"run_id"`
		Duplicate bool      `json:"duplicate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.RunID == uuid.Nil {
		t.Fatalf("no run in %d %s", w.Code, w.Body)
	}
	return out.RunID, out.Duplicate
}

// A webhook starts its workflow as the app, for nobody, with the body as vars.payload, once its secret is set and
// only for a caller that knows it: as a bearer token or as a signature of the body. A repeated delivery gets the
// first run back. Every call is logged without its body, for those who may read runs.
func TestAWebhookStartsItsWorkflow(t *testing.T) {
	h, pool, reg, _ := setupEmail(t, testKey(7))
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	user := newActor(t, h, pool, "user@x.test", "end_user")
	installHooks(t, pool, reg)

	expect(t, call(h, "/api/v1/hooks/hooks/nope", `{}`, nil), 404, "no such workflow")
	expect(t, call(h, "/api/v1/hooks/hooks/plain", `{}`, nil), 404, "a workflow with no webhook")
	expect(t, call(h, "/api/v1/hooks/hooks/alert", `{}`, map[string]string{"Authorization": "Bearer anything"}), 401, "before the secret is set")

	expect(t, admin.do(h, "PUT", "/api/v1/connections/hooks/secrets/alert_token", `{"value":"t0ken"}`), 204, "setting the token")
	expect(t, admin.do(h, "PUT", "/api/v1/connections/hooks/secrets/signing", `{"value":"s3cret"}`), 204, "setting the signing secret")

	// A token.
	expect(t, call(h, "/api/v1/hooks/hooks/alert", `{}`, nil), 401, "no token")
	expect(t, call(h, "/api/v1/hooks/hooks/alert", `{}`, map[string]string{"Authorization": "Bearer wrong"}), 401, "a wrong token")
	expect(t, call(h, "/api/v1/hooks/hooks/alert", `not json`, map[string]string{"Authorization": "Bearer t0ken"}), 400, "a body that is not JSON")
	expect(t, call(h, "/api/v1/hooks/hooks/alert", `{"a":"`+strings.Repeat("x", apps.MaxWebhookBody)+`"}`, map[string]string{"Authorization": "Bearer t0ken"}), 413, "a body too large")
	w := call(h, "/api/v1/hooks/hooks/alert", `{"title":"disk full","count":3}`, map[string]string{"Authorization": "Bearer t0ken"})
	expect(t, w, 202, "the right token")
	run, _ := runOf(t, w)
	var startedBy, runAs uuid.UUID
	var record *uuid.UUID
	var title string
	var count float64
	if err := pool.QueryRow(context.Background(), `SELECT started_by, run_as, record_id, vars->'payload'->>'title', (vars->'payload'->>'count')::float8
		FROM workflow_executions WHERE id = $1`, run).Scan(&startedBy, &runAs, &record, &title, &count); err != nil {
		t.Fatal(err)
	}
	var principal uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id FROM users WHERE app_name = 'hooks'`).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if startedBy != principal || runAs != principal || record != nil || title != "disk full" || count != 3 {
		t.Errorf("the run: started by %v, as %v (the app is %v), record %v, payload %q %v", startedBy, runAs, principal, record, title, count)
	}

	// A signature, with or without its sha256= prefix; a changed body fails.
	body := `{"ref":"main"}`
	expect(t, call(h, "/api/v1/hooks/hooks/push", body, map[string]string{"X-Hub-Signature-256": "sha256=" + sign("wrong", body)}), 401, "signed with the wrong secret")
	expect(t, call(h, "/api/v1/hooks/hooks/push", `{"ref":"evil"}`, map[string]string{"X-Hub-Signature-256": "sha256=" + sign("s3cret", body)}), 401, "a changed body")
	expect(t, call(h, "/api/v1/hooks/hooks/push", body, map[string]string{"Authorization": "Bearer s3cret"}), 401, "a token where a signature is wanted")
	expect(t, call(h, "/api/v1/hooks/hooks/push", body, map[string]string{"X-Hub-Signature-256": sign("s3cret", body)}), 202, "a signature without its prefix")
	first := call(h, "/api/v1/hooks/hooks/push", body, map[string]string{"X-Hub-Signature-256": "sha256=" + sign("s3cret", body), "X-GitHub-Delivery": "d-1"})
	expect(t, first, 202, "a signed delivery")
	again := call(h, "/api/v1/hooks/hooks/push", body, map[string]string{"X-Hub-Signature-256": "sha256=" + sign("s3cret", body), "X-GitHub-Delivery": "d-1"})
	expect(t, again, 200, "the same delivery again")
	a, _ := runOf(t, first)
	b, dup := runOf(t, again)
	if a != b || !dup {
		t.Errorf("a repeated delivery started %v (duplicate %v), the first %v", b, dup, a)
	}
	var runs int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM workflow_executions WHERE app = 'hooks' AND workflow = 'push'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Errorf("%d push runs, want 2 (one unprefixed, one delivery)", runs)
	}

	// The log.
	expect(t, user.do(h, "GET", "/api/v1/workflows/hooks/push/deliveries", ""), 403, "an end user")
	type delivery struct {
		DeliveryKey string     `json:"delivery_key"`
		Outcome     string     `json:"outcome"`
		Reason      string     `json:"reason"`
		RunID       *uuid.UUID `json:"run_id"`
	}
	log := decodeBody[[]delivery](t, agent.do(h, "GET", "/api/v1/workflows/hooks/push/deliveries", ""))
	var outcomes []string
	for _, d := range log {
		outcomes = append(outcomes, d.Outcome)
	}
	if strings.Join(outcomes, ",") != "duplicate,started,started,refused,refused,refused" {
		t.Errorf("push deliveries, newest first: %v", outcomes)
	}
	if log[0].DeliveryKey != "d-1" || log[0].RunID == nil || *log[0].RunID != a || log[3].Reason != "the X-Hub-Signature-256 header with the body's signature is required" || log[4].Reason != "the signature does not match the body" {
		t.Errorf("the log: %+v", log)
	}
	alertLog := decodeBody[[]delivery](t, agent.do(h, "GET", "/api/v1/workflows/hooks/alert/deliveries", ""))
	if len(alertLog) != 6 || !strings.Contains(alertLog[len(alertLog)-1].Reason, "secret is not set") {
		t.Errorf("alert deliveries: %+v", alertLog)
	}

	// The builder says it has a webhook and that its secret is set.
	def := agent.do(h, "GET", "/api/v1/workflows/hooks/push", "").Body.String()
	if !strings.Contains(def, `"auth":"hmac"`) || !strings.Contains(def, `"webhook_secret_set":true`) {
		t.Errorf("the builder's definition: %s", def)
	}

	// A flood is turned away.
	if _, err := pool.Exec(context.Background(), `UPDATE rate_limits SET count = 1000 WHERE key = 'webhook:hooks/alert'`); err != nil {
		t.Fatal(err)
	}
	expect(t, call(h, "/api/v1/hooks/hooks/alert", `{}`, map[string]string{"Authorization": "Bearer t0ken"}), 429, "too many calls")
}

func installHooks(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry) {
	t.Helper()
	c, err := (&apps.Bundle{Files: map[string][]byte{"app.yaml": []byte(webhookApp), "default/workflows/alert.yaml": []byte(tokenHook),
		"default/workflows/push.yaml": []byte(signedHook), "default/workflows/plain.yaml": []byte(plainFlow)}}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Install(context.Background(), pool, c, apps.InstallOptions{ApprovePermissions: true}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
}
