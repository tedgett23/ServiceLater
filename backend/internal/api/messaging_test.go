package api_test

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/api"
	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
	"github.com/servicelater/servicelater/backend/internal/notify"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

type texted struct{ to, body string }

// fakeSMS records the texts it is asked to send.
type fakeSMS struct {
	mu   sync.Mutex
	sent []texted
	fail error
}

func (f *fakeSMS) Send(_ context.Context, cfg settings.SMS, token, to, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	if cfg.AccountSID == "" || token == "" {
		return errors.New("not configured")
	}
	f.sent = append(f.sent, texted{to, body})
	return nil
}

func (f *fakeSMS) last(t *testing.T) texted {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		t.Fatal("nothing was texted")
	}
	return f.sent[len(f.sent)-1]
}

type posted struct {
	webhook string
	post    notify.TeamsPost
}

// fakeTeams records the posts it is asked to make.
type fakeTeams struct {
	mu    sync.Mutex
	posts []posted
}

func (f *fakeTeams) Post(_ context.Context, webhook string, p notify.TeamsPost) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, posted{webhook, p})
	return nil
}

// setupMessaging is setupApps with a secrets key and fake SMS and Teams senders.
func setupMessaging(t *testing.T) (http.Handler, *pgxpool.Pool, *apps.Registry, *secrets.Keyring, *fakeSMS, *fakeTeams) {
	t.Helper()
	pool := dbtest.Pool(t)
	dbtest.InstallFirstParty(t, pool)
	reg := apps.NewRegistry(pool)
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.NewKeyring(testKey(5))
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := auth.NewTokens("0123456789abcdef0123456789abcdef", time.Minute)
	svc := auth.NewService(pool, tk, time.Hour)
	if _, err := svc.Bootstrap(context.Background(), "admin@example.com", "Admin", "hunter2hunter2"); err != nil {
		t.Fatal(err)
	}
	sms, teams := &fakeSMS{}, &fakeTeams{}
	return api.NewRouter(api.Deps{Pool: pool, Auth: svc, Tokens: tk, Apps: reg, Keys: keys, SMS: sms, Teams: teams}), pool, reg, keys, sms, teams
}

const smsJSON = `{"enabled":true,"account_sid":"` + testSID + `","from":"+15550100000","auth_token":"tok"}`

var codeRE = regexp.MustCompile(`\b(\d{6})\b`)

// verifyPhone sets someone's number and types back the code texted to it.
func verifyPhone(t *testing.T, h http.Handler, a actor, sms *fakeSMS, number string) {
	t.Helper()
	expect(t, a.do(h, "PUT", "/api/v1/account/phone", `{"number":"`+number+`"}`), 200, "setting a number")
	code := codeRE.FindString(sms.last(t).body)
	expect(t, a.do(h, "POST", "/api/v1/account/phone/verify", `{"code":"`+code+`"}`), 200, "verifying it")
}

