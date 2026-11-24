package apps

import (
	"fmt"
	"regexp"
	"strings"
)

// Webhook starts a workflow when another system calls POST /api/v1/hooks/<app>/<workflow> (docs/apps-platform.md
// §7.8). The caller proves itself with the app's secret Secret (declared in its manifest's config, type secret, and
// set by an admin under Admin → Connections; a webhook whose secret is not set refuses every call): as a token, the
// request's Authorization: Bearer <secret>, or as a signature, the hex HMAC-SHA256 of the body under the secret in
// SignatureHeader (after SignaturePrefix). The JSON body becomes vars.payload; the run acts as the app, for nobody,
// with no record. A call that repeats the value of DedupeHeader returns the run the first one started.
type Webhook struct {
	Secret          string `json:"secret" yaml:"secret"`
	Auth            string `json:"auth" yaml:"auth"`
	SignatureHeader string `json:"signature_header,omitempty" yaml:"signature_header"`
	SignaturePrefix string `json:"signature_prefix,omitempty" yaml:"signature_prefix"`
	DedupeHeader    string `json:"dedupe_header,omitempty" yaml:"dedupe_header"`
}

// How a webhook's caller proves itself.
const (
	WebhookToken = "token" // Authorization: Bearer <secret>
	WebhookHMAC  = "hmac"  // a signature of the body
)

// Defaults for a webhook that does not name its headers. GitHub's and Stripe's need their own names.
const (
	DefaultSignatureHeader = "X-Signature-256"
	DefaultSignaturePrefix = "sha256="
	DefaultDedupeHeader    = "Idempotency-Key"
)

// MaxWebhookBody is the largest body a webhook accepts: what fits in a run's variables.
const MaxWebhookBody = 48 << 10

// Header names are HTTP tokens (RFC 9110).
var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,100}$")

// Header is the header a webhook's signature arrives in.
func (h *Webhook) Header() string {
	if h.SignatureHeader != "" {
		return h.SignatureHeader
	}
	return DefaultSignatureHeader
}

// Prefix is what may come before the hex signature in Header: sha256= unless the webhook says otherwise. A
// signature without it is accepted too.
func (h *Webhook) Prefix() string {
	if h.SignaturePrefix != "" {
		return h.SignaturePrefix
	}
	return DefaultSignaturePrefix
}

// Dedupe is the header whose value marks a repeated delivery.
func (h *Webhook) Dedupe() string {
	if h.DedupeHeader != "" {
		return h.DedupeHeader
	}
	return DefaultDedupeHeader
}

// problems checks the webhook on its own; that its secret is declared is checked with the manifest (checkWebhooks).
func (h *Webhook) problems() []string {
	var out []string
	if h.Secret == "" {
		out = append(out, "secret is required: the name of a secret in the app's manifest config")
	}
	switch h.Auth {
	case WebhookToken:
		if h.SignatureHeader != "" || h.SignaturePrefix != "" {
			out = append(out, "signature_header and signature_prefix are only for auth: hmac")
		}
	case WebhookHMAC:
	default:
		out = append(out, fmt.Sprintf("auth must be %s (Authorization: Bearer <secret>) or %s (a signature of the body), not %q", WebhookToken, WebhookHMAC, h.Auth))
	}
	for _, f := range []struct{ key, val string }{{"signature_header", h.SignatureHeader}, {"dedupe_header", h.DedupeHeader}} {
		if f.val != "" && !headerName.MatchString(f.val) {
			out = append(out, fmt.Sprintf("%s %q is not a header name", f.key, f.val))
		}
	}
	if h.Auth == WebhookHMAC && strings.EqualFold(h.Header(), "Authorization") {
		out = append(out, "signature_header cannot be Authorization")
	}
	if len(h.SignaturePrefix) > 50 {
		out = append(out, "signature_prefix is longer than 50 characters")
	}
	return out
}

// checkWebhooks checks that every webhook's secret is one its app declares.
func checkWebhooks(m *Manifest, defs []Definition, p *Problems) {
	if m == nil {
		return
	}
	secrets := map[string]bool{}
	for _, c := range m.Config {
		if c.Type == "secret" {
			secrets[c.Key] = true
		}
	}
	for _, d := range defs {
		if d.Workflow == nil || d.Workflow.Webhook == nil || d.Workflow.Webhook.Secret == "" {
			continue
		}
		if !secrets[d.Workflow.Webhook.Secret] {
			p.add(d.File, "webhook: the app declares no secret %q: add it to its manifest's config with type: secret", d.Workflow.Webhook.Secret)
		}
	}
}
