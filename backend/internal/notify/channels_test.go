package notify_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/notify"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// The Twilio client posts the Messages API's form with the account's credentials; a refusal Twilio would repeat is
// permanent, being rate-limited is not.
func TestTwilioSendsTheMessagesForm(t *testing.T) {
	var got url.Values
	var user, pass, path string
	status := http.StatusCreated
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		user, pass, _ = r.BasicAuth()
		raw, _ := io.ReadAll(r.Body)
		got, _ = url.ParseQuery(string(raw))
		w.WriteHeader(status)
		if status >= 400 {
			_, _ = w.Write([]byte(`{"code":21211,"message":"The 'To' number is not a valid phone number."}`))
		}
	}))
	defer srv.Close()
	cfg := settings.SMS{AccountSID: testSID, From: "+15550100000", APIBase: srv.URL}
	if err := (notify.Twilio{}).Send(context.Background(), cfg, "tok", "+15550101234", "hello"); err != nil {
		t.Fatal(err)
	}
	if path != "/2010-04-01/Accounts/"+testSID+"/Messages.json" || user != cfg.AccountSID || pass != "tok" ||
		got.Get("To") != "+15550101234" || got.Get("From") != "+15550100000" || got.Get("Body") != "hello" {
		t.Errorf("the request: %s %s %s %v", path, user, pass, got)
	}
	cfg.From = "MG0123456789abcdef0123456789abcdef"
	_ = (notify.Twilio{}).Send(context.Background(), cfg, "tok", "+15550101234", "hello")
	if got.Get("MessagingServiceSid") != cfg.From || got.Has("From") {
		t.Errorf("a messaging service: %v", got)
	}

	status = http.StatusBadRequest
	err := (notify.Twilio{}).Send(context.Background(), cfg, "tok", "+1", "hello")
	var se *notify.SendError
	if !errors.As(err, &se) || !se.Permanent || !strings.Contains(err.Error(), "not a valid phone number") {
		t.Errorf("a refusal: %v", err)
	}
	status = http.StatusTooManyRequests
	if err := (notify.Twilio{}).Send(context.Background(), cfg, "tok", "+15550101234", "hello"); !errors.As(err, &se) || se.Permanent {
		t.Errorf("rate-limited: %v", err)
	}
}

// A Teams post is one Adaptive Card with a button to the record.
func TestTeamsPostsAnAdaptiveCard(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	err := (notify.TeamsPoster{}).Post(context.Background(), srv.URL, notify.TeamsPost{Title: "INC2 is waiting", Text: "Fiber cut",
		LinkLabel: "Open INC2", Link: "https://sl.example.com/tickets/INC2"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	for _, want := range []string{`"type":"message"`, `"contentType":"application/vnd.microsoft.card.adaptive"`, `"text":"INC2 is waiting"`,
		`"type":"Action.OpenUrl"`, `"url":"https://sl.example.com/tickets/INC2"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the card lacks %s: %s", want, raw)
		}
	}
}

// A text is the title, the body if there is room, and the link.
func TestSMSText(t *testing.T) {
	m := notify.Message{Title: "INC1 is urgent", Body: "Core  switch\ndown", Link: "/tickets/INC1"}
	if got := notify.SMSText(m, "https://sl.example.com"); got != "INC1 is urgent: Core switch down https://sl.example.com/tickets/INC1" {
		t.Errorf("text: %q", got)
	}
	m.Body = strings.Repeat("x", 1000)
	if got := notify.SMSText(m, "https://sl.example.com"); len([]rune(got)) > 300 || !strings.HasSuffix(got, " https://sl.example.com/tickets/INC1") {
		t.Errorf("a long text: %d %q", len(got), got[len(got)-40:])
	}
}

// testSID is a made-up Twilio account id, assembled so that secret scanners do not take it for a real one.
const testSID = "A" + "C0123456789abcdef0123456789abcdef"
