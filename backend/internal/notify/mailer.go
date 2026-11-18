package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

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

// EmailKind is the job that sends one queued email (enqueued by the notifications_email trigger).
const EmailKind = "notify.email"

// Statuses of an email delivery.
const (
	StatusQueued  = "queued"
	StatusSent    = "sent"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
)

// Mailer sends queued emails. It runs in the worker role, which needs the secrets key to sign in to the mail server.
type Mailer struct {
	Pool   *pgxpool.Pool
	Apps   *apps.Registry
	Keys   *secrets.Keyring
	Sender Sender
}

// Config is the email settings and the server's password, read as the system. A password that is set but cannot be
// decrypted (no key, or the wrong one) is an error, never an empty password.
func Config(ctx context.Context, q *dbgen.Queries, k *secrets.Keyring) (settings.Email, string, error) {
	rows, err := q.GetSystemSettings(ctx, settings.EmailPrefix)
	if err != nil {
		return settings.Email{}, "", err
	}
	stored := make(map[string][]byte, len(rows))
	for _, r := range rows {
		stored[r.Name] = r.Value
	}
	e := settings.EmailFrom(stored)
	password, err := secrets.Get(ctx, q, k, settings.EmailPassword)
	if errors.Is(err, secrets.ErrNotSet) {
		return e, "", nil
	}
	return e, password, err
}

// Handler sends one delivery. It is a no-op for one that is no longer queued. An email that cannot be sent for good
// (a refusal, no key) is marked failed at once; one that might go later (no connection) is tried again by the job
// queue, and marked failed after the last try.
func (m *Mailer) Handler() jobs.Handler {
	return func(ctx context.Context, job jobs.Job) error {
		var p struct {
			DeliveryID uuid.UUID `json:"delivery_id"`
		}
		if err := job.Decode(&p); err != nil {
			return jobs.Permanent(err)
		}
		var d dbgen.GetEmailDeliveryRow
		var e settings.Email
		var password string
		var configErr error
		err := db.WithSystem(ctx, m.Pool, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			var err error
			if d, err = q.GetEmailDelivery(ctx, p.DeliveryID); err != nil {
				return err
			}
			e, password, configErr = Config(ctx, q, m.Keys)
			return nil
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // gone: its recipient was deleted
		}
		if err != nil {
			return err
		}
		if d.Status != StatusQueued {
			return nil
		}
		finish := func(status, detail string) error {
			return m.record(ctx, d.ID, status, detail, job.Attempt)
		}
		switch {
		case !e.Enabled:
			return finish(StatusSkipped, "email was turned off before it was sent")
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

		// Filled in as the recipient: an email never says more than they could read in the app.
		var msg Message
		var visible bool
		err = db.WithUser(ctx, m.Pool, d.UserID.UUID, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			n, err := q.GetNotificationForEmail(ctx, d.NotificationID.UUID)
			if err != nil {
				return err
			}
			msg, visible, err = NewRenderer(m.Apps, q).Render(ctx, n)
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
		out := Email{To: d.ToAddress, ToName: d.UserName.String, Subject: msg.Title, Body: Compose(msg, e.BaseURL),
			NotificationID: msg.ID.String()}
		// While incoming mail is on, a reply to a notification about a record becomes a comment on it: the reply finds
		// its way back by this Message-ID (inbound.NotificationMessageID).
		if replyTo := m.replyAddress(ctx); replyTo != "" && msg.TaskID != nil {
			out.MessageID, out.ReplyTo = "n."+msg.ID.String()+"@servicelater", replyTo
			out.Body = "Reply to this email to comment on " + msg.Reference + ".\n\n" + out.Body
		}
		err = m.Sender.Send(ctx, e, password, out)
		if err == nil {
			return finish(StatusSent, "")
		}
		var se *SendError
		if (errors.As(err, &se) && se.Permanent) || job.Attempt >= job.MaxAttempts {
			if rerr := finish(StatusFailed, err.Error()); rerr != nil {
				return rerr
			}
			return jobs.Permanent(err)
		}
		if rerr := m.record(ctx, d.ID, StatusQueued, fmt.Sprintf("try %d failed, trying again: %v", job.Attempt, err), job.Attempt); rerr != nil {
			return rerr
		}
		return err
	}
}

// replyAddress is the incoming mailbox's address while incoming mail is on, else "".
func (m *Mailer) replyAddress(ctx context.Context) string {
	var addr string
	_ = db.WithSystem(ctx, m.Pool, func(tx pgx.Tx) error {
		rows, err := dbgen.New(tx).GetSystemSettings(ctx, settings.InboundPrefix)
		stored := map[string][]byte{}
		for _, r := range rows {
			stored[r.Name] = r.Value
		}
		if in := settings.InboundFrom(stored); in.Enabled {
			addr = in.Address
		}
		return err
	})
	return addr
}

func (m *Mailer) record(ctx context.Context, id uuid.UUID, status, detail string, attempts int) error {
	return db.WithSystem(ctx, m.Pool, func(tx pgx.Tx) error {
		return dbgen.New(tx).UpdateEmailDelivery(ctx, dbgen.UpdateEmailDeliveryParams{ID: id, Status: status,
			Detail: clip(detail, 2000), Attempts: int32(attempts)}) //nolint:gosec // attempts are a handful
	})
}

// Compose is an email's text: the notification, where to open it, and how to stop these emails.
func Compose(m Message, baseURL string) string {
	var b strings.Builder
	b.WriteString(m.Title)
	b.WriteString("\n")
	if m.Body != "" {
		b.WriteString("\n" + m.Body + "\n")
	}
	if baseURL != "" {
		b.WriteString("\nOpen it in ServiceLater: " + baseURL + m.Link + "\n")
		b.WriteString("\n-- \nYou get these emails because email notifications are on for you.\nTurn them off: " + baseURL + "/account\n")
	} else {
		b.WriteString("\n-- \nYou get these emails because email notifications are on for you. Turn them off on your account page in ServiceLater.\n")
	}
	return b.String()
}

func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}
