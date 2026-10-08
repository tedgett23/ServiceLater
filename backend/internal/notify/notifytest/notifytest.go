// Package notifytest runs a real SMTP server inside a test, so sending email is tested end to end. Only tests import it.
package notifytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// Message is one email the server accepted.
type Message struct {
	From string
	To   []string
	Data string
}

// Server is an SMTP server on 127.0.0.1. With Username set it requires PLAIN sign-in; with TLS it offers STARTTLS
// with a certificate RootCAs trusts.
type Server struct {
	Host string
	Port int
	// RootCAs trusts the server's certificate (nil without TLS).
	RootCAs *x509.CertPool

	username, password string
	mu                 sync.Mutex
	messages           []Message
	reject             map[string]*smtp.SMTPError
}

// Options are what the server requires.
type Options struct {
	Username, Password string
	// TLS offers STARTTLS (and, with Implicit, speaks TLS from the first byte instead).
	TLS, Implicit bool
}

// Start runs a server until the test ends.
func Start(t *testing.T, o Options) *Server {
	t.Helper()
	s := &Server{Host: "127.0.0.1", username: o.Username, password: o.Password, reject: map[string]*smtp.SMTPError{}}
	srv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) { return &session{s: s}, nil }))
	srv.Domain = "localhost"
	srv.AllowInsecureAuth = !o.TLS
	srv.ReadTimeout, srv.WriteTimeout = 10*time.Second, 10*time.Second
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if o.TLS {
		cert, pool := selfSigned(t)
		s.RootCAs = pool
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		if o.Implicit {
			l = tls.NewListener(l, srv.TLSConfig)
		}
	}
	s.Port = l.Addr().(*net.TCPAddr).Port
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return s
}

// Reject makes the server refuse mail to this address with a permanent error.
func (s *Server) Reject(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reject[addr] = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such mailbox"}
}

// Messages are the emails accepted so far.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.messages...)
}

// Wait waits up to five seconds for at least n emails.
func (s *Server) Wait(t *testing.T, n int) []Message {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if m := s.Messages(); len(m) >= n {
			return m
		}
	}
	t.Fatalf("waited for %d emails, got %d", n, len(s.Messages()))
	return nil
}

type session struct {
	s      *Server
	authed bool
	from   string
	to     []string
}

func (x *session) AuthMechanisms() []string {
	if x.s.username == "" {
		return nil
	}
	return []string{sasl.Plain}
}

func (x *session) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		if user != x.s.username || pass != x.s.password {
			return &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "authentication failed"}
		}
		x.authed = true
		return nil
	}), nil
}

func (x *session) Mail(from string, _ *smtp.MailOptions) error {
	if x.s.username != "" && !x.authed {
		return smtp.ErrAuthRequired
	}
	x.from = from
	return nil
}

func (x *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	x.s.mu.Lock()
	defer x.s.mu.Unlock()
	if e := x.s.reject[to]; e != nil {
		return e
	}
	x.to = append(x.to, to)
	return nil
}

func (x *session) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if len(x.to) == 0 {
		return errors.New("no recipients")
	}
	x.s.mu.Lock()
	defer x.s.mu.Unlock()
	x.s.messages = append(x.s.messages, Message{From: x.from, To: x.to, Data: strings.ReplaceAll(string(b), "\r\n", "\n")})
	return nil
}

func (x *session) Reset()        { x.from, x.to = "", nil }
func (x *session) Logout() error { return nil }

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}
