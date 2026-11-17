package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/notify"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// SMSSender texts a message; notify.Twilio is the real one, tests use their own.
type SMSSender interface {
	Send(ctx context.Context, cfg settings.SMS, token, to, body string) error
}

// TeamsPoster posts to a Teams channel; notify.TeamsPoster is the real one.
type TeamsPoster interface {
	Post(ctx context.Context, webhook string, p notify.TeamsPost) error
}

type smsSettingsDTO struct {
	settings.SMS
	// TokenSet says the auth token is stored (encrypted; never shown), TokenReadable that this server's key opens it.
	TokenSet      bool `json:"token_set"`
	TokenReadable bool `json:"token_readable"`
	// KeyConfigured says the server has a secrets key; without one the token cannot be set.
	KeyConfigured bool `json:"key_configured"`
}

func (h *handlers) smsSettings(ctx context.Context, q *dbgen.Queries) (smsSettingsDTO, error) {
	rows, err := q.ListSettings(ctx, settings.SMSPrefix)
	if err != nil {
		return smsSettingsDTO{}, err
	}
	stored := map[string][]byte{}
	for _, r := range rows {
		stored[r.Name] = r.Value
	}
	out := smsSettingsDTO{SMS: settings.SMSFrom(stored), KeyConfigured: h.keys.Configured()}
	status, err := q.SecretStatus(ctx)
	for _, s := range status {
		if s.Name == settings.SMSAuthToken {
			out.TokenSet, out.TokenReadable = true, h.keys.Has(s.KeyID)
		}
	}
	return out, err
}

// GET /settings/sms (settings:manage): how text messages are sent (Twilio), and whether the auth token is stored.
func (h *handlers) getSMSSettings(w http.ResponseWriter, r *http.Request) {
	var out smsSettingsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		out, err = h.smsSettings(r.Context(), q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT /settings/sms (settings:manage) with every setting, and auth_token to set it (null clears it; leave it out to keep
// it). Turning SMS on needs the account SID, a sender and a token.
func (h *handlers) putSMSSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		settings.SMS
		// AuthToken is the Twilio account's auth token (or an API key secret), encrypted before it is stored.
		AuthToken nullable[string] `json:"auth_token"`
	}
	if !decode(w, r, &req) {
		return
	}
	var out smsSettingsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		s := req.SMS
		if err := s.Check(); err != nil {
			return invalid(err.Error())
		}
		if req.AuthToken.Set {
			switch {
			case req.AuthToken.Value == nil || *req.AuthToken.Value == "":
				if err := secrets.Clear(r.Context(), q, settings.SMSAuthToken); err != nil {
					return err
				}
			case !h.keys.Configured():
				return invalid("the server has no secrets key, so it cannot store the auth token: set SL_SECRETS_KEY or SL_SECRETS_KEY_FILE and restart")
			default:
				if err := secrets.Set(r.Context(), q, h.keys, settings.SMSAuthToken, strings.TrimSpace(*req.AuthToken.Value)); err != nil {
					return err
				}
			}
		}
		for name, value := range s.Rows() {
			if err := q.UpsertSetting(r.Context(), dbgen.UpsertSettingParams{Name: name, Value: value}); err != nil {
				return err
			}
		}
		var err error
		if out, err = h.smsSettings(r.Context(), q); err != nil {
			return err
		}
		if s.Enabled && !out.TokenSet {
			return invalid("turning SMS on needs the auth token")
		}
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// sendSMSNow texts a message straight away, as the system, with the stored settings, and logs it. For tests and
// verification codes, which someone is waiting for; notifications go through the job queue.
func (h *handlers) sendSMSNow(ctx context.Context, userID uuid.UUID, kind, to, body string) error {
	var cfg settings.SMS
	var token string
	err := db.WithSystem(ctx, h.pool, func(tx pgx.Tx) error {
		var err error
		cfg, token, err = notify.SMSConfig(ctx, dbgen.New(tx), h.keys)
		return err
	})
	if err == nil && (cfg.AccountSID == "" || cfg.From == "") {
		err = errors.New("SMS is not set up: it needs an account SID and a sender (Admin → Messaging)")
	}
	if err == nil {
		err = h.smsSender.Send(ctx, cfg, token, to, body)
	}
	status, detail := notify.StatusSent, ""
	if err != nil {
		status, detail = notify.StatusFailed, err.Error()
	}
	_ = db.WithSystem(context.WithoutCancel(ctx), h.pool, func(tx pgx.Tx) error {
		_, lerr := dbgen.New(tx).InsertChannelDelivery(ctx, dbgen.InsertChannelDeliveryParams{Channel: "sms",
			UserID: uuid.NullUUID{UUID: userID, Valid: userID != uuid.Nil}, ToAddress: to, Kind: kind, Status: status, Detail: clipText(detail, 2000)})
		return lerr
	})
	return err
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// POST /settings/sms/test (settings:manage) with {"to"}: text a test message now, with the settings as they are
// stored (SMS need not be on). Answers what the provider said.
func (h *handlers) testSMS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		// To is a number in international form, such as +15551234567.
		To string `json:"to"`
	}
	if !decode(w, r, &req) {
		return
	}
	to, ok := phoneNumber(req.To)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "to must be a number in international form, such as +15551234567")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if err := h.sendSMSNow(r.Context(), p.UserID, "test", to, "A test message from ServiceLater: SMS works."); err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": notify.StatusFailed, "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": notify.StatusSent})
}