// A person adds a mobile number, which counts only once they type back the code texted to it; SMS is then theirs to
// turn off and on. Codes are limited, and a wrong one is refused.
func TestAPhoneIsVerifiedBeforeItIsTexted(t *testing.T) {
	h, pool, _, _, sms, _ := setupMessaging(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	refusal(t, agent.do(h, "PUT", "/api/v1/settings/sms", smsJSON), 403, "", "an agent setting up SMS")
	refusal(t, admin.do(h, "PUT", "/api/v1/settings/sms", `{"enabled":true,"account_sid":"`+testSID+`","from":"+15550100000"}`), 422, "auth token", "SMS on with no token")
	w := admin.do(h, "PUT", "/api/v1/settings/sms", smsJSON)
	expect(t, w, 200, "setting up SMS")
	if b := w.Body.String(); strings.Contains(b, `"tok"`) || !strings.Contains(b, `"token_set":true`) {
		t.Errorf("the SMS settings: %s", b)
	}
	if acct := agent.do(h, "GET", "/api/v1/account", "").Body.String(); !strings.Contains(acct, `"sms_enabled":true`) {
		t.Errorf("the account does not say SMS is available: %s", acct)
	}

	refusal(t, agent.do(h, "PUT", "/api/v1/account/phone", `{"number":"555-0101"}`), 422, "international form", "a local number")
	expect(t, agent.do(h, "PUT", "/api/v1/account/phone", `{"number":"+1 (555) 010-1234"}`), 200, "setting a number")
	got := sms.last(t)
	code := codeRE.FindString(got.body)
	if got.to != "+15550101234" || code == "" {
		t.Fatalf("the code text: %+v", got)
	}
	refusal(t, agent.do(h, "PATCH", "/api/v1/account", `{"sms_notifications":true}`), 422, "verify a mobile number first", "SMS on before verifying")
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	refusal(t, agent.do(h, "POST", "/api/v1/account/phone/verify", `{"code":"`+wrong+`"}`), 422, "wrong or has expired", "a wrong code")
	w = agent.do(h, "POST", "/api/v1/account/phone/verify", `{"code":"`+code+`"}`)
	expect(t, w, 200, "the right code")
	if b := w.Body.String(); !strings.Contains(b, `"phone":"+15550101234"`) || !strings.Contains(b, `"phone_verified":true`) || !strings.Contains(b, `"sms_notifications":true`) {
		t.Errorf("after verifying: %s", b)
	}
	expect(t, agent.do(h, "PATCH", "/api/v1/account", `{"sms_notifications":false}`), 200, "turning SMS off")
	if n := countRows(t, pool, "SELECT count(*) FROM user_phones WHERE user_id = $1 AND sms_notifications", agent.id); n != 0 {
		t.Error("SMS is still on")
	}
	if n := countRows(t, pool, "SELECT count(*) FROM channel_deliveries WHERE kind = 'verification' AND status = 'sent'"); n != 1 {
		t.Errorf("%d verification texts logged", n)
	}
	for i := 0; i < 4; i++ {
		expect(t, agent.do(h, "PUT", "/api/v1/account/phone", `{"number":"+15550101234"}`), 200, "another code")
	}
	refusal(t, agent.do(h, "PUT", "/api/v1/account/phone", `{"number":"+15550101234"}`), 429, "too many codes", "a sixth code in an hour")
}

// A notification whose template asks for SMS is texted to its recipient when they verified a number and SMS is on,
// worded as they would read it in the app, with a link; one read before it goes is not sent.
func TestNotificationsAreTexted(t *testing.T) {
	h, pool, reg, keys, sms, _ := setupMessaging(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tomas := newActor(t, h, pool, "tomas@x.test", "agent")
	installNotifyApp(t, pool, reg, map[string]string{
		"notifications/urgent.yaml": "kind: notification\nname: urgent\ntitle: \"{{record.reference}} is urgent\"\nbody: \"{{record.title}}\"\nchannels: [in_app, sms]\n",
		"rules/urgent.yaml": "kind: rule\nname: urgent\nrecord_type: incident\ntiming: after\non: [update]\nwhen: record.priority == 1 && old.priority != 1\n" +
			"run:\n  - action: notify\n    template: urgent\n    to: [assignee]\n",
	})
	expect(t, admin.do(h, "PUT", "/api/v1/settings/sms", smsJSON), 200, "setting up SMS")
	if _, err := pool.Exec(context.Background(), `INSERT INTO settings (name, value) VALUES ('email.base_url', '"https://sl.example.com"')`); err != nil {
		t.Fatal(err)
	}
	verifyPhone(t, h, tomas, sms, "+15550102222")
	before := len(sms.sent)

	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Core switch down"}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignee_id":"`+tomas.id+`","priority":1}`), 200, "making it urgent")
	texter := &notify.Texter{Pool: pool, Apps: reg, Keys: keys, Sender: sms}
	if n := runJobs(t, pool, notify.SMSKind, texter.Handler()); n != 1 {
		t.Fatalf("%d SMS jobs, want 1", n)
	}
	got := sms.last(t)
	if len(sms.sent) != before+1 || got.to != "+15550102222" || got.body != "INC1 is urgent: Core switch down https://sl.example.com/tickets/INC1" {
		t.Errorf("the text: %+v", got)
	}

	// Read in the app before the worker got to it: not texted.
	tk2 := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Second outage"}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk2.ID, `{"assignee_id":"`+tomas.id+`","priority":1}`), 200, "another urgent one")
	expect(t, tomas.do(h, "GET", "/api/v1/tickets/"+tk2.ID, ""), 200, "opening it")
	expect(t, tomas.do(h, "POST", "/api/v1/notifications/read", `{"task_id":"`+tk2.ID+`"}`), 200, "reading it")
	runJobs(t, pool, notify.SMSKind, texter.Handler())
	if len(sms.sent) != before+1 {
		t.Errorf("a notification read in the app was texted: %+v", sms.sent)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM channel_deliveries WHERE channel = 'sms' AND kind = 'notification' AND status = 'skipped'"); n != 1 {
		t.Errorf("%d skipped texts logged, want 1", n)
	}
	// Someone without a verified number gets no text.
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignee_id":"`+agent.id+`","priority":2}`), 200, "")
	if n := countRows(t, pool, "SELECT count(*) FROM channel_deliveries WHERE channel = 'sms' AND user_id = $1", agent.id); n != 0 {
		t.Errorf("%d texts to someone with no number", n)
	}
}

