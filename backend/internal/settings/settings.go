// Package settings is the instance configuration admins change at run time, kept in the settings table. Which settings
// exist, their types and defaults are fixed here, so the table never holds anything code does not understand. Values
// that must stay secret (a password) are not settings: they are secrets (internal/secrets), named here too.
package settings

import (
	"encoding/json"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"slices"
	"strings"
)

// TLS modes for the mail server.
const (
	TLSStartTLS = "starttls" // plain connection upgraded with STARTTLS, which is required (usually port 587)
	TLSImplicit = "tls"      // TLS from the first byte (usually port 465)
	TLSNone     = "none"     // no encryption: only for a relay on the same machine
)

// EmailPassword is the secret that holds the mail server's password.
const EmailPassword = "email.password"

// EmailPrefix is what every email setting's name starts with.
const EmailPrefix = "email."

// Email is how this instance sends email.
type Email struct {
	// Enabled turns email notifications on. Test emails can be sent while it is off.
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	// TLS is starttls, tls or none (none only for a server on this machine).
	TLS string `json:"tls"`
	// Username signs in to the server; empty sends without signing in.
	Username    string `json:"username"`
	FromAddress string `json:"from_address"`
	FromName    string `json:"from_name"`
	// BaseURL is the address people open ServiceLater at, for the links in emails (https://servicelater.example.com).
	BaseURL string `json:"base_url"`
	// Approvals emails a person when a workflow asks them to approve something.
	Approvals bool `json:"approvals"`
}

// DefaultEmail is what each email setting is until an admin sets it.
var DefaultEmail = Email{Port: 587, TLS: TLSStartTLS, FromName: "ServiceLater", Approvals: true}

var emailNames = []string{"enabled", "host", "port", "tls", "username", "from_address", "from_name", "base_url", "approvals"}

// EmailFrom reads the email settings from stored rows (name to JSON value); what is not stored has its default.
func EmailFrom(stored map[string][]byte) Email {
	e := DefaultEmail
	all := map[string]any{"enabled": &e.Enabled, "host": &e.Host, "port": &e.Port, "tls": &e.TLS, "username": &e.Username,
		"from_address": &e.FromAddress, "from_name": &e.FromName, "base_url": &e.BaseURL, "approvals": &e.Approvals}
	for name, dst := range all {
		if raw, ok := stored[EmailPrefix+name]; ok {
			_ = json.Unmarshal(raw, dst) // a value of the wrong type keeps the default
		}
	}
	return e
}

// Rows are the email settings as they are stored, one per name.
func (e Email) Rows() map[string][]byte {
	vals := map[string]any{"enabled": e.Enabled, "host": e.Host, "port": e.Port, "tls": e.TLS, "username": e.Username,
		"from_address": e.FromAddress, "from_name": e.FromName, "base_url": e.BaseURL, "approvals": e.Approvals}
	out := make(map[string][]byte, len(emailNames))
	for _, n := range emailNames {
		raw, _ := json.Marshal(vals[n])
		out[EmailPrefix+n] = raw
	}
	return out
}

// Check normalises the settings and says what is wrong with them. Turning email on needs a server and a sender.
func (e *Email) Check() error {
	e.Host = strings.TrimSpace(e.Host)
	e.Username = strings.TrimSpace(e.Username)
	e.FromAddress = strings.TrimSpace(e.FromAddress)
	e.FromName = strings.TrimSpace(e.FromName)
	e.BaseURL = strings.TrimRight(strings.TrimSpace(e.BaseURL), "/")
	switch {
	case len(e.Host) > 253 || strings.ContainsAny(e.Host, " /:@") && net.ParseIP(e.Host) == nil:
		return fmt.Errorf("host must be a server name or address, such as smtp.example.com")
	case e.Port < 1 || e.Port > 65535:
		return fmt.Errorf("port must be 1 to 65535")
	case e.TLS != TLSStartTLS && e.TLS != TLSImplicit && e.TLS != TLSNone:
		return fmt.Errorf("tls must be starttls, tls or none")
	case e.TLS == TLSNone && e.Host != "" && !Local(e.Host):
		return fmt.Errorf("email without encryption is only allowed to a server on this machine (localhost); use starttls or tls")
	case len(e.Username) > 320 || len(e.FromName) > 100:
		return fmt.Errorf("username or sender name is too long")
	case strings.ContainsAny(e.FromName, "\r\n<>\""):
		return fmt.Errorf("the sender name cannot contain line breaks, quotes or angle brackets")
	}
	if e.FromAddress != "" {
		a, err := mail.ParseAddress(e.FromAddress)
		if err != nil || a.Name != "" {
			return fmt.Errorf("from_address must be an email address, such as servicedesk@example.com")
		}
	}
	if e.BaseURL != "" {
		u, err := url.Parse(e.BaseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("base_url must be the address people open ServiceLater at, such as https://servicelater.example.com")
		}
	}
	if e.Enabled && (e.Host == "" || e.FromAddress == "") {
		return fmt.Errorf("turning email on needs a host and a from address")
	}
	return nil
}

// Local reports whether a host is this machine.
func Local(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// HistoryPrefix is what every history setting's name starts with.
const HistoryPrefix = "history."

// AllTypes in History.RecordTypes means every record type, including ones added later.
const AllTypes = "*"

// History is which changes are recorded field by field (docs/apps-platform.md §9.4). Whatever it says, a record's
// creation, status, assignee and assignment group, and an item's creation, status and deletion, are always recorded:
// "who closed this" must always have an answer. Fields an app marks history: false are left out too.
type History struct {
	// RecordTypes are the record types whose other changes are recorded: their names, or * for every type.
	RecordTypes []string `json:"record_types"`
	// CMDB records the other changes to configuration items.
	CMDB bool `json:"cmdb"`
}

// DefaultHistory records everything.
var DefaultHistory = History{RecordTypes: []string{AllTypes}, CMDB: true}

// HistoryFrom reads the history settings from stored rows; what is not stored has its default.
func HistoryFrom(stored map[string][]byte) History {
	h := DefaultHistory
	if raw, ok := stored[HistoryPrefix+"record_types"]; ok {
		var types []string
		if json.Unmarshal(raw, &types) == nil {
			h.RecordTypes = types
		}
	}
	if raw, ok := stored[HistoryPrefix+"cmdb"]; ok {
		_ = json.Unmarshal(raw, &h.CMDB)
	}
	if h.RecordTypes == nil {
		h.RecordTypes = []string{}
	}
	return h
}

// Rows are the history settings as they are stored.
func (h History) Rows() map[string][]byte {
	types, _ := json.Marshal(h.RecordTypes)
	cmdb, _ := json.Marshal(h.CMDB)
	return map[string][]byte{HistoryPrefix + "record_types": types, HistoryPrefix + "cmdb": cmdb}
}

// Check normalises the settings against the record types this version knows and says what is wrong.
func (h *History) Check(known []string) error {
	seen := map[string]bool{}
	var out []string
	for _, t := range h.RecordTypes {
		t = strings.TrimSpace(t)
		switch {
		case t == AllTypes:
			h.RecordTypes = []string{AllTypes}
			return nil
		case !slices.Contains(known, t):
			return fmt.Errorf("record_types: %q is not a record type (available: %s, or * for every type)", t, strings.Join(known, ", "))
		case !seen[t]:
			seen[t] = true
			out = append(out, t)
		}
	}
	slices.Sort(out)
	if out == nil {
		out = []string{}
	}
	h.RecordTypes = out
	return nil
}