var phoneRE = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

// phoneNumber is a number as typed, in international form: spaces, dashes, dots and brackets go; a leading 00 is +.
func phoneNumber(raw string) (string, bool) {
	s := strings.Map(func(c rune) rune {
		if strings.ContainsRune(" -.()", c) {
			return -1
		}
		return c
	}, strings.TrimSpace(raw))
	if strings.HasPrefix(s, "00") {
		s = "+" + s[2:]
	}
	return s, phoneRE.MatchString(s)
}

// ---- Teams ----

type teamsGroupDTO struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Connected says the group has a Teams channel (its webhook is stored, encrypted; never shown).
	Connected bool `json:"connected"`
}

type teamsSettingsDTO struct {
	settings.Teams
	KeyConfigured bool            `json:"key_configured"`
	Groups        []teamsGroupDTO `json:"groups"`
}

func (h *handlers) teamsSettings(ctx context.Context, q *dbgen.Queries) (teamsSettingsDTO, error) {
	rows, err := q.ListSettings(ctx, settings.TeamsPrefix)
	if err != nil {
		return teamsSettingsDTO{}, err
	}
	stored := map[string][]byte{}
	for _, r := range rows {
		stored[r.Name] = r.Value
	}
	out := teamsSettingsDTO{Teams: settings.TeamsFrom(stored), KeyConfigured: h.keys.Configured(), Groups: []teamsGroupDTO{}}
	status, err := q.SecretStatus(ctx)
	if err != nil {
		return out, err
	}
	names := make([]string, len(status))
	for i, s := range status {
		names[i] = s.Name
	}
	groups, err := q.ListGroups(ctx)
	for _, g := range groups {
		out.Groups = append(out.Groups, teamsGroupDTO{ID: g.ID, Name: g.Name, Connected: slices.Contains(names, settings.TeamsWebhook(g.ID))})
	}
	return out, err
}

