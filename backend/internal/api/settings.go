package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/notify"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

const maxDeliveries = 100

// emailSettingsDTO is how this instance sends email. The password is never sent back: only whether it is set.
type emailSettingsDTO struct {
	settings.Email
	// PasswordSet says the mail server password is stored (encrypted); PasswordUpdatedAt and PasswordUpdatedBy say
	// when and by whom it was last set.
	PasswordSet       bool       `json:"password_set"`
	PasswordUpdatedAt *time.Time `json:"password_updated_at"`
	PasswordUpdatedBy *string    `json:"password_updated_by"`
	// PasswordReadable is false when the password was stored with a key this server does not have, so it must be set
	// again.
	PasswordReadable bool `json:"password_readable"`
	// KeyConfigured says the server has a secrets key (SL_SECRETS_KEY or SL_SECRETS_KEY_FILE); without one the
	// password cannot be set.
	KeyConfigured bool `json:"key_configured"`
}

// emailSettingsRequest replaces the email settings. password sets the mail server password, null clears it, and
// leaving it out keeps it.
type emailSettingsRequest struct {
	settings.Email
	Password nullable[string] `json:"password"`
}

func (h *handlers) emailSettings(ctx context.Context, q *dbgen.Queries) (emailSettingsDTO, error) {
	rows, err := q.ListSettings(ctx, settings.EmailPrefix)
	if err != nil {
		return emailSettingsDTO{}, err
	}
	stored := make(map[string][]byte, len(rows))
	for _, r := range rows {
		stored[r.Name] = r.Value
	}
	out := emailSettingsDTO{Email: settings.EmailFrom(stored), KeyConfigured: h.keys.Configured()}
	status, err := q.SecretStatus(ctx)
	for _, s := range status {
		if s.Name == settings.EmailPassword {
			out.PasswordSet, out.PasswordReadable = true, h.keys.Has(s.KeyID)
			at := s.UpdatedAt.Time
			out.PasswordUpdatedAt = &at
			if s.UpdatedByName != "" {
				by := s.UpdatedByName
				out.PasswordUpdatedBy = &by
			}
		}
	}
	return out, err
}

