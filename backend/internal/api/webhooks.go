package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/workflow"
)

// How many calls one webhook takes in a minute, refused ones included (so a secret cannot be guessed at speed).
const (
	webhookPerMinute = 300
	maxDeliveryKey   = 200
)

// webhookStarted is what a call that reached its workflow gets back.
type webhookStarted struct {
	// RunID is the workflow run the call started (GET /api/v1/workflow-runs/{id}, for those who may read runs).
	RunID uuid.UUID `json:"run_id"`
	// Duplicate is true when an earlier call with the same dedupe header started RunID, and this one started nothing.
	Duplicate bool `json:"duplicate,omitempty"`
}

// POST /hooks/{app}/{workflow}: starts a workflow whose definition has a webhook. No session: the caller proves
// itself with the webhook's secret, as Authorization: Bearer <secret> or a signature of the body (HMAC-SHA256, hex,
// in the webhook's signature header), and the run acts as the app, for nobody, with the JSON body as vars.payload.
// A call repeating the value of the webhook's dedupe header (Idempotency-Key unless it names another) gets the first
// call's run back with 200 and starts nothing. Every call is logged without its body (GET
// /workflows/{app}/{name}/deliveries).
func (h *handlers) receiveWebhook(w http.ResponseWriter, r *http.Request) {
	app, name := chi.URLParam(r, "app"), chi.URLParam(r, "workflow")
	wf := h.apps.Workflow(app, name)
	if wf == nil || wf.Webhook == nil {
		writeError(w, http.StatusNotFound, "there is no webhook here")
		return
	}
	hook := wf.Webhook
	ctx := r.Context()

	var calls int32
	if err := db.WithSystem(ctx, h.pool, func(tx pgx.Tx) error {
		var err error
		calls, err = dbgen.New(tx).HitRateLimit(ctx, dbgen.HitRateLimitParams{Key: "webhook:" + app + "/" + name, WindowSecs: time.Minute.Seconds()})
		return err
	}); err != nil {
		writeDBError(w, err)
		return
	}
	if calls > webhookPerMinute {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "this webhook has had too many calls this minute")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, apps.MaxWebhookBody+1))
	if err != nil || len(body) > apps.MaxWebhookBody {
		h.logDelivery(r, wf, "", "refused", "the body is larger than 48 KB", len(body))
		writeError(w, http.StatusRequestEntityTooLarge, "the body is larger than 48 KB")
		return
	}
	key := strings.TrimSpace(r.Header.Get(hook.Dedupe()))
	if len(key) > maxDeliveryKey {
		key = key[:maxDeliveryKey]
	}

	var secret string
	err = db.WithSystem(ctx, h.pool, func(tx pgx.Tx) error {
		var err error
		secret, err = secrets.Get(ctx, dbgen.New(tx), h.keys, apps.AppSecretName(app, hook.Secret))
		return err
	})
	switch {
	case errors.Is(err, secrets.ErrNotSet) || (err == nil && secret == ""):
		h.logDelivery(r, wf, key, "refused", "the webhook's secret is not set (Admin → Connections)", len(body))
		writeError(w, http.StatusUnauthorized, "this webhook does not accept calls yet")
		return
	case err != nil:
		slog.Error("reading a webhook's secret", "app", app, "workflow", name, "err", err)
		writeError(w, http.StatusServiceUnavailable, "the webhook's secret cannot be read")
		return
	}
	if why := verifyWebhook(hook, secret, r.Header, body); why != "" {
		h.logDelivery(r, wf, key, "refused", why, len(body))
		writeError(w, http.StatusUnauthorized, why)
		return
	}

	var payload any
	if len(bytes.TrimSpace(body)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&payload); err != nil || dec.More() {
			h.logDelivery(r, wf, key, "refused", "the body is not JSON", len(body))
			writeError(w, http.StatusBadRequest, "the body must be JSON")
			return
		}
	}

	var out webhookStarted
	err = db.WithSystem(ctx, h.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		delivery, err := q.InsertWebhookDelivery(ctx, dbgen.InsertWebhookDeliveryParams{App: app, Workflow: name,
			DeliveryKey: textOrNull(key), Outcome: "started", Size: int32(len(body)), Remote: remoteOf(r)}) //nolint:gosec // at most MaxWebhookBody
		if errors.Is(err, pgx.ErrNoRows) {
			// An earlier call with this key started a run: answer with it, and note the repeat.
			first, err := q.FindWebhookDelivery(ctx, dbgen.FindWebhookDeliveryParams{App: app, Workflow: name, DeliveryKey: textOrNull(key)})
			if err != nil {
				return err
			}
			out = webhookStarted{RunID: first.UUID, Duplicate: true}
			_, err = q.InsertWebhookDelivery(ctx, dbgen.InsertWebhookDeliveryParams{App: app, Workflow: name,
				DeliveryKey: textOrNull(key), Outcome: "duplicate", ExecutionID: first, Size: int32(len(body)), Remote: remoteOf(r)}) //nolint:gosec // as above
			return err
		}
		if err != nil {
			return err
		}
		principal, err := q.AppPrincipal(ctx, app)
		if err != nil {
			return err
		}
		vars := map[string]any{"payload": payload, "webhook": map[string]any{"delivery": key, "received_at": time.Now().UTC().Format(time.RFC3339)}}
		id, err := workflow.Start(ctx, q, workflow.StartParams{Def: wf, UserID: principal, Vars: vars})
		if err != nil {
			return err
		}
		out.RunID = id
		return q.SetWebhookDeliveryRun(ctx, dbgen.SetWebhookDeliveryRunParams{ID: delivery, ExecutionID: uuid.NullUUID{UUID: id, Valid: true}})
	})
	if err != nil {
		slog.Error("starting a workflow from a webhook", "app", app, "workflow", name, "err", err)
		writeError(w, http.StatusInternalServerError, "the workflow could not be started")
		return
	}
	if out.Duplicate {
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

// verifyWebhook says why a call does not prove it knows the secret, or "" when it does.
func verifyWebhook(hook *apps.Webhook, secret string, hdr http.Header, body []byte) string {
	switch hook.Auth {
	case apps.WebhookToken:
		got, ok := strings.CutPrefix(hdr.Get("Authorization"), "Bearer ")
		if !ok || got == "" {
			return "Authorization: Bearer <token> is required"
		}
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(secret)) != 1 {
			return "the token is wrong"
		}
		return ""
	case apps.WebhookHMAC:
		raw := strings.TrimSpace(hdr.Get(hook.Header()))
		if raw == "" {
			return "the " + hook.Header() + " header with the body's signature is required"
		}
		sig, err := hex.DecodeString(strings.TrimPrefix(raw, hook.Prefix()))
		if err != nil {
			return "the signature in " + hook.Header() + " is not hex"
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if !hmac.Equal(sig, mac.Sum(nil)) {
			return "the signature does not match the body"
		}
		return ""
	}
	return "the webhook's auth is not one this server knows"
}

// logDelivery records a refused call. A failure to record it is logged and does not change the answer.
func (h *handlers) logDelivery(r *http.Request, wf *apps.Workflow, key, outcome, reason string, size int) {
	err := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).InsertWebhookDelivery(r.Context(), dbgen.InsertWebhookDeliveryParams{App: wf.App, Workflow: wf.Name,
			DeliveryKey: textOrNull(key), Outcome: outcome, Reason: reason, Size: int32(size), Remote: remoteOf(r)}) //nolint:gosec // at most MaxWebhookBody+1
		return err
	})
	if err != nil {
		slog.Error("recording a webhook call", "app", wf.App, "workflow", wf.Name, "err", err)
	}
}