// GET /settings/teams (settings:manage): whether notifications post to Teams, and which groups have a channel.
func (h *handlers) getTeamsSettings(w http.ResponseWriter, r *http.Request) {
	var out teamsSettingsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		var err error
		out, err = h.teamsSettings(r.Context(), q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT /settings/teams (settings:manage) with {"enabled"}: turn posting to groups' Teams channels on or off.
func (h *handlers) putTeamsSettings(w http.ResponseWriter, r *http.Request) {
	var req settings.Teams
	if !decode(w, r, &req) {
		return
	}
	var out teamsSettingsDTO
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		for name, value := range req.Rows() {
			if err := q.UpsertSetting(r.Context(), dbgen.UpsertSettingParams{Name: name, Value: value}); err != nil {
				return err
			}
		}
		var err error
		out, err = h.teamsSettings(r.Context(), q)
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT /groups/{id}/teams (settings:manage) with {"webhook"}: connect a group to a Teams channel. The webhook is made in
// Teams (the channel's Workflows: "Post to a channel when a webhook request is received") and stored encrypted;
// anyone with it can post to the channel, so it is never shown again. A test card is posted first: a webhook that
// does not work is not saved.
func (h *handlers) putGroupTeams(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		// Webhook is the address Teams gave for posting to the channel.
		Webhook string `json:"webhook"`
	}
	if !decode(w, r, &req) {
		return
	}
	webhook := strings.TrimSpace(req.Webhook)
	if err := settings.CheckTeamsWebhook(webhook); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !h.keys.Configured() {
		writeError(w, http.StatusUnprocessableEntity, "the server has no secrets key, so it cannot store the webhook: set SL_SECRETS_KEY or SL_SECRETS_KEY_FILE and restart")
		return
	}
	var name string
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		g, err := q.GetGroup(r.Context(), id)
		name = g.Name
		return err
	}) {
		return
	}
	post := notify.TeamsPost{Title: "ServiceLater is connected", Text: "Notifications for the " + name + " group will appear in this channel."}
	err := h.teamsPoster.Post(r.Context(), webhook, post)
	h.logTeams(r.Context(), id, name, "test", err)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "the test post did not reach Teams, so the channel was not saved: "+err.Error())
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		return secrets.Set(r.Context(), q, h.keys, settings.TeamsWebhook(id), webhook)
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /groups/{id}/teams (settings:manage): disconnect a group from its Teams channel.
func (h *handlers) deleteGroupTeams(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		return secrets.Clear(r.Context(), q, settings.TeamsWebhook(id))
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) logTeams(ctx context.Context, group uuid.UUID, name, kind string, err error) {
	status, detail := notify.StatusSent, ""
	if err != nil {
		status, detail = notify.StatusFailed, err.Error()
	}
	_ = db.WithSystem(context.WithoutCancel(ctx), h.pool, func(tx pgx.Tx) error {
		_, lerr := dbgen.New(tx).InsertChannelDelivery(ctx, dbgen.InsertChannelDeliveryParams{Channel: "teams",
			GroupID: uuid.NullUUID{UUID: group, Valid: true}, ToAddress: name, Kind: kind, Status: status, Detail: clipText(detail, 2000)})
		return lerr
	})
}

// ---- the delivery log ----

