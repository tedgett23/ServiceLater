package inbound_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/inbound"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// A reply is the text above the quoted message, however the mail client quotes it.
func TestReplyKeepsOnlyTheNewText(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"gmail":           {"Thanks, fixed now.\n\nOn Mon, 17 Nov 2026 at 10:00, Service Desk <desk@x.test> wrote:\n> Your ticket INC4\n> was updated", "Thanks, fixed now."},
		"gmail two lines": {"Fixed.\n\nOn Mon, 17 Nov 2026 at 10:00, Service Desk\n<desk@x.test> wrote:\n> old", "Fixed."},
		"outlook":         {"Still broken.\r\n\r\n________________________________\r\nFrom: Service Desk\r\nSent: Monday\r\nSubject: INC4", "Still broken."},
		"outlook header":  {"Still broken.\n\nFrom: Service Desk <desk@x.test>\nSent: Monday, 17 November 2026 10:00\nTo: Lena", "Still broken."},
		"original":        {"See below\n-----Original Message-----\nFrom: x", "See below"},
		"signature":       {"Works now.\n\n-- \nLena Fischer\nFinance", "Works now."},
		"quoted lines":    {"> quoted\nMy answer\n> more", "My answer"},
		"german":          {"Danke!\n\nAm 17.11.2026 um 10:00 schrieb Service Desk <desk@x.test>:\n> alt", "Danke!"},
	} {
		if got := inbound.Reply(strings.ReplaceAll(tc.in, "\r\n", "\n")); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// An HTML-only email is read as text, and mail a program sent is recognised.
func TestParse(t *testing.T) {
	m, err := inbound.Parse([]byte("From: Lena <Lena@Corp.Test>\r\nSubject: =?utf-8?q?Caf=C3=A9_printer?=\r\nMessage-ID: <a@b>\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n<html><head><style>p{}</style></head><body><p>Hello&nbsp;there</p><p>Line&amp;two</p></body></html>\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.From != "lena@corp.test" || m.Subject != "Café printer" || m.Text != "Hello there\nLine&two" || m.Automatic {
		t.Errorf("parsed: %+v (text %q)", m, m.Text)
	}
	for _, h := range []string{"Auto-Submitted: auto-replied", "Precedence: bulk", "List-Id: <x.y>", "X-Autoreply: yes"} {
		m, _ := inbound.Parse([]byte("From: a@b.test\r\n" + h + "\r\n\r\nhi\r\n"))
		if !m.Automatic {
			t.Errorf("%s is not seen as automatic", h)
		}
	}
	if m, _ := inbound.Parse([]byte("From: MAILER-DAEMON@b.test\r\n\r\nbounce\r\n")); !m.Automatic {
		t.Error("a bounce is not seen as automatic")
	}
}

// A receiving server's verdict counts only when it is the trusted one, and only its own (topmost) header.
func TestTrustedAuthenticationResults(t *testing.T) {
	raw := []byte("Authentication-Results: mx.google.com; dmarc=pass header.from=corp.test\r\nAuthentication-Results: evil; dmarc=pass\r\nFrom: kim@corp.test\r\n\r\nhi\r\n")
	m, _ := inbound.Parse(raw)
	none := func(string) ([]string, error) { return []string{"v=DMARC1; p=reject"}, nil }
	if a := (inbound.Verifier{LookupTXT: none, TrustedAuthServ: "mx.google.com"}).Check(raw, m); a.DMARC != inbound.DMARCPass {
		t.Errorf("the trusted server's pass: %+v", a)
	}
	if a := (inbound.Verifier{LookupTXT: none, TrustedAuthServ: "evil"}).Check(raw, m); a.DMARC != inbound.DMARCFail {
		t.Errorf("a header that is not the topmost: %+v", a)
	}
	if a := (inbound.Verifier{LookupTXT: none}).Check(raw, m); a.DMARC != inbound.DMARCFail {
		t.Errorf("nothing trusted: %+v", a)
	}
}

// The Microsoft 365 mailbox signs in with client credentials, lists unread mail, reads each as MIME and marks it read.
func TestGraphMailbox(t *testing.T) {
	var marked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tenant-1/oauth2/v2.0/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "client_credentials" || !strings.HasSuffix(r.Form.Get("scope"), "/.default") {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "token_type": "Bearer", "expires_in": 3600})
		case r.Header.Get("Authorization") != "Bearer tok":
			http.Error(w, `{"error":{"code":"InvalidAuthenticationToken","message":"no"}}`, http.StatusUnauthorized)
		case r.URL.Path == "/v1.0/users/desk@corp.test/mailFolders/inbox/messages":
			if r.URL.Query().Get("$filter") != "isRead eq false" {
				http.Error(w, "bad filter", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"value":[{"id":"AAA1"}]}`))
		case r.URL.Path == "/v1.0/users/desk@corp.test/messages/AAA1/$value":
			_, _ = w.Write([]byte("From: kim@corp.test\r\nSubject: hi\r\n\r\nhello\r\n"))
		case r.Method == http.MethodPatch && r.URL.Path == "/v1.0/users/desk@corp.test/messages/AAA1":
			body, _ := io.ReadAll(r.Body)
			marked = append(marked, string(body))
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := settings.Inbound{Mode: settings.InboundGraph, TenantID: "tenant-1", ClientID: "00000000-0000-0000-0000-000000000001",
		Mailbox: "desk@corp.test", GraphBase: srv.URL, LoginBase: srv.URL}
	box, err := inbound.Open(context.Background(), cfg, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := box.Unread(context.Background(), 25)
	if err != nil || len(msgs) != 1 || !strings.Contains(string(msgs[0].Raw), "hello") {
		t.Fatalf("unread: %v %+v", err, msgs)
	}
	if err := box.Done(context.Background(), msgs[0].ID); err != nil || len(marked) != 1 || marked[0] != `{"isRead":true}` {
		t.Errorf("marking it read: %v %v", err, marked)
	}
}