// GET /settings/email (settings:manage): how this instance sends email, without the password.
func (h *handlers) getEmailSettings(w http.ResponseWriter, r *http.Request) {
	var out emailSettingsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		out, err = h.emailSettings(r.Context(), q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT /settings/email (settings:manage) with every setting, and password to set it (null clears it; leave it out to
// keep it). The password is encrypted before it is stored, which needs the server's secrets key. Turning email on
// needs a host and a from address.
func (h *handlers) putEmailSettings(w http.ResponseWriter, r *http.Request) {
	var req emailSettingsRequest
	if !decode(w, r, &req) {
		return
	}
	var out emailSettingsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		e := req.Email
		if err := e.Check(); err != nil {
			return invalid(err.Error())
		}
		for name, value := range e.Rows() {
			if err := q.UpsertSetting(r.Context(), dbgen.UpsertSettingParams{Name: name, Value: value}); err != nil {
				return err
			}
		}
		switch {
		case !req.Password.Set:
		case req.Password.Value == nil || *req.Password.Value == "":
			if err := secrets.Clear(r.Context(), q, settings.EmailPassword); err != nil {
				return err
			}
		case !h.keys.Configured():
			return invalid("the server has no secrets key, so it cannot store a password: set SL_SECRETS_KEY or SL_SECRETS_KEY_FILE and restart")
		case len(*req.Password.Value) > 1000:
			return invalid("password is too long")
		default:
			if err := secrets.Set(r.Context(), q, h.keys, settings.EmailPassword, *req.Password.Value); err != nil {
				return err
			}
		}
		var err error
		out, err = h.emailSettings(r.Context(), q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// testEmailRequest asks for a test email to an address; the caller's own when left out.
type testEmailRequest struct {
	To string `json:"to"`
}

// testEmailDTO is how a test email went: sent, or failed with what the mail server said.
type testEmailDTO struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// POST /settings/email/test (settings:manage) with {"to"?}: send a test email now with the saved settings, whether
// email is on or not, and answer with what the mail server said. It is recorded in the delivery log.
func (h *handlers) testEmail(w http.ResponseWriter, r *http.Request) {
	var req testEmailRequest
	if !decode(w, r, &req) {
		return
	}
	var to, name string
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		if _, err := q.ListSettings(r.Context(), settings.EmailPrefix); err != nil {
			return err // RLS: only those who manage settings get this far
		}
		u, err := q.GetUser(r.Context(), p.UserID)
		if err != nil {
			return err
		}
		to, name = strings.TrimSpace(req.To), ""
		if to == "" {
			to, name = u.Email, u.Name
		}
		if a, err := mail.ParseAddress(to); err != nil || a.Name != "" {
			return invalid("to must be an email address")
		}
		return nil
	}) {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	var e settings.Email
	var password string
	err := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		var err error
		e, password, err = notify.Config(r.Context(), dbgen.New(tx), h.keys)
		return err
	})
	out := testEmailDTO{Status: notify.StatusSent}
	if err == nil {
		err = h.sender.Send(r.Context(), e, password, notify.Email{To: to, ToName: name, Subject: "ServiceLater test email",
			Body: "This is a test email from ServiceLater. If you can read it, email is set up.\n"})
	}
	if err != nil {
		out = testEmailDTO{Status: notify.StatusFailed, Detail: err.Error()}
	}
	if rerr := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).InsertEmailDelivery(r.Context(), dbgen.InsertEmailDeliveryParams{UserID: uuid.NullUUID{UUID: p.UserID, Valid: true},
			ToAddress: to, Status: out.Status, Detail: out.Detail})
		return err
	}); rerr != nil {
		writeDBError(w, rerr)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// deliveryDTO is one email in the log.
type deliveryDTO struct {
	ID        uuid.UUID `json:"id"`
	ToAddress string    `json:"to_address"`
	// UserName is who it was for, when it was for someone.
	UserName *string `json:"user_name"`
	// Kind is notification or test; What says which notification (a template, or approval_pending).
	Kind string `json:"kind"`
	What string `json:"what,omitempty"`
	// Status is queued, sent, failed or skipped; Detail says why it failed or was skipped.
	Status    string    `json:"status"`
	Detail    string    `json:"detail"`
	Attempts  int32     `json:"attempts"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GET /settings/email/deliveries (settings:manage): the latest 100 emails, newest first, with how each went.
func (h *handlers) listEmailDeliveries(w http.ResponseWriter, r *http.Request) {
	out := []deliveryDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListEmailDeliveries(r.Context(), maxDeliveries)
		for _, d := range rows {
			out = append(out, deliveryDTO{ID: d.ID, ToAddress: d.ToAddress, UserName: textPtr(d.UserName), Kind: d.Kind, What: d.What,
				Status: d.Status, Detail: d.Detail, Attempts: d.Attempts, CreatedAt: d.CreatedAt.Time, UpdatedAt: d.UpdatedAt.Time})
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// accountDTO is the signed-in person's own preferences.
type accountDTO struct {
	Email string `json:"email"`
	// EmailNotifications says notifications also reach them by email (when the instance sends email).
	EmailNotifications bool `json:"email_notifications"`
	// EmailEnabled says the instance sends email at all.
	EmailEnabled bool `json:"email_enabled"`
	// Theme is how the web UI is drawn for them: auto (follow the device), light or dark.
	Theme string `json:"theme"`
	// SMSEnabled says the instance sends text messages; Phone is their mobile number (empty when none), PhoneVerified
	// that they confirmed it with the code texted to it, and SMSNotifications that notifications also reach them by
	// SMS (only those whose template asks).
	SMSEnabled       bool   `json:"sms_enabled"`
	Phone            string `json:"phone"`
	PhoneVerified    bool   `json:"phone_verified"`
	SMSNotifications bool   `json:"sms_notifications"`
}

func (h *handlers) account(r *http.Request, q *dbgen.Queries, p auth.Principal) (accountDTO, error) {
	u, err := q.GetUser(r.Context(), p.UserID)
	if err != nil {
		return accountDTO{}, err
	}
	out := accountDTO{Email: u.Email, EmailNotifications: u.EmailNotifications, Theme: u.Theme}
	// Whether email is on is not a secret, but the settings are for those who manage them: ask as the system.
	phone, err := q.GetMyPhone(r.Context())
	if err == nil {
		out.Phone, out.PhoneVerified, out.SMSNotifications = phone.Number, phone.VerifiedAt.Valid, phone.SmsNotifications
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	err = db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		rows, err := q.GetSystemSettings(r.Context(), settings.EmailPrefix+"enabled")
		for _, row := range rows {
			out.EmailEnabled = string(row.Value) == "true"
		}
		if err != nil {
			return err
		}
		rows, err = q.GetSystemSettings(r.Context(), settings.SMSPrefix+"enabled")
		for _, row := range rows {
			out.SMSEnabled = string(row.Value) == "true"
		}
		return err
	})
	return out, err
}

// GET /account: the signed-in person's own preferences.
func (h *handlers) getAccount(w http.ResponseWriter, r *http.Request) {
	var out accountDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		var err error
		out, err = h.account(r, q, p)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// accountRequest changes the signed-in person's own preferences.
type accountRequest struct {
	EmailNotifications *bool `json:"email_notifications"`
	// SMSNotifications turns text messages on or off (once the number is verified).
	SMSNotifications *bool `json:"sms_notifications"`
	// Theme is auto, light or dark.
	Theme *string `json:"theme"`
}

// themes are the values accountRequest.Theme may take, mirrored by the users_theme CHECK.
var themes = map[string]bool{"auto": true, "light": true, "dark": true}

// PATCH /account with {"email_notifications"} and/or {"theme"}: turn email notifications on or off for yourself (in-app
// notifications are not affected), or choose how the web UI is drawn.
func (h *handlers) patchAccount(w http.ResponseWriter, r *http.Request) {
	var req accountRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Theme != nil && !themes[*req.Theme] {
		writeError(w, http.StatusUnprocessableEntity, "theme must be auto, light or dark")
		return
	}
	var out accountDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		if req.Theme != nil {
			if _, err := q.SetMyTheme(r.Context(), dbgen.SetMyThemeParams{ID: p.UserID, Theme: *req.Theme}); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("updating your own account: %w", err)
				}
				return err
			}
		}
		if req.SMSNotifications != nil {
			changed, err := q.SetMySMSNotifications(r.Context(), *req.SMSNotifications)
			if err != nil {
				return err
			}
			if !changed {
				return invalid("add and verify a mobile number first")
			}
		}
		if req.EmailNotifications != nil {
			if _, err := q.SetMyEmailNotifications(r.Context(), dbgen.SetMyEmailNotificationsParams{ID: p.UserID, EmailNotifications: *req.EmailNotifications}); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("updating your own account: %w", err)
				}
				return err
			}
		}
		var err error
		out, err = h.account(r, q, p)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
