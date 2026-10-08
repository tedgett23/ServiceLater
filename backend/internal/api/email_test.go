package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/api"
	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/notify"
	"github.com/servicelater/servicelater/backend/internal/notify/notifytest"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, secrets.KeySize) }

// setupEmail is setupApps with a secrets key (nil: none).
func setupEmail(t *testing.T, key []byte) (http.Handler, *pgxpool.Pool, *apps.Registry, *secrets.Keyring) {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.InstallFirstParty(t, pool)
	reg := apps.NewRegistry(pool)
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := auth.NewTokens("0123456789abcdef0123456789abcdef", time.Minute)
	svc := auth.NewService(pool, tk, time.Hour)
	if _, err := svc.Bootstrap(context.Background(), "admin@example.com", "Admin", "hunter2hunter2"); err != nil {
		t.Fatal(err)
	}
	return api.NewRouter(api.Deps{Pool: pool, Auth: svc, Tokens: tk, Apps: reg, Keys: keys}), pool, reg, keys
}

type emailSettingsBody struct {
	settings.Email
	PasswordSet      bool `json:"password_set"`
	PasswordReadable bool `json:"password_readable"`
	KeyConfigured    bool `json:"key_configured"`
}

func emailJSON(srv *notifytest.Server, enabled bool, extra string) string {
	return fmt.Sprintf(`{"enabled":%t,"host":%q,"port":%d,"tls":"none","username":"desk","from_address":"desk@example.com",`+
		`"from_name":"Service Desk","base_url":"https://sl.example.com","approvals":true%s}`, enabled, srv.Host, srv.Port, extra)
}

