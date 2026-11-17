package inbound

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/servicelater/servicelater/backend/internal/settings"
)

// Message is a message waiting in the mailbox: its id there, and its bytes (nil when it is too big to read).
type Message struct {
	ID   string
	Raw  []byte
	Size int64
}

// Mailbox is where mail is read from. Unread lists messages not yet handled; Done marks one handled (read), so it is
// not read again.
type Mailbox interface {
	Unread(ctx context.Context, max int) ([]Message, error)
	Done(ctx context.Context, id string) error
	Close() error
}

// Open connects to the mailbox the settings describe. secret is the IMAP password or the Graph client secret.
func Open(ctx context.Context, cfg settings.Inbound, secret string, tlsConfig *tls.Config) (Mailbox, error) {
	if cfg.Mode == settings.InboundGraph {
		return openGraph(ctx, cfg, secret)
	}
	return openIMAP(cfg, secret, tlsConfig)
}

// ---- IMAP ----

type imapBox struct{ c *imapclient.Client }

func openIMAP(cfg settings.Inbound, password string, tlsConfig *tls.Config) (Mailbox, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	opts := &imapclient.Options{TLSConfig: tlsConfig, Dialer: &net.Dialer{Timeout: 15 * time.Second}}
	var c *imapclient.Client
	var err error
	switch cfg.TLS {
	case settings.TLSImplicit:
		c, err = imapclient.DialTLS(addr, opts)
	case settings.TLSStartTLS:
		c, err = imapclient.DialStartTLS(addr, opts)
	default:
		c, err = imapclient.DialInsecure(addr, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", addr, err)
	}
	if err := c.Login(cfg.Username, password).Wait(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("signing in as %s: %w", cfg.Username, err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("opening the inbox: %w", err)
	}
	return &imapBox{c: c}, nil
}

func (b *imapBox) Unread(_ context.Context, max int) ([]Message, error) {
	data, err := b.c.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen, imap.FlagDeleted}}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("searching the inbox: %w", err)
	}
	uids := data.AllUIDs()
	if len(uids) > max {
		uids = uids[:max]
	}
	if len(uids) == 0 {
		return nil, nil
	}
	sizes, err := b.c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, RFC822Size: true}).Collect()
	if err != nil {
		return nil, fmt.Errorf("reading message sizes: %w", err)
	}
	var out []Message
	section := &imap.FetchItemBodySection{Peek: true}
	for _, s := range sizes {
		m := Message{ID: strconv.FormatUint(uint64(s.UID), 10), Size: s.RFC822Size}
		if s.RFC822Size <= MaxMessage {
			bufs, err := b.c.Fetch(imap.UIDSetNum(s.UID), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
			if err != nil {
				return out, fmt.Errorf("reading a message: %w", err)
			}
			if len(bufs) == 1 {
				m.Raw = bufs[0].FindBodySection(section)
			}
		}
		out = append(out, m)
	}
	return out, nil
}

func (b *imapBox) Done(_ context.Context, id string) error {
	uid, err := strconv.ParseUint(id, 10, 32)
	if err != nil {
		return err
	}
	return b.c.Store(imap.UIDSetNum(imap.UID(uid)), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}, Silent: true}, nil).Close()
}

func (b *imapBox) Close() error {
	_ = b.c.Logout().Wait()
	return b.c.Close()
}

// ---- Microsoft 365 through Graph ----

type graphBox struct {
	client *http.Client
	base   string // .../v1.0/users/{mailbox}
}

func openGraph(ctx context.Context, cfg settings.Inbound, secret string) (Mailbox, error) {
	cc := clientcredentials.Config{ClientID: cfg.ClientID, ClientSecret: secret,
		TokenURL: cfg.LoginBase + "/" + url.PathEscape(cfg.TenantID) + "/oauth2/v2.0/token",
		Scopes:   []string{cfg.GraphBase + "/.default"}}
	client := cc.Client(ctx)
	client.Timeout = 30 * time.Second
	return &graphBox{client: client, base: cfg.GraphBase + "/v1.0/users/" + url.PathEscape(cfg.Mailbox)}, nil
}

func (g *graphBox) do(ctx context.Context, method, path string, body io.Reader, out any) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching Microsoft Graph: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxMessage+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return nil, fmt.Errorf("Microsoft Graph said %s: %s %s", resp.Status, e.Error.Code, e.Error.Message) //nolint:staticcheck // a product name
	}
	if out != nil {
		return raw, json.Unmarshal(raw, out)
	}
	return raw, nil
}

func (g *graphBox) Unread(ctx context.Context, max int) ([]Message, error) {
	var list struct {
		Value []struct {
			ID   string `json:"id"`
			Size int64  `json:"size"`
		} `json:"value"`
	}
	q := url.Values{"$filter": {"isRead eq false"}, "$select": {"id"}, "$top": {strconv.Itoa(max)}, "$orderby": {"receivedDateTime"}}
	if _, err := g.do(ctx, http.MethodGet, "/mailFolders/inbox/messages?"+q.Encode(), nil, &list); err != nil {
		return nil, err
	}
	var out []Message
	for _, v := range list.Value {
		raw, err := g.do(ctx, http.MethodGet, "/messages/"+url.PathEscape(v.ID)+"/$value", nil, nil)
		if err != nil {
			return out, err
		}
		m := Message{ID: v.ID, Size: int64(len(raw))}
		if len(raw) <= MaxMessage {
			m.Raw = raw
		}
		out = append(out, m)
	}
	return out, nil
}

func (g *graphBox) Done(ctx context.Context, id string) error {
	_, err := g.do(ctx, http.MethodPatch, "/messages/"+url.PathEscape(id), strings.NewReader(`{"isRead":true}`), nil)
	return err
}

func (g *graphBox) Close() error { return nil }

// ErrNotConfigured is a mailbox without what it needs to be read.
var ErrNotConfigured = errors.New("incoming mail is not set up")
