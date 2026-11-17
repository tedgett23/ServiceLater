package settings

import (
	"encoding/json"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
)

// Ways mail is read.
const (
	InboundIMAP  = "imap"  // a mailbox over IMAP, with a password or an app password
	InboundGraph = "graph" // a Microsoft 365 mailbox through Microsoft Graph, with an app registration
)

// InboundPrefix is what every incoming-mail setting's name starts with; the password and the Graph client secret are
// secrets.
const (
	InboundPrefix      = "inbound."
	InboundPassword    = "inbound.password"     //nolint:gosec // the name of a secret, not a password
	InboundGraphSecret = "inbound.graph_secret" //nolint:gosec // the name of a secret, not a secret
)

// Inbound is how this instance reads the email sent to it, and what it does with it.
type Inbound struct {
	// Enabled turns reading the mailbox on.
	Enabled bool `json:"enabled"`
	// Mode is imap or graph.
	Mode string `json:"mode"`
	// Address is the mailbox's address: notification emails ask for replies there, and mail from it is ignored.
	Address string `json:"address"`

	// IMAP: the server, its port, starttls or tls, and who signs in.
	Host     string `json:"host"`
	Port     int    `json:"port"`
	TLS      string `json:"tls"`
	Username string `json:"username"`

	// Graph: the Entra tenant, the app registration's client id (its secret is a secret), and the mailbox read. The
	// endpoints are the public cloud's unless set (national clouds differ).
	TenantID  string `json:"tenant_id"`
	ClientID  string `json:"client_id"`
	Mailbox   string `json:"mailbox"`
	GraphBase string `json:"graph_base"`
	LoginBase string `json:"login_base"`

	// CreateIncidents files an incident from an email that is not a reply, when it passes the checks below.
	CreateIncidents bool `json:"create_incidents"`
	// AllowedDomains are the sender domains that may file incidents (example.com); required to create any.
	AllowedDomains []string `json:"allowed_domains"`
	// CreateAccounts gives someone from an allowed domain who has no account one, as an end user, instead of
	// ignoring their email.
	CreateAccounts bool `json:"create_accounts"`
	// TrustedAuthServ is the receiving mail server's name in Authentication-Results (mx.google.com); when set, its
	// dmarc=pass is accepted for mail ServiceLater cannot verify itself (no aligned DKIM signature). Empty trusts
	// only ServiceLater's own check.
	TrustedAuthServ string `json:"trusted_authserv"`
	// MaxPerHour is how many incidents one sender may file by email in an hour.
	MaxPerHour int `json:"max_per_hour"`
}

// DefaultInbound is what each setting is until an admin sets it.
var DefaultInbound = Inbound{Mode: InboundIMAP, Port: 993, TLS: TLSImplicit, GraphBase: "https://graph.microsoft.com",
	LoginBase: "https://login.microsoftonline.com", MaxPerHour: 10, AllowedDomains: []string{}}

func (in *Inbound) fields() map[string]any {
	return map[string]any{"enabled": &in.Enabled, "mode": &in.Mode, "address": &in.Address, "host": &in.Host, "port": &in.Port,
		"tls": &in.TLS, "username": &in.Username, "tenant_id": &in.TenantID, "client_id": &in.ClientID, "mailbox": &in.Mailbox,
		"graph_base": &in.GraphBase, "login_base": &in.LoginBase, "create_incidents": &in.CreateIncidents,
		"allowed_domains": &in.AllowedDomains, "create_accounts": &in.CreateAccounts, "trusted_authserv": &in.TrustedAuthServ,
		"max_per_hour": &in.MaxPerHour}
}

// InboundFrom reads the settings from stored rows; what is not stored has its default.
func InboundFrom(stored map[string][]byte) Inbound {
	in := DefaultInbound
	in.AllowedDomains = []string{}
	for name, dst := range in.fields() {
		if raw, ok := stored[InboundPrefix+name]; ok {
			_ = json.Unmarshal(raw, dst)
		}
	}
	return in
}

