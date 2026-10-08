package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wneessen/go-mail"

	"github.com/servicelater/servicelater/backend/internal/settings"
)

// Email is one message to send.
type Email struct {
	To, ToName    string
	Subject, Body string
	// NotificationID, when set, is put in a header, so a reply or a bounce can be traced to it.
	NotificationID string
}

// Sender sends email with go-mail. The zero value verifies the server's certificate against the system's roots.
type Sender struct {
	// TLSConfig replaces the default (tests trust their own server's certificate).
	TLSConfig *tls.Config
	Timeout   time.Duration
}

// SendError is a refusal from the mail server, or a failure to reach it.
type SendError struct {
	Err error
	// Permanent says sending again will not help (the server rejected the sender, the recipient or the sign-in).
	Permanent bool
}

func (e *SendError) Error() string { return e.Err.Error() }
func (e *SendError) Unwrap() error { return e.Err }

// Send sends one email through the server in e, signing in with password when e has a username.
func (s Sender) Send(ctx context.Context, e settings.Email, password string, m Email) error {
	if e.Host == "" || e.FromAddress == "" {
		return &SendError{Err: errors.New("email is not set up: it needs a host and a from address"), Permanent: true}
	}
	msg := mail.NewMsg()
	if err := msg.FromFormat(e.FromName, e.FromAddress); err != nil {
		return &SendError{Err: fmt.Errorf("from address: %w", err), Permanent: true}
	}
	if err := msg.AddToFormat(m.ToName, m.To); err != nil {
		return &SendError{Err: fmt.Errorf("recipient %s: %w", m.To, err), Permanent: true}
	}
	msg.Subject(oneLine(m.Subject))
	msg.SetMessageID()
	msg.SetDate()
	// Automatic mail: out-of-office replies and mailing lists leave it alone (RFC 3834).
	msg.SetGenHeader(mail.Header("Auto-Submitted"), "auto-generated")
	if m.NotificationID != "" {
		msg.SetGenHeader(mail.Header("X-ServiceLater-Notification"), m.NotificationID)
	}
	msg.SetBodyString(mail.TypeTextPlain, m.Body)

	timeout := s.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	tlsConf := s.TLSConfig
	if tlsConf == nil {
		tlsConf = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tlsConf = tlsConf.Clone()
	tlsConf.ServerName = e.Host
	opts := []mail.Option{mail.WithPort(e.Port), mail.WithTimeout(timeout), mail.WithTLSConfig(tlsConf)}
	switch e.TLS {
	case settings.TLSImplicit:
		opts = append(opts, mail.WithSSL())
	case settings.TLSNone:
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
	default:
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	}
	if e.Username != "" {
		// Over TLS the best mechanism the server offers is chosen. Without TLS (only allowed to this machine) automatic
		// choice refuses to send a password at all, so PLAIN is named; go-mail allows it only to localhost.
		auth := mail.SMTPAuthAutoDiscover
		if e.TLS == settings.TLSNone {
			auth = mail.SMTPAuthPlain
		}
		opts = append(opts, mail.WithSMTPAuth(auth), mail.WithUsername(e.Username), mail.WithPassword(password))
	}
	c, err := mail.NewClient(e.Host, opts...)
	if err != nil {
		return &SendError{Err: err, Permanent: true}
	}
	if err := c.DialAndSendWithContext(ctx, msg); err != nil {
		return &SendError{Err: err, Permanent: permanent(err)}
	}
	return nil
}

// permanent reports whether the server refused for good: a 5xx reply, or a failed sign-in. Anything else (no
// connection, a timeout, a 4xx "try later") is worth trying again.
func permanent(err error) bool {
	var se *mail.SendError
	if errors.As(err, &se) {
		if se.IsTemp() {
			return false
		}
		if code := se.ErrorCode(); code >= 500 && code < 600 {
			return true
		}
	}
	s := err.Error()
	return strings.Contains(s, "authentication failed") || strings.Contains(s, "535 ")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