type channelDeliveryDTO struct {
	ID uuid.UUID `json:"id"`
	// Channel is sms or teams.
	Channel string `json:"channel"`
	// To is the number (SMS) or the group (Teams).
	To       string  `json:"to"`
	UserName *string `json:"user_name"`
	// Kind is notification, test or verification.
	Kind string `json:"kind"`
	// What is the template it was (app/template).
	What string `json:"what"`
	// Status is queued, sent, failed or skipped; Detail says why, for the last three.
	Status    string    `json:"status"`
	Detail    string    `json:"detail"`
	Attempts  int32     `json:"attempts"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GET /settings/messaging/deliveries?channel= (settings:manage): the latest 100 text messages and Teams posts, newest
// first. channel is sms or teams (both when left out).
func (h *handlers) listChannelDeliveries(w http.ResponseWriter, r *http.Request) {
	var channel pgtype.Text
	if c := r.URL.Query().Get("channel"); c != "" {
		if c != "sms" && c != "teams" {
			writeError(w, http.StatusBadRequest, "channel must be sms or teams")
			return
		}
		channel = pgtype.Text{String: c, Valid: true}
	}
	out := []channelDeliveryDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListChannelDeliveries(r.Context(), dbgen.ListChannelDeliveriesParams{Channel: channel, Limit: 100})
		for _, d := range rows {
			o := channelDeliveryDTO{ID: d.ID, Channel: d.Channel, To: d.ToAddress, Kind: d.Kind, What: d.What, Status: d.Status,
				Detail: d.Detail, Attempts: d.Attempts, CreatedAt: d.CreatedAt.Time, UpdatedAt: d.UpdatedAt.Time}
			if d.UserName.Valid {
				o.UserName = &d.UserName.String
			}
			out = append(out, o)
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// ---- a person's phone ----

const (
	codeTTL        = 10 * time.Minute
	maxCodesAnHour = 5
)

func codeHash(userID uuid.UUID, code string) []byte {
	sum := sha256.Sum256([]byte(userID.String() + ":" + code))
	return sum[:]
}

// PUT /account/phone with {"number"}: set your mobile number for SMS notifications. A six-digit code is texted to it
// at once; the number counts, and SMS turns on, once you send the code back (POST /account/phone/verify). At most five
// codes an hour.
func (h *handlers) putMyPhone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		// Number is in international form, such as +15551234567 (spaces and dashes are fine).
		Number string `json:"number"`
	}
	if !decode(w, r, &req) {
		return
	}
	number, ok := phoneNumber(req.Number)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "the number must be in international form, starting with + and the country code (+15551234567)")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	var tries int32
	if err := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		var err error
		tries, err = dbgen.New(tx).HitRateLimit(r.Context(), dbgen.HitRateLimitParams{Key: "sms-code:" + p.UserID.String(), WindowSecs: 3600})
		return err
	}); err != nil {
		writeDBError(w, err)
		return
	}
	if tries > maxCodesAnHour {
		w.Header().Set("Retry-After", "3600")
		writeError(w, http.StatusTooManyRequests, "too many codes sent this hour; try again later")
		return
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	code := fmt.Sprintf("%06d", n.Int64())
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		return q.SetMyPhone(r.Context(), dbgen.SetMyPhoneParams{Number: number, CodeHash: codeHash(p.UserID, code),
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(codeTTL), Valid: true}})
	}) {
		return
	}
	if err := h.sendSMSNow(r.Context(), p.UserID, "verification", number, "Your ServiceLater code is "+code+". It expires in 10 minutes."); err != nil {
		writeError(w, http.StatusBadGateway, "the code could not be texted: "+err.Error())
		return
	}
	if out, ok := h.accountNow(w, r); ok {
		writeJSON(w, http.StatusOK, out)
	}
}

// POST /account/phone/verify with {"code"}: confirm your number with the code texted to it. Five tries per code.
func (h *handlers) verifyMyPhone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	code := strings.TrimSpace(req.Code)
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		ok, err := q.VerifyMyPhone(r.Context(), codeHash(p.UserID, code))
		if err == nil && !ok {
			return invalid("that code is wrong or has expired; send a new one if it has")
		}
		return err
	}) {
		return
	}
	if out, ok := h.accountNow(w, r); ok {
		writeJSON(w, http.StatusOK, out)
	}
}

// DELETE /account/phone: remove your number; no more text messages.
func (h *handlers) deleteMyPhone(w http.ResponseWriter, r *http.Request) {
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error { return q.ClearMyPhone(r.Context()) }) {
		return
	}
	if out, ok := h.accountNow(w, r); ok {
		writeJSON(w, http.StatusOK, out)
	}
}

// accountNow reads the caller's account afresh, for the phone handlers to answer with; false when it has already
// answered with an error.
func (h *handlers) accountNow(w http.ResponseWriter, r *http.Request) (accountDTO, bool) {
	var out accountDTO
	ok := h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		var err error
		out, err = h.account(r, q, p)
		return err
	})
	return out, ok
}
