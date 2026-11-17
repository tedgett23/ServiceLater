// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Roles a single binary can run. SL_ROLES is a comma-separated set; "all" (the default) is web,api,worker.
const (
	RoleWeb    = "web"    // serves the embedded SPA (and optionally proxies /api and /ws to SL_API_URL)
	RoleAPI    = "api"    // REST + WebSocket + LISTEN/NOTIFY fan-out
	RoleWorker = "worker" // job queue workers and workflow engine
)

type Config struct {
	Roles       map[string]bool
	Addr        string
	DatabaseURL string
	APIURL      string // web role: upstream API for /api and /ws. Empty when routing is done by an ingress or the api role is local.

	TLS       string // off (default) | auto (Let's Encrypt) | files
	Domain    string // auto
	TLSCert   string // files
	TLSKey    string // files
	CertCache string // auto

	JWTSecret  string        // api role: HMAC key for access tokens, >= 32 bytes, identical on every api replica
	AccessTTL  time.Duration // short-lived access token
	RefreshTTL time.Duration // refresh session lifetime

	// Apps: which first-party apps installed in the binary are installed and upgraded at startup (default: the
	// core ones; "all", "none" or a comma-separated list), and where this instance keeps its local overrides.
	BundledApps  string
	AppsLocalDir string

	// AttachmentsDir is the folder attachments are kept in when the instance stores them locally
	// (SL_ATTACHMENTS_DIR, default /data/attachments). On a cluster it must be the same shared folder on every api and
	// worker server.
	AttachmentsDir string

	// MaxLiveConns is how many live (WebSocket) connections one person may hold at once: their open tabs and devices
	// (SL_MAX_LIVE_CONNS, default 10).
	MaxLiveConns int

	// WorkerConcurrency is how many background jobs the worker role runs at once (SL_WORKER_CONCURRENCY, default 4).
	WorkerConcurrency int

	// MailPoll is how often the worker reads the incoming mailbox, when incoming mail is on (SL_MAIL_POLL, default 1m,
	// at least 1s).
	MailPoll time.Duration

	// Optional: create the first admin on startup when no users exist.
	BootstrapEmail, BootstrapName, BootstrapPassword string
}

func Load() (Config, error) {
	roles, err := parseRoles(getenv("SL_ROLES", "all"))
	if err != nil {
		return Config{}, err
	}
	c := Config{
		Roles:       roles,
		Addr:        getenv("SL_ADDR", ":8080"),
		DatabaseURL: DatabaseURL(),
		APIURL:      os.Getenv("SL_API_URL"),
		TLS:         getenv("SL_TLS", "off"),
		Domain:      os.Getenv("SL_DOMAIN"),
		TLSCert:     os.Getenv("SL_TLS_CERT"),
		TLSKey:      os.Getenv("SL_TLS_KEY"),
		CertCache:   getenv("SL_TLS_CACHE_DIR", "/data/certs"),

		JWTSecret:         os.Getenv("SL_JWT_SECRET"),
		AccessTTL:         15 * time.Minute,
		RefreshTTL:        30 * 24 * time.Hour,
		WorkerConcurrency: 4,
		MailPoll:          time.Minute,
		MaxLiveConns:      10,
		BundledApps:       os.Getenv("SL_BUNDLED_APPS"),
		AppsLocalDir:      os.Getenv("SL_APPS_LOCAL_DIR"),
		AttachmentsDir:    getenv("SL_ATTACHMENTS_DIR", "/data/attachments"),
		BootstrapEmail:    os.Getenv("SL_BOOTSTRAP_ADMIN_EMAIL"),
		BootstrapName:     getenv("SL_BOOTSTRAP_ADMIN_NAME", "Administrator"),
		BootstrapPassword: os.Getenv("SL_BOOTSTRAP_ADMIN_PASSWORD"),
	}
	if v := os.Getenv("SL_MAX_LIVE_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return c, fmt.Errorf("SL_MAX_LIVE_CONNS must be a number from 1 to 1000, got %q", v)
		}
		c.MaxLiveConns = n
	}
	if v := os.Getenv("SL_WORKER_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 64 {
			return c, fmt.Errorf("SL_WORKER_CONCURRENCY must be a number from 1 to 64, got %q", v)
		}
		c.WorkerConcurrency = n
	}
	if v := os.Getenv("SL_MAIL_POLL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			return c, fmt.Errorf("SL_MAIL_POLL must be a duration of at least 1s, such as 1m, got %q", v)
		}
		c.MailPoll = d
	}
	if c.Roles[RoleAPI] && len(c.JWTSecret) < 32 {
		return c, fmt.Errorf("SL_JWT_SECRET must be set to at least 32 characters (generate one with: openssl rand -base64 48)")
	}
	if (c.BootstrapEmail == "") != (c.BootstrapPassword == "") {
		return c, fmt.Errorf("SL_BOOTSTRAP_ADMIN_EMAIL and SL_BOOTSTRAP_ADMIN_PASSWORD must be set together")
	}
	switch c.TLS {
	case "off":
	case "auto":
		if c.Domain == "" {
			return c, fmt.Errorf("SL_TLS=auto requires SL_DOMAIN")
		}
	case "files":
		if c.TLSCert == "" || c.TLSKey == "" {
			return c, fmt.Errorf("SL_TLS=files requires SL_TLS_CERT and SL_TLS_KEY")
		}
	default:
		return c, fmt.Errorf("SL_TLS must be off, auto or files, got %q", c.TLS)
	}
	return c, nil
}

// DatabaseURL is the connection string from SL_DATABASE_URL, or the local dev default.
func DatabaseURL() string {
	return getenv("SL_DATABASE_URL", "postgres://servicelater:servicelater@localhost:5432/servicelater?sslmode=disable")
}

func (c Config) Has(role string) bool { return c.Roles[role] }

func (c Config) NeedsDB() bool { return c.Roles[RoleAPI] || c.Roles[RoleWorker] }

func parseRoles(s string) (map[string]bool, error) {
	roles := map[string]bool{}
	for _, r := range strings.Split(s, ",") {
		switch r = strings.TrimSpace(r); r {
		case "all":
			roles[RoleWeb], roles[RoleAPI], roles[RoleWorker] = true, true, true
		case RoleWeb, RoleAPI, RoleWorker:
			roles[r] = true
		case "":
		default:
			return nil, fmt.Errorf("unknown role %q (want all, web, api, worker)", r)
		}
	}
	if len(roles) == 0 {
		return nil, fmt.Errorf("SL_ROLES is empty")
	}
	return roles, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
