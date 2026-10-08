package notify_test

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/notify"
	"github.com/servicelater/servicelater/backend/internal/notify/notifytest"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

func cfg(s *notifytest.Server, mode, user string) settings.Email {
	return settings.Email{Enabled: true, Host: s.Host, Port: s.Port, TLS: mode, Username: user,
		FromAddress: "desk@example.com", FromName: "Service Desk"}
}

func TestEmailGoesOutOverEveryKindOfConnection(t *testing.T) {
	ctx := context.Background()
	msg := notify.Email{To: "ada@example.com", ToName: "Ada", Subject: "INC4 was\nassigned to you", Body: "Printer on fire\n", NotificationID: "n-1"}
	for name, o := range map[string]notifytest.Options{
		"a relay on this machine":  {Username: "desk", Password: "s3cret"},
		"starttls":                 {Username: "desk", Password: "s3cret", TLS: true},
		"tls from the first byte":  {Username: "desk", Password: "s3cret", TLS: true, Implicit: true},
		"no sign-in on this relay": {},
	} {
		srv := notifytest.Start(t, o)
		mode := settings.TLSNone
		if o.TLS {
			mode = settings.TLSStartTLS
			if o.Implicit {
				mode = settings.TLSImplicit
			}
		}
		sender := notify.Sender{TLSConfig: &tls.Config{RootCAs: srv.RootCAs, MinVersion: tls.VersionTLS12}}
		if err := sender.Send(ctx, cfg(srv, mode, o.Username), o.Password, msg); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		got := srv.Wait(t, 1)[0]
		for _, want := range []string{"Subject: INC4 was assigned to you", "From: \"Service Desk\" <desk@example.com>",
			"Auto-Submitted: auto-generated", "X-ServiceLater-Notification: n-1", "Printer on fire"} {
			if !strings.Contains(got.Data, want) {
				t.Errorf("%s: no %q in\n%s", name, want, got.Data)
			}
		}
		if got.From != "desk@example.com" || len(got.To) != 1 || got.To[0] != "ada@example.com" {
			t.Errorf("%s: envelope %s -> %v", name, got.From, got.To)
		}
	}
}

func TestRefusalsAreToldApartFromTroubleThatMayPass(t *testing.T) {
	ctx := context.Background()
	srv := notifytest.Start(t, notifytest.Options{Username: "desk", Password: "s3cret"})
	msg := notify.Email{To: "ada@example.com", Subject: "x", Body: "y"}
	var se *notify.SendError

	err := notify.Sender{}.Send(ctx, cfg(srv, settings.TLSNone, "desk"), "wrong", msg)
	if !errors.As(err, &se) || !se.Permanent {
		t.Errorf("a wrong password is permanent: %v", err)
	}
	srv.Reject("gone@example.com")
	err = notify.Sender{}.Send(ctx, cfg(srv, settings.TLSNone, "desk"), "s3cret", notify.Email{To: "gone@example.com", Subject: "x", Body: "y"})
	if !errors.As(err, &se) || !se.Permanent || !strings.Contains(err.Error(), "no such mailbox") {
		t.Errorf("a rejected recipient is permanent: %v", err)
	}
	down := cfg(srv, settings.TLSNone, "")
	down.Port = 1
	if err := (notify.Sender{}).Send(ctx, down, "", msg); !errors.As(err, &se) || se.Permanent {
		t.Errorf("no server there may pass later: %v", err)
	}
	// A server whose certificate is not trusted is never sent to.
	tlsSrv := notifytest.Start(t, notifytest.Options{TLS: true})
	if err := (notify.Sender{}).Send(ctx, cfg(tlsSrv, settings.TLSStartTLS, ""), "", msg); err == nil {
		t.Error("sent to an untrusted certificate")
	}
}

func TestTheEmailSaysWhereToOpenItAndHowToStop(t *testing.T) {
	body := notify.Compose(notify.Message{Title: "INC4 was assigned to you", Body: "Printer on fire", Link: "/tickets/INC4"}, "https://sl.example.com")
	for _, want := range []string{"INC4 was assigned to you", "Printer on fire", "https://sl.example.com/tickets/INC4", "https://sl.example.com/account"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in\n%s", want, body)
		}
	}
	if b := notify.Compose(notify.Message{Title: "t", Link: "/tickets/INC4"}, ""); strings.Contains(b, "/tickets/INC4") {
		t.Errorf("a link with no base address: %s", b)
	}
}
