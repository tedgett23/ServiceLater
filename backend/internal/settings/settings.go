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
	"regexp"
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

// Storage backends for attachments.
const (
	BackendLocal = "local" // a folder: SL_ATTACHMENTS_DIR, shared by every api and worker server on a cluster
	BackendS3    = "s3"    // an S3-compatible bucket (AWS, MinIO, Garage, Ceph...)
)

// StoragePrefix is what every storage setting's name starts with.
const StoragePrefix = "storage."

// StorageS3SecretKey is the secret that holds the bucket's secret access key.
const StorageS3SecretKey = "storage.s3_secret_key" //nolint:gosec // the name a secret is kept under, not a secret

// StorageLocalID names the folder this instance keeps its files in. It is not an admin's setting: the first server to
// use the folder writes it there and here, and every server checks that its folder says the same, so a server that
// does not have the shared folder mounted refuses instead of keeping files nobody else can read.
const StorageLocalID = "storage.local_id"

// Limits on the largest file an admin may allow: S3 takes up to 5 TiB in one object.
const (
	MinMaxSize = 1 << 20
	MaxMaxSize = 5 << 40
)

// Storage is where attachments are kept. Files already stored stay where they are when it changes: each remembers
// its backend.
type Storage struct {
	// Backend is local (a folder on the server) or s3 (a bucket). New files go there.
	Backend string `json:"backend"`
	// MaxSize is the largest file people may attach, in bytes.
	MaxSize int64 `json:"max_size"`
	// S3Endpoint is the bucket service's address: https://s3.amazonaws.com, or http://minio:9000 for one of your own.
	S3Endpoint string `json:"s3_endpoint"`
	S3Region   string `json:"s3_region"`
	S3Bucket   string `json:"s3_bucket"`
	// S3Prefix goes before every key, to share a bucket (attachments/ say).
	S3Prefix string `json:"s3_prefix"`
	// S3PathStyle addresses the bucket as endpoint/bucket rather than bucket.endpoint, which most self-hosted
	// services need.
	S3PathStyle bool `json:"s3_path_style"`
	// S3AccessKeyID signs in with a key (its secret is stored encrypted); empty uses the server's own credentials
	// (AWS_* variables, ~/.aws, or the instance or pod role).
	S3AccessKeyID string `json:"s3_access_key_id"`
	// S3Direct has browsers upload to and download from the bucket themselves, so files never pass through the
	// server. It needs the bucket reachable from people's browsers, with CORS allowing this site. Off, the server
	// carries them.
	S3Direct bool `json:"s3_direct"`
}

// DefaultStorage keeps files in the local folder, up to 10 GiB each.
var DefaultStorage = Storage{Backend: BackendLocal, MaxSize: 10 << 30, S3Endpoint: "https://s3.amazonaws.com", S3Region: "us-east-1", S3Direct: true}

var storageNames = []string{"backend", "max_size", "s3_endpoint", "s3_region", "s3_bucket", "s3_prefix", "s3_path_style", "s3_access_key_id", "s3_direct"}

func (s *Storage) fields() map[string]any {
	return map[string]any{"backend": &s.Backend, "max_size": &s.MaxSize, "s3_endpoint": &s.S3Endpoint, "s3_region": &s.S3Region,
		"s3_bucket": &s.S3Bucket, "s3_prefix": &s.S3Prefix, "s3_path_style": &s.S3PathStyle, "s3_access_key_id": &s.S3AccessKeyID,
		"s3_direct": &s.S3Direct}
}

// StorageFrom reads the storage settings from stored rows; what is not stored has its default.
func StorageFrom(stored map[string][]byte) Storage {
	s := DefaultStorage
	for name, dst := range s.fields() {
		if raw, ok := stored[StoragePrefix+name]; ok {
			_ = json.Unmarshal(raw, dst) // a value of the wrong type keeps the default
		}
	}
	return s
}

// Rows are the storage settings as they are stored, one per name.
func (s Storage) Rows() map[string][]byte {
	f := s.fields()
	out := make(map[string][]byte, len(storageNames))
	for _, n := range storageNames {
		raw, _ := json.Marshal(f[n])
		out[StoragePrefix+n] = raw
	}
	return out
}

var (
	bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionRE = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	prefixRE = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*/$`)
)

// Check normalises the settings and says what is wrong with them. Choosing S3 needs a bucket.
func (s *Storage) Check() error {
	s.S3Endpoint = strings.TrimRight(strings.TrimSpace(s.S3Endpoint), "/")
	s.S3Region = strings.TrimSpace(s.S3Region)
	s.S3Bucket = strings.TrimSpace(s.S3Bucket)
	s.S3AccessKeyID = strings.TrimSpace(s.S3AccessKeyID)
	s.S3Prefix = strings.Trim(strings.TrimSpace(s.S3Prefix), "/")
	if s.S3Prefix != "" {
		s.S3Prefix += "/"
	}
	switch {
	case s.Backend != BackendLocal && s.Backend != BackendS3:
		return fmt.Errorf("backend must be local or s3")
	case s.MaxSize < MinMaxSize || s.MaxSize > MaxMaxSize:
		return fmt.Errorf("max_size must be from 1 MiB to 5 TiB")
	case s.S3Region != "" && !regionRE.MatchString(s.S3Region):
		return fmt.Errorf("s3_region must be a region name, such as us-east-1")
	case s.S3Bucket != "" && !bucketRE.MatchString(s.S3Bucket):
		return fmt.Errorf("s3_bucket must be a bucket name: 3 to 63 lowercase letters, digits, dots and hyphens")
	case s.S3Prefix != "" && (len(s.S3Prefix) > 200 || !prefixRE.MatchString(s.S3Prefix)):
		return fmt.Errorf("s3_prefix may hold letters, digits, dots, hyphens, underscores and slashes")
	case len(s.S3AccessKeyID) > 128 || strings.ContainsAny(s.S3AccessKeyID, " \t\r\n"):
		return fmt.Errorf("s3_access_key_id is not an access key")
	}
	if s.S3Endpoint != "" {
		u, err := url.Parse(s.S3Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") ||
			u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return fmt.Errorf("s3_endpoint must be the service's address, such as https://s3.amazonaws.com or http://minio:9000")
		}
	}
	if s.Backend == BackendS3 && (s.S3Bucket == "" || s.S3Endpoint == "") {
		return fmt.Errorf("storing files in S3 needs an endpoint and a bucket")
	}
	return nil
}
