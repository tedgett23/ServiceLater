package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/inbound"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

type inboundSettingsDTO struct {
	settings.Inbound
	// SecretSet says the IMAP password, or the Graph client secret (by mode), is stored (encrypted; never shown).
	PasswordSet    bool `json:"password_set"`
	GraphSecretSet bool `json:"graph_secret_set"`
	KeyConfigured  bool `json:"key_configured"`
}

func (h *handlers) inboundSettings(ctx context.Context, q *dbgen.Queries) (inboundSettingsDTO, error) {
	rows, err := q.ListSettings(ctx, settings.InboundPrefix)
	if err != nil {
		return inboundSettingsDTO{}, err
	}
	stored := map[string][]byte{}
	for _, r := range rows {
		stored[r.Name] = r.Value
	}
	out := inboundSettingsDTO{Inbound: settings.InboundFrom(stored), KeyConfigured: h.keys.Configured()}
	status, err := q.SecretStatus(ctx)
	for _, s := range status {
		switch s.Name {
		case settings.InboundPassword:
			out.PasswordSet = true
		case settings.InboundGraphSecret:
			out.GraphSecretSet = true
		}
	}
	return out, err
}

// GET /settings/inbound (settings:manage): how incoming mail is read, and what new mail may do.
func (h *handlers) getInboundSettings(w http.ResponseWriter, r *http.Request) {
	var out inboundSettingsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		out, err = h.inboundSettings(r.Context(), q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT /settings/inbound (settings:manage) with every setting, and password (IMAP) or graph_secret (Microsoft 365) to set
// them (null clears; leave out to keep). Replies to notifications become comments; with create_incidents, new mail
// from allowed_domains that passes DMARC files an incident.
func (h *handlers) putInboundSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		settings.Inbound
		// Password is the IMAP account's password or app password.
		Password nullable[string] `json:"password"`
		// GraphSecret is the app registration's client secret.
		GraphSecret nullable[string] `json:"graph_secret"`
	}
	if !decode(w, r, &req) {
		return
	}
	var out inboundSettingsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		in := req.Inbound
		if err := in.Check(); err != nil {
			return invalid(err.Error())
		}
		for _, s := range []struct {
			name  string
			value nullable[string]
		}{{settings.InboundPassword, req.Password}, {settings.InboundGraphSecret, req.GraphSecret}} {
			if !s.value.Set {
				continue
			}
			if s.value.Value == nil || *s.value.Value == "" {
				if err := secrets.Clear(r.Context(), q, s.name); err != nil {
					return err
				}
				continue
			}
			if !h.keys.Configured() {
				return invalid("the server has no secrets key, so it cannot store the password: set SL_SECRETS_KEY or SL_SECRETS_KEY_FILE and restart")
			}
			if err := secrets.Set(r.Context(), q, h.keys, s.name, *s.value.Value); err != nil {
				return err
			}
		}
		for name, value := range in.Rows() {
			if err := q.UpsertSetting(r.Context(), dbgen.UpsertSettingParams{Name: name, Value: value}); err != nil {
				return err
			}
		}
		var err error
		out, err = h.inboundSettings(r.Context(), q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /settings/inbound/test (settings:manage): connect to the mailbox with the stored settings and say how many
// messages are waiting, without reading or marking any.
func (h *handlers) testInbound(w http.ResponseWriter, r *http.Request) {
	var cfg settings.Inbound
	var secret string
	if err := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		var err error
		cfg, secret, err = inbound.Config(r.Context(), dbgen.New(tx), h.keys)
		return err
	}); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "detail": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	box, err := h.openMailbox(ctx, cfg, secret, nil)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "detail": err.Error()})
		return
	}
	defer func() { _ = box.Close() }()
	waiting, err := box.Unread(ctx, 1)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "detail": err.Error()})
		return
	}
	detail := "connected; no unread mail"
	if len(waiting) > 0 {
		detail = "connected; there is unread mail waiting"
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "detail": detail})
}

type inboundEmailDTO struct {
	ID   uuid.UUID `json:"id"`
	From string    `json:"from"`
	// Subject is the email's subject.
	Subject string `json:"subject"`
	// Action is comment, incident, ignored, refused or failed; Detail says why.
	Action string `json:"action"`
	Detail string `json:"detail"`
	// Reference is the record it was added to or filed as.
	Reference      string    `json:"reference,omitempty"`
	Attachments    int32     `json:"attachments"`
	Authentication string    `json:"authentication"`
	CreatedAt      time.Time `json:"created_at"`
}

// GET /settings/inbound/log (settings:manage): the latest 100 emails read, newest first, with what each became.
func (h *handlers) listInboundEmails(w http.ResponseWriter, r *http.Request) {
	out := []inboundEmailDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListInboundEmails(r.Context(), 100)
		for _, e := range rows {
			out = append(out, inboundEmailDTO{ID: e.ID, From: e.FromAddress, Subject: e.Subject, Action: e.Action,
				Detail: e.Detail, Reference: strings.TrimSpace(e.Reference), Attachments: e.Attachments,
				Authentication: e.Authentication, CreatedAt: e.CreatedAt.Time})
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