// Routing a ticket to a group with a Teams channel posts one card to the channel, when Teams is on; connecting a
// channel posts a test first, and a webhook that is not Microsoft's is refused.
func TestAGroupIsToldInItsTeamsChannel(t *testing.T) {
	h, pool, reg, keys, _, teams := setupMessaging(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	g := decodeBody[group](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Network"}`))
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+g.ID+"/members/"+agent.id, ""), 204, "a member")

	hook := "https://prod-01.westeurope.logic.azure.com/workflows/abc/triggers/manual/paths/invoke?sig=s3cret"
	refusal(t, admin.do(h, "PUT", "/api/v1/groups/"+g.ID+"/teams", `{"webhook":"https://example.com/hook"}`), 422, "Workflows", "a webhook that is not Teams'")
	refusal(t, agent.do(h, "PUT", "/api/v1/groups/"+g.ID+"/teams", `{"webhook":"`+hook+`"}`), 403, "", "an agent connecting a channel")
	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+g.ID+"/teams", `{"webhook":"`+hook+`"}`), 204, "connecting the channel")
	if len(teams.posts) != 1 || teams.posts[0].webhook != hook || !strings.Contains(teams.posts[0].post.Text, "Network") {
		t.Fatalf("the test post: %+v", teams.posts)
	}
	st := admin.do(h, "GET", "/api/v1/settings/teams", "").Body.String()
	if strings.Contains(st, "s3cret") || !strings.Contains(st, `"connected":true`) {
		t.Errorf("the Teams settings: %s", st)
	}

	poster := &notify.Poster{Pool: pool, Apps: reg, Keys: keys, Teams: teams}
	// Teams off: nothing is queued.
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Switch down"}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignment_group_id":"`+g.ID+`"}`), 200, "routing it")
	if n := runJobs(t, pool, notify.TeamsKind, poster.Handler()); n != 0 {
		t.Errorf("%d posts while Teams is off", n)
	}

	expect(t, admin.do(h, "PUT", "/api/v1/settings/teams", `{"enabled":true}`), 200, "turning Teams on")
	if _, err := pool.Exec(context.Background(), `INSERT INTO settings (name, value) VALUES ('email.base_url', '"https://sl.example.com"')`); err != nil {
		t.Fatal(err)
	}
	tk2 := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Fiber cut"}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk2.ID, `{"assignment_group_id":"`+g.ID+`"}`), 200, "routing another")
	if n := runJobs(t, pool, notify.TeamsKind, poster.Handler()); n != 1 {
		t.Fatalf("%d posts, want 1", n)
	}
	p := teams.posts[len(teams.posts)-1]
	if p.webhook != hook || p.post.Title != "INC2 is waiting for Network" || p.post.Text != "Fiber cut" ||
		p.post.Link != "https://sl.example.com/tickets/INC2" || p.post.LinkLabel != "Open INC2" {
		t.Errorf("the post: %+v", p)
	}
	log := admin.do(h, "GET", "/api/v1/settings/messaging/deliveries?channel=teams", "").Body.String()
	if !strings.Contains(log, `"status":"sent"`) || !strings.Contains(log, `"to":"Network"`) {
		t.Errorf("the delivery log: %s", log)
	}
	expect(t, admin.do(h, "DELETE", "/api/v1/groups/"+g.ID+"/teams", ""), 204, "disconnecting")
}

// A pending approval's notification is worded by core-itsm's approval-pending template.
func TestTheApprovalNotificationIsATemplate(t *testing.T) {
	h, pool, reg, _, _, _ := setupMessaging(t)
	_ = h
	if n := reg.Notification("core-itsm", "approval-pending"); n == nil || !strings.Contains(n.Title, "{{record.reference}}") {
		t.Fatalf("the template: %+v", n)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM pg_proc WHERE proname = 'notify_on_approval_request' AND prosrc LIKE '%approval-pending%'"); n != 1 {
		t.Error("the trigger does not name the template")
	}
}

// testSID is a made-up Twilio account id, assembled so that secret scanners do not take it for a real one.
const testSID = "A" + "C0123456789abcdef0123456789abcdef"
