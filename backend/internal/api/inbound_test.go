package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/inbound"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/notify/notifytest"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/storage"
)

// startIMAP serves an in-memory mailbox for desk / pw, and returns its port and a client to append mail with.
func startIMAP(t *testing.T) (int, *imapclient.Client) {
	t.Helper()
	mem := imapmemserver.New()
	u := imapmemserver.NewUser("desk", "pw")
	if err := u.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(u)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		InsecureAuth: true,
	})
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login("desk", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return ln.Addr().(*net.TCPAddr).Port, c
}

func deliver(t *testing.T, c *imapclient.Client, raw []byte) {
	t.Helper()
	cmd := c.Append("INBOX", int64(len(raw)), nil)
	if _, err := cmd.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

// fakeDNS answers the TXT lookups DKIM and DMARC make.
type fakeDNS map[string]string

func (d fakeDNS) LookupTXT(name string) ([]string, error) {
	if v, ok := d[strings.TrimSuffix(name, ".")]; ok {
		return []string{v}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

// mailFrom writes an email; with key, it is DKIM-signed for domain.
func mailFrom(t *testing.T, from, subject, body, inReplyTo string, key ed25519.PrivateKey, domain string, extra ...string) []byte {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: servicedesk@corp.test\r\nSubject: %s\r\nMessage-ID: <%s@mail.test>\r\nDate: Mon, 17 Nov 2026 10:00:00 +0000\r\nMIME-Version: 1.0\r\n",
		from, subject, strconv.FormatInt(int64(len(subject)*1000+len(body)+len(from)), 36)+base64.RawURLEncoding.EncodeToString([]byte(subject))[:6])
	if inReplyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: <%s>\r\nReferences: <%s>\r\n", inReplyTo, inReplyTo)
	}
	for _, h := range extra {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n" + strings.ReplaceAll(body, "\n", "\r\n") + "\r\n")
	raw := []byte(b.String())
	if key == nil {
		return raw
	}
	var signed bytes.Buffer
	if err := dkim.Sign(&signed, bytes.NewReader(raw), &dkim.SignOptions{Domain: domain, Selector: "s1", Signer: key}); err != nil {
		t.Fatal(err)
	}
	return signed.Bytes()
}

// withAttachment is a reply with a file attached.
func withAttachment(from, inReplyTo, text, filename string, data []byte) []byte {
	return []byte("From: " + from + "\r\nTo: servicedesk@corp.test\r\nSubject: Re: update\r\nMessage-ID: <att-" + filename + "@mail.test>\r\n" +
		"In-Reply-To: <" + inReplyTo + ">\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=XX\r\n\r\n" +
		"--XX\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + strings.ReplaceAll(text, "\n", "\r\n") + "\r\n" +
		"--XX\r\nContent-Type: image/png\r\nContent-Disposition: attachment; filename=\"" + filename + "\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(data) + "\r\n--XX--\r\n")
}

func fetchMail(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry, keys *secrets.Keyring, dns fakeDNS) {
	t.Helper()
	files := &storage.Manager{Pool: pool, Keys: keys, LocalDir: t.TempDir()}
	r := &inbound.Reader{Pool: pool, Keys: keys, Processor: &inbound.Processor{Pool: pool, Apps: reg, Files: files, LookupTXT: dns.LookupTXT}}
	if err := r.Handler()(context.Background(), jobs.Job{Kind: inbound.FetchKind, Attempt: 1, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
}

type logged struct {
	From, Action, Detail, Reference string
	Attachments                     int
}

func inboundLog(t *testing.T, pool *pgxpool.Pool) map[string]logged {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT e.subject, e.from_address, e.action, e.detail, coalesce(tt.prefix || t.number::text, ''), e.attachments
		FROM inbound_emails e LEFT JOIN tasks t ON t.id = e.task_id LEFT JOIN task_types tt ON tt.type = t.type`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]logged{}
	for rows.Next() {
		var subject string
		var l logged
		if err := rows.Scan(&subject, &l.From, &l.Action, &l.Detail, &l.Reference, &l.Attachments); err != nil {
			t.Fatal(err)
		}
		out[l.From+" "+subject] = l
	}
	return out
}

// Email in: a reply to a notification becomes a comment (with its files) as the person it went to; new mail from an
// allowed domain that DMARC vouches for files an incident; everything else is refused or ignored, and logged.
func TestEmailRepliesAndNewMail(t *testing.T) {
	h, pool, reg, keys, _, _ := setupMessaging(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	lena := newActor(t, h, pool, "lena@corp.test", "end_user")
	newActor(t, h, pool, "kim@corp.test", "end_user")
	port, imapc := startIMAP(t)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	dns := fakeDNS{
		"s1._domainkey.corp.test":  "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub),
		"_dmarc.corp.test":         "v=DMARC1; p=reject",
		"s1._domainkey.other.test": "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(otherKey.Public().(ed25519.PublicKey)),
	}
	expect(t, admin.do(h, "PUT", "/api/v1/settings/inbound", `{"enabled":true,"mode":"imap","address":"servicedesk@corp.test","host":"127.0.0.1",`+
		`"port":`+strconv.Itoa(port)+`,"tls":"none","username":"desk","password":"pw","create_incidents":true,"allowed_domains":["corp.test"],"max_per_hour":10}`), 200, "setting up incoming mail")
	refusal(t, admin.do(h, "PUT", "/api/v1/settings/inbound", `{"enabled":true,"mode":"imap","address":"servicedesk@corp.test","host":"127.0.0.1","port":993,"tls":"tls","username":"desk","create_incidents":true,"allowed_domains":[],"max_per_hour":10}`),
		422, "at least one allowed domain", "filing incidents with no allowed domain")
	if test := decodeBody[map[string]string](t, admin.do(h, "POST", "/api/v1/settings/inbound/test", "")); test["status"] != "ok" {
		t.Fatalf("the connection test: %v", test)
	}

	tk := decodeBody[problemBody](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Printer jams"}`))
	var notif string
	if err := pool.QueryRow(context.Background(), `INSERT INTO notifications (user_id, kind, task_id, app, template) VALUES ($1, 'template', $2, 'core-itsm', 'ticket-assigned') RETURNING id`,
		lena.id, tk.ID).Scan(&notif); err != nil {
		t.Fatal(err)
	}
	msgID := inbound.NotificationMessageID(uuid.MustParse(notif))

	// Replies are checked too: corp.test publishes p=reject, so Lena's reply must carry its DKIM signature.
	reply := withAttachment("Lena <lena@corp.test>", msgID, "Still jams, photo attached.\n\nOn Mon, 17 Nov 2026, Service Desk wrote:\n> Your ticket was updated", "jam.png", []byte("\x89PNGfake"))
	var signed bytes.Buffer
	if err := dkim.Sign(&signed, bytes.NewReader(reply), &dkim.SignOptions{Domain: "corp.test", Selector: "s1", Signer: priv}); err != nil {
		t.Fatal(err)
	}
	deliver(t, imapc, signed.Bytes())
	deliver(t, imapc, mailFrom(t, "Mallory <mallory@corp.test>", "Re: forged", "Close it", msgID, priv, "corp.test"))
	deliver(t, imapc, mailFrom(t, "lena@corp.test", "Out of office", "I am away", msgID, nil, "", "Auto-Submitted: auto-replied"))
	deliver(t, imapc, mailFrom(t, "Kim <kim@corp.test>", "VPN will not connect", "Since this morning.\n\n-- \nKim", "", priv, "corp.test"))
	deliver(t, imapc, mailFrom(t, "kim@corp.test", "Unsigned claim", "Please reset everything", "", nil, ""))
	deliver(t, imapc, mailFrom(t, "x@other.test", "Elsewhere", "Hello", "", otherKey, "other.test"))
	fetchMail(t, pool, reg, keys, dns)

	got := inboundLog(t, pool)
	check := func(key, action, contains string) logged {
		t.Helper()
		l, ok := got[key]
		if !ok || l.Action != action || !strings.Contains(l.Detail, contains) {
			t.Errorf("%s: %+v (want %s, %q)", key, l, action, contains)
		}
		return l
	}
	if l := check("lena@corp.test Re: update", "comment", "comment"); l.Reference != tk.Reference || l.Attachments != 1 {
		t.Errorf("the reply: %+v", l)
	}
	check("mallory@corp.test Re: forged", "refused", "not the address the notification went to")
	check("lena@corp.test Out of office", "ignored", "sent by a program")
	if l := check("kim@corp.test VPN will not connect", "incident", "filed"); !strings.HasPrefix(l.Reference, "INC") {
		t.Errorf("the new incident: %+v", l)
	}
	check("kim@corp.test Unsigned claim", "refused", "does not vouch for it")
	check("x@other.test Elsewhere", "refused", "other.test is not a domain allowed")

	cs := decodeBody[commentList](t, lena.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/comments", "")).Items
	if len(cs) != 1 || cs[0].Body != "Still jams, photo attached." {
		t.Errorf("the comment: %+v", cs)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM attachments WHERE task_id = $1 AND status = 'ready' AND filename = 'jam.png'", tk.ID); n != 1 {
		t.Errorf("%d attachments from the reply", n)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM tasks t JOIN users u ON u.id = t.requester_id WHERE u.email = 'kim@corp.test' AND t.title = 'VPN will not connect' AND t.description LIKE 'Since this morning.%'"); n != 1 {
		t.Error("the incident was not filed for Kim")
	}

	// Everything was marked read; a second run reads nothing new, and a resent message is not handled twice.
	if _, err := imapc.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	unseen, err := imapc.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
	if err != nil || len(unseen.AllUIDs()) != 0 {
		t.Errorf("unread after the run: %v %v", unseen, err)
	}
	deliver(t, imapc, mailFrom(t, "Mallory <mallory@corp.test>", "Re: forged", "Close it", msgID, priv, "corp.test"))
	fetchMail(t, pool, reg, keys, dns)
	if n := countRows(t, pool, "SELECT count(*) FROM inbound_emails"); n != 6 {
		t.Errorf("%d emails logged, want 6", n)
	}
	logBody := admin.do(h, "GET", "/api/v1/settings/inbound/log", "").Body.String()
	if !strings.Contains(logBody, `"action":"incident"`) || strings.Contains(logBody, `"pw"`) {
		t.Errorf("the log: %s", logBody)
	}
}

// While incoming mail is on, a notification email about a record asks for replies at the mailbox and carries the
// Message-ID a reply will name.
func TestNotificationEmailsInviteReplies(t *testing.T) {
	h, pool, reg, keys := setupEmail(t, testKey(6))
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tomas := newActor(t, h, pool, "tomas@x.test", "agent")
	srv := notifytest.Start(t, notifytest.Options{Username: "desk", Password: "pw"})
	expect(t, admin.do(h, "PUT", "/api/v1/settings/email", emailJSON(srv, true, `,"password":"pw"`)), 200, "email on")
	expect(t, admin.do(h, "PUT", "/api/v1/settings/inbound", `{"enabled":true,"mode":"imap","address":"servicedesk@x.test","host":"imap.x.test","port":993,"tls":"tls","username":"desk","max_per_hour":10}`), 200, "incoming mail on")

	tk := decodeBody[problemBody](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Disk full"}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignee_id":"`+tomas.id+`"}`), 200, "assigning Tomás")
	sendQueued(t, pool, reg, keys)
	msgs := srv.Wait(t, 1)
	var notif string
	if err := pool.QueryRow(context.Background(), "SELECT id FROM notifications WHERE user_id = $1 AND task_id = $2", tomas.id, tk.ID).Scan(&notif); err != nil {
		t.Fatal(err)
	}
	data := msgs[0].Data
	for _, want := range []string{"Message-ID: <" + inbound.NotificationMessageID(uuid.MustParse(notif)) + ">", "Reply-To: <servicedesk@x.test>",
		"Reply to this email to comment on " + tk.Reference} {
		if !strings.Contains(data, want) {
			t.Errorf("the email lacks %q:\n%s", want, data)
		}
	}
}