// remoteOf is the address the call came from, as this server saw it (a proxy's, behind one).
func remoteOf(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

// webhookDeliveryDTO is one call to a webhook.
type webhookDeliveryDTO struct {
	ID uuid.UUID `json:"id"`
	// DeliveryKey is the value of the webhook's dedupe header, when the call had one.
	DeliveryKey string `json:"delivery_key"`
	// Outcome is started, duplicate (an earlier call with the same key started the run) or refused.
	Outcome string `json:"outcome"`
	// Reason says why a call was refused.
	Reason string `json:"reason"`
	// RunID is the run it started or repeated; null when refused.
	RunID *uuid.UUID `json:"run_id"`
	// Size is the body's size in bytes.
	Size int32 `json:"size"`
	// Remote is the address the call came from, as the server saw it.
	Remote     string    `json:"remote"`
	ReceivedAt time.Time `json:"received_at"`
}

// GET /workflows/{app}/{name}/deliveries (workflows:read_all): a webhook's latest 50 calls, newest first.
func (h *handlers) listWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	out := []webhookDeliveryDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rows, err := q.ListWebhookDeliveries(r.Context(), dbgen.ListWebhookDeliveriesParams{App: chi.URLParam(r, "app"), Workflow: chi.URLParam(r, "name")})
		for _, d := range rows {
			item := webhookDeliveryDTO{ID: d.ID, DeliveryKey: d.DeliveryKey.String, Outcome: d.Outcome, Reason: d.Reason,
				Size: d.Size, Remote: d.Remote, ReceivedAt: d.ReceivedAt.Time}
			if d.ExecutionID.Valid {
				item.RunID = &d.ExecutionID.UUID
			}
			out = append(out, item)
		}
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// textOrNull is s, or null when it is empty.
func textOrNull(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
