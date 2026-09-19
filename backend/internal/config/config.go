// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
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
		BootstrapEmail:    os.Getenv("SL_BOOTSTRAP_ADMIN_EMAIL"),
		BootstrapName:     getenv("SL_BOOTSTRAP_ADMIN_NAME", "Administrator"),
		BootstrapPassword: os.Getenv("SL_BOOTSTRAP_ADMIN_PASSWORD"),
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
