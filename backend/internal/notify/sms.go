package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// SMSKind is the job that sends one queued text message (enqueued by the notifications_sms trigger, migration 00051).
const SMSKind = "notify.sms"

// maxSMS is how long a text may be: two segments, so a long title is cut rather than sent as five.
const maxSMS = 300

// Twilio sends text messages with Twilio's Messages API (or a compatible one at settings.SMS.APIBase).
type Twilio struct {
	// Client is the HTTP client; nil uses one with a 15 second timeout.
	Client *http.Client
}

// Send texts body to a number. A refusal Twilio will repeat (a bad number, a wrong account) is permanent; being
// rate-limited or unreachable is not.
func (t Twilio) Send(ctx context.Context, cfg settings.SMS, token, to, body string) error {
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	form := url.Values{"To": {to}, "Body": {body}}
	if strings.HasPrefix(cfg.From, "MG") {
		form.Set("MessagingServiceSid", cfg.From)
	} else {
		form.Set("From", cfg.From)
	}
	endpoint := strings.TrimRight(cfg.APIBase, "/") + "/2010-04-01/Accounts/" + url.PathEscape(cfg.AccountSID) + "/Messages.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return &SendError{Err: err, Permanent: true}
	}
	req.SetBasicAuth(cfg.AccountSID, token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return &SendError{Err: fmt.Errorf("reaching the SMS provider: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var problem struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	msg := resp.Status
	if json.Unmarshal(raw, &problem) == nil && problem.Message != "" {
		msg = fmt.Sprintf("%s (Twilio %d)", problem.Message, problem.Code)
	}
	return &SendError{Err: errors.New("the SMS provider refused: " + msg),
		Permanent: resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests}
}

// SMSConfig is the SMS settings and the auth token, read as the system. A token that is set but cannot be decrypted
// is an error, never an empty token.
func SMSConfig(ctx context.Context, q *dbgen.Queries, k *secrets.Keyring) (settings.SMS, string, error) {
	rows, err := q.GetSystemSettings(ctx, settings.SMSPrefix)
	if err != nil {
		return settings.SMS{}, "", err
	}
	stored := make(map[string][]byte, len(rows))
	for _, r := range rows {
		stored[r.Name] = r.Value
	}
	cfg := settings.SMSFrom(stored)
	token, err := secrets.Get(ctx, q, k, settings.SMSAuthToken)
	if errors.Is(err, secrets.ErrNotSet) {
		return cfg, "", errors.New("no auth token is set (Admin → Messaging)")
	}
	return cfg, token, err
}

// SMSText is a text message's words: the title, the body if there is room, and the link.
func SMSText(m Message, baseURL string) string {
	link := ""
	if baseURL != "" {
		link = " " + baseURL + m.Link
	}
	text := oneLine(m.Title)
	if b := oneLine(m.Body); b != "" {
		text += ": " + b
	}
	return clip(text, maxSMS-len([]rune(link))) + link
}

// baseURL is where people open ServiceLater (the email settings' base URL), for links in texts and posts.
func baseURL(ctx context.Context, q *dbgen.Queries) (string, error) {
	rows, err := q.GetSystemSettings(ctx, settings.EmailPrefix)
	stored := map[string][]byte{}
	for _, r := range rows {
		stored[r.Name] = r.Value
	}
	return settings.EmailFrom(stored).BaseURL, err
}

// Texter sends queued text messages, in the worker role.
type Texter struct {
	Pool   *pgxpool.Pool
	Apps   *apps.Registry
	Keys   *secrets.Keyring
	Sender interface {
		Send(ctx context.Context, cfg settings.SMS, token, to, body string) error
	}
}

// Handler sends one queued SMS, rendered as its recipient: it says no more than they could read in the app. One no
// longer queued, already read in the app, or that SMS was turned off for, is left; a refusal is final; anything else
// is tried again.
func (t *Texter) Handler() jobs.Handler {
	return func(ctx context.Context, job jobs.Job) error {
		var p struct {
			DeliveryID uuid.UUID `json:"delivery_id"`
		}
		if err := job.Decode(&p); err != nil {
			return jobs.Permanent(err)
		}
		var d dbgen.GetChannelDeliveryRow
		var cfg settings.SMS
		var token, base string
		var configErr error
		err := db.WithSystem(ctx, t.Pool, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			var err error
			if d, err = q.GetChannelDelivery(ctx, p.DeliveryID); err != nil {
				return err
			}
			cfg, token, configErr = SMSConfig(ctx, q, t.Keys)
			base, err = baseURL(ctx, q)
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.Status != StatusQueued {
			return nil
		}
		finish := func(status, detail string) error {
			return recordChannel(ctx, t.Pool, d.ID, status, detail, job.Attempt)
		}
		switch {
		case !cfg.Enabled:
			return finish(StatusSkipped, "SMS was turned off before it was sent")
		case !d.NotificationID.Valid || !d.UserID.Valid:
			return finish(StatusSkipped, "the notification was deleted before it was sent")
		case d.NotificationReadAt.Valid:
			return finish(StatusSkipped, "read in ServiceLater before it was sent")
		case configErr != nil:
			if err := finish(StatusFailed, configErr.Error()); err != nil {
				return err
			}
			return jobs.Permanent(configErr)
		}
		var msg Message
		var visible bool
		err = db.WithUser(ctx, t.Pool, d.UserID.UUID, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			n, err := q.GetNotificationForEmail(ctx, d.NotificationID.UUID)
			if err != nil {
				return err
			}
			msg, visible, err = NewRenderer(t.Apps, q).Render(ctx, n)
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return finish(StatusSkipped, "the notification is gone")
		}
		if err != nil {
			return err
		}
		if !visible {
			return finish(StatusSkipped, "the recipient can no longer see the record")
		}
		return sent(ctx, t.Pool, d.ID, job, t.Sender.Send(ctx, cfg, token, d.ToAddress, SMSText(msg, base)))
	}
}

// sent records how a send went: sent, failed for good, or queued to be tried again (the job retries).
func sent(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, job jobs.Job, err error) error {
	if err == nil {
		return recordChannel(ctx, pool, id, StatusSent, "", job.Attempt)
	}
	var se *SendError
	if (errors.As(err, &se) && se.Permanent) || job.Attempt >= job.MaxAttempts {
		if rerr := recordChannel(ctx, pool, id, StatusFailed, err.Error(), job.Attempt); rerr != nil {
			return rerr
		}
		return jobs.Permanent(err)
	}
	if rerr := recordChannel(ctx, pool, id, StatusQueued, fmt.Sprintf("try %d failed, trying again: %v", job.Attempt, err), job.Attempt); rerr != nil {
		return rerr
	}
	return err
}

func recordChannel(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, status, detail string, attempts int) error {
	return db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		return dbgen.New(tx).UpdateChannelDelivery(ctx, dbgen.UpdateChannelDeliveryParams{ID: id, Status: status,
			Detail: clip(detail, 2000), Attempts: int32(attempts)}) //nolint:gosec // attempts are a handful
	})
}