// Rows are the settings as they are stored.
func (in Inbound) Rows() map[string][]byte {
	out := map[string][]byte{}
	for name, ptr := range in.fields() {
		raw, _ := json.Marshal(ptr)
		out[InboundPrefix+name] = raw
	}
	return out
}

var (
	guidRE   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

func httpsOrLocal(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && (u.Scheme == "https" || (u.Scheme == "http" && Local(u.Hostname())))
}

// Check normalises the settings and says what is wrong with them.
func (in *Inbound) Check() error {
	in.Address, in.Host, in.Username = strings.TrimSpace(in.Address), strings.TrimSpace(in.Host), strings.TrimSpace(in.Username)
	in.TenantID, in.ClientID, in.Mailbox = strings.TrimSpace(in.TenantID), strings.TrimSpace(in.ClientID), strings.TrimSpace(in.Mailbox)
	in.GraphBase, in.LoginBase = strings.TrimRight(strings.TrimSpace(in.GraphBase), "/"), strings.TrimRight(strings.TrimSpace(in.LoginBase), "/")
	in.TrustedAuthServ = strings.ToLower(strings.TrimSpace(in.TrustedAuthServ))
	if in.GraphBase == "" {
		in.GraphBase = DefaultInbound.GraphBase
	}
	if in.LoginBase == "" {
		in.LoginBase = DefaultInbound.LoginBase
	}
	domains := []string{}
	for _, d := range in.AllowedDomains {
		d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
		if d == "" {
			continue
		}
		if !domainRE.MatchString(d) {
			return fmt.Errorf("%s is not a domain (such as example.com)", d)
		}
		domains = append(domains, d)
	}
	in.AllowedDomains = domains
	if in.Address != "" {
		if a, err := mail.ParseAddress(in.Address); err != nil || a.Name != "" {
			return fmt.Errorf("address must be the mailbox's email address, such as servicedesk@example.com")
		}
		in.Address = strings.ToLower(in.Address)
	}
	switch {
	case in.Mode != InboundIMAP && in.Mode != InboundGraph:
		return fmt.Errorf("mode must be imap or graph")
	case in.Port < 1 || in.Port > 65535:
		return fmt.Errorf("port must be 1 to 65535")
	case in.TLS != TLSImplicit && in.TLS != TLSStartTLS && in.TLS != TLSNone:
		return fmt.Errorf("tls must be tls, starttls or none")
	case in.TLS == TLSNone && in.Host != "" && !Local(in.Host):
		return fmt.Errorf("reading mail without encryption is only allowed from a server on this machine")
	case len(in.Host) > 253 || strings.ContainsAny(in.Host, " /@") && net.ParseIP(in.Host) == nil:
		return fmt.Errorf("host must be a server name, such as imap.gmail.com")
	case in.TenantID != "" && !guidRE.MatchString(in.TenantID) && !domainRE.MatchString(strings.ToLower(in.TenantID)):
		return fmt.Errorf("tenant_id must be the directory (tenant) id, or its domain")
	case in.ClientID != "" && !guidRE.MatchString(in.ClientID):
		return fmt.Errorf("client_id must be the app registration's application (client) id")
	case !httpsOrLocal(in.GraphBase) || !httpsOrLocal(in.LoginBase):
		return fmt.Errorf("the Graph and sign-in addresses must be https")
	case in.MaxPerHour < 1 || in.MaxPerHour > 1000:
		return fmt.Errorf("max_per_hour must be 1 to 1000")
	case in.CreateIncidents && len(in.AllowedDomains) == 0:
		return fmt.Errorf("filing incidents by email needs at least one allowed domain")
	}
	if in.Enabled {
		switch {
		case in.Address == "":
			return fmt.Errorf("turning incoming mail on needs the mailbox's address")
		case in.Mode == InboundIMAP && (in.Host == "" || in.Username == ""):
			return fmt.Errorf("reading over IMAP needs a host and a username")
		case in.Mode == InboundGraph && (in.TenantID == "" || in.ClientID == "" || in.Mailbox == ""):
			return fmt.Errorf("reading through Microsoft 365 needs the tenant, the client id and the mailbox")
		}
	}
	return nil
}