// The mail server's password is encrypted before it is stored, is never read back through the API, and cannot be
// read by the app's database role at all.
func TestTheMailPasswordIsEncryptedAndNeverReadBack(t *testing.T) {
	h, pool, _, keys := setupEmail(t, testKey(1))
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	srv := notifytest.Start(t, notifytest.Options{Username: "desk", Password: "correct horse"})

	expect(t, agent.do(h, "GET", "/api/v1/settings/email", ""), 403, "an agent reading email settings")
	def := decodeBody[emailSettingsBody](t, admin.do(h, "GET", "/api/v1/settings/email", ""))
	if def.Port != 587 || def.TLS != "starttls" || !def.Approvals || def.Enabled || def.PasswordSet || !def.KeyConfigured {
		t.Errorf("defaults: %+v", def)
	}
	for body, want := range map[string]string{
		`{"host":"smtp.example.com","port":25,"tls":"none"}`:                                      "only allowed to a server on this machine",
		`{"enabled":true,"port":587,"tls":"starttls"}`:                                            "turning email on needs a host and a from address",
		`{"host":"smtp.example.com","port":587,"tls":"starttls","from_address":"Desk <d@x.com>"}`: "from_address must be an email address",
		`{"host":"smtp.example.com","port":587,"tls":"maybe"}`:                                    "tls must be starttls, tls or none",
		`{"host":"smtp.example.com","port":587,"tls":"starttls","base_url":"ftp://x"}`:            "base_url must be",
	} {
		refusal(t, admin.do(h, "PUT", "/api/v1/settings/email", body), 422, want, body)
	}

	w := admin.do(h, "PUT", "/api/v1/settings/email", emailJSON(srv, true, `,"password":"correct horse"`))
	saved := decodeBody[emailSettingsBody](t, w)
	if !saved.PasswordSet || !saved.PasswordReadable || saved.Host != srv.Host || strings.Contains(w.Body.String(), "correct horse") {
		t.Fatalf("saved: %s", w.Body)
	}
	if w := admin.do(h, "GET", "/api/v1/settings/email", ""); strings.Contains(w.Body.String(), "correct horse") {
		t.Errorf("the password is read back: %s", w.Body)
	}
	// Leaving the password out keeps it.
	expect(t, admin.do(h, "PUT", "/api/v1/settings/email", emailJSON(srv, true, "")), 200, "saving without the password")

	ctx := context.Background()
	err := db.WithUser(ctx, pool, uuid.MustParse(admin.id), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT ciphertext FROM secrets")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the app role reads secrets, even as an admin: %v", err)
	}
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, err := q.GetSecret(ctx, settings.EmailPassword)
		if err != nil {
			return err
		}
		if bytes.Contains(row.Ciphertext, []byte("correct horse")) || row.KeyID != secrets.KeyID(testKey(1)) {
			t.Errorf("stored in the clear, or under another key: %+v", row)
		}
		got, err := secrets.Get(ctx, q, keys, settings.EmailPassword)
		if got != "correct horse" {
			t.Errorf("decrypted: %q", got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The test email signs in with it.
	res := decodeBody[struct{ Status, Detail string }](t, admin.do(h, "POST", "/api/v1/settings/email/test", `{}`))
	if res.Status != "sent" {
		t.Fatalf("test email: %+v", res)
	}
	if got := srv.Wait(t, 1)[0]; got.To[0] != "boss@x.test" || !strings.Contains(got.Data, "ServiceLater test email") {
		t.Errorf("test email: %+v", got)
	}
	expect(t, agent.do(h, "POST", "/api/v1/settings/email/test", `{}`), 403, "an agent sending a test email")

	// Rotating the key re-encrypts it; afterwards the new key alone reads it.
	rotated, _ := secrets.NewKeyring(testKey(2), testKey(1))
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		n, err := secrets.Rotate(ctx, dbgen.New(tx), rotated)
		if n != 1 {
			t.Errorf("rotated %d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	only, _ := secrets.NewKeyring(testKey(2))
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		got, err := secrets.Get(ctx, dbgen.New(tx), only, settings.EmailPassword)
		if got != "correct horse" {
			t.Errorf("after rotating: %q", got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// This server still has only the old key: it says the password must be set again rather than sending without it.
	if s := decodeBody[emailSettingsBody](t, admin.do(h, "GET", "/api/v1/settings/email", "")); !s.PasswordSet || s.PasswordReadable {
		t.Errorf("a password under a key this server lacks: %+v", s)
	}
	res = decodeBody[struct{ Status, Detail string }](t, admin.do(h, "POST", "/api/v1/settings/email/test", `{}`))
	if res.Status != "failed" || !strings.Contains(res.Detail, "not configured") {
		t.Errorf("a test email with an unreadable password: %+v", res)
	}

	expect(t, admin.do(h, "PUT", "/api/v1/settings/email", emailJSON(srv, false, `,"password":null`)), 200, "clearing the password")
	if s := decodeBody[emailSettingsBody](t, admin.do(h, "GET", "/api/v1/settings/email", "")); s.PasswordSet {
		t.Errorf("cleared: %+v", s)
	}
}

// A server with no secrets key runs, but refuses to store a password rather than storing it in the clear.
func TestWithoutAKeyNoPasswordIsStored(t *testing.T) {
	h, pool, _, _ := setupEmail(t, nil)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	srv := notifytest.Start(t, notifytest.Options{})
	if s := decodeBody[emailSettingsBody](t, admin.do(h, "GET", "/api/v1/settings/email", "")); s.KeyConfigured {
		t.Errorf("no key: %+v", s)
	}
	refusal(t, admin.do(h, "PUT", "/api/v1/settings/email", emailJSON(srv, true, `,"password":"x"`)), 422, "no secrets key", "a password with no key")
	expect(t, admin.do(h, "PUT", "/api/v1/settings/email", emailJSON(srv, true, "")), 200, "settings without a password")
}

// sendQueued runs the email job for every queued delivery, as the worker would.
func sendQueued(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry, keys *secrets.Keyring) {
	t.Helper()
	ctx := context.Background()
	var ids []uuid.UUID
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT id FROM email_deliveries WHERE status = 'queued' ORDER BY created_at")
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m := &notify.Mailer{Pool: pool, Apps: reg, Keys: keys}
	for _, id := range ids {
		payload, _ := json.Marshal(map[string]any{"delivery_id": id})
		if err := m.Handler()(ctx, jobs.Job{Kind: notify.EmailKind, Payload: payload, Attempt: 1, MaxAttempts: 5}); err != nil {
			t.Errorf("sending %s: %v", id, err)
		}
	}
}

type deliveryBody struct {
	ToAddress string `json:"to_address"`
	Kind      string `json:"kind"`
	What      string `json:"what"`
	Status    string `json:"status"`
	Detail    string `json:"detail"`
}

// Notifications whose template asks for email are also emailed, filled in as the recipient sees the record, unless
// the person turned email off, email is off, or they read it in the app first.
func TestNotificationsAreEmailed(t *testing.T) {
	h, pool, reg, keys := setupEmail(t, testKey(1))
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	priya := newActor(t, h, pool, "priya@x.test", "agent")
	srv := notifytest.Start(t, notifytest.Options{Username: "desk", Password: "pw"})
	expect(t, admin.do(h, "PUT", "/api/v1/settings/email", emailJSON(srv, true, `,"password":"pw"`)), 200, "turning email on")

	assign := func(title string) typedTicket {
		tk := decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"`+title+`"}`))
		expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignee_id":"`+priya.id+`"}`), 200, "assigning "+title)
		return tk
	}
	first := assign("Printer on fire")
	sendQueued(t, pool, reg, keys)
	got := srv.Wait(t, 1)[0]
	for _, want := range []string{"Subject: " + first.Reference + " was assigned to you", "Printer on fire",
		"https://sl.example.com/tickets/" + first.Reference, "https://sl.example.com/account"} {
		if !strings.Contains(got.Data, want) {
			t.Errorf("no %q in\n%s", want, got.Data)
		}
	}
	if got.To[0] != "priya@x.test" {
		t.Errorf("to %v", got.To)
	}

	// Read in the app before the email went out: it is not sent.
	second := assign("Scanner jammed")
	expect(t, priya.do(h, "POST", "/api/v1/notifications/read", `{"task_id":"`+second.ID+`"}`), 200, "reading it in the app")
	sendQueued(t, pool, reg, keys)

	// Priya turns email off for herself: notifications still arrive in the app, not by email.
	acct := decodeBody[struct {
		EmailNotifications bool `json:"email_notifications"`
		EmailEnabled       bool `json:"email_enabled"`
	}](t, priya.do(h, "PATCH", "/api/v1/account", `{"email_notifications":false}`))
	if acct.EmailNotifications || !acct.EmailEnabled {
		t.Errorf("account: %+v", acct)
	}
	assign("Monitor flickers")
	sendQueued(t, pool, reg, keys)
	if n := len(srv.Messages()); n != 1 {
		t.Errorf("emails sent: %d, want only the first", n)
	}

	log := decodeBody[struct{ Items []deliveryBody }](t, admin.do(h, "GET", "/api/v1/settings/email/deliveries", "")).Items
	var seen []string
	for _, d := range log {
		seen = append(seen, d.What+":"+d.Status)
	}
	if strings.Join(seen, ",") != "ticket-assigned:skipped,ticket-assigned:sent" || !strings.Contains(log[0].Detail, "read in ServiceLater") {
		t.Errorf("the delivery log: %v %+v", seen, log)
	}
	expect(t, agent.do(h, "GET", "/api/v1/settings/email/deliveries", ""), 403, "an agent reading the delivery log")
}

// A refusal from the mail server fails the delivery at once, with what the server said; email turned off queues nothing.
func TestARefusedEmailIsLoggedAndEmailOffQueuesNothing(t *testing.T) {
	h, pool, reg, keys := setupEmail(t, testKey(1))
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	priya := newActor(t, h, pool, "priya@x.test", "agent")
	srv := notifytest.Start(t, notifytest.Options{})
	srv.Reject("priya@x.test")
	expect(t, admin.do(h, "PUT", "/api/v1/settings/email", strings.Replace(emailJSON(srv, true, ""), `"username":"desk"`, `"username":""`, 1)), 200, "turning email on")
	tk := decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Bounces"}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignee_id":"`+priya.id+`"}`), 200, "assigning it")

	ctx := context.Background()
	var id uuid.UUID
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT id FROM email_deliveries").Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"delivery_id": id})
	err := (&notify.Mailer{Pool: pool, Apps: reg, Keys: keys}).Handler()(ctx, jobs.Job{Payload: payload, Attempt: 1, MaxAttempts: 5})
	if !jobs.IsPermanent(err) {
		t.Errorf("a refusal is not retried: %v", err)
	}
	log := decodeBody[struct{ Items []deliveryBody }](t, admin.do(h, "GET", "/api/v1/settings/email/deliveries", "")).Items
	if len(log) != 1 || log[0].Status != "failed" || !strings.Contains(log[0].Detail, "no such mailbox") {
		t.Errorf("the log: %+v", log)
	}

	expect(t, admin.do(h, "PUT", "/api/v1/settings/email", emailJSON(srv, false, "")), 200, "turning email off")
	other := decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Quiet"}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+other.ID, `{"assignee_id":"`+priya.id+`"}`), 200, "assigning it")
	if log := decodeBody[struct{ Items []deliveryBody }](t, admin.do(h, "GET", "/api/v1/settings/email/deliveries", "")).Items; len(log) != 1 {
		t.Errorf("email off queued something: %+v", log)
	}
}

// A pending approval is emailed too, unless the approvals setting is off.
func TestApprovalRequestsAreEmailedUnlessTurnedOff(t *testing.T) {
	h, pool, _, _ := setupEmail(t, testKey(1))
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	srv := notifytest.Start(t, notifytest.Options{})
	ctx := context.Background()
	ask := func() int {
		var n int
		if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "INSERT INTO notifications (user_id, kind) VALUES ($1, 'approval_pending')", lena.id); err != nil {
				return err
			}
			return tx.QueryRow(ctx, "SELECT count(*) FROM email_deliveries WHERE what = 'approval_pending'").Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	expect(t, admin.do(h, "PUT", "/api/v1/settings/email", emailJSON(srv, true, "")), 200, "turning email on")
	if n := ask(); n != 1 {
		t.Errorf("an approval request is emailed: %d", n)
	}
	expect(t, admin.do(h, "PUT", "/api/v1/settings/email", strings.Replace(emailJSON(srv, true, ""), `"approvals":true`, `"approvals":false`, 1)), 200, "turning approval emails off")
	if n := ask(); n != 1 {
		t.Errorf("approval emails off: %d", n)
	}
}
