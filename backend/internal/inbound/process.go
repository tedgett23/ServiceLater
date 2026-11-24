package inbound

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/settings"
	"github.com/servicelater/servicelater/backend/internal/storage"
)

// Actions an email can end in, as the log records them.
const (
	ActionComment  = "comment"
	ActionIncident = "incident"
	ActionIgnored  = "ignored" // not for us to act on: a program's mail, our own, a duplicate
	ActionRefused  = "refused" // a person's mail that failed a check
	ActionFailed   = "failed"  // could not be read or acted on
)

// NotificationMessageID is the Message-ID a notification email is sent with. A reply names it in In-Reply-To, and
// the notification's id, random and only in that person's email, ties the reply to them and to the record.
func NotificationMessageID(notification uuid.UUID) string {
	return "n." + notification.String() + "@servicelater"
}

// notificationFor finds the notification a message replies to, in its In-Reply-To and References.
func notificationFor(m *Mail) (uuid.UUID, bool) {
	for _, id := range append(slices.Clone(m.InReplyTo), m.References...) {
		rest, ok := strings.CutPrefix(id, "n.")
		if !ok {
			continue
		}
		raw, _, _ := strings.Cut(rest, "@")
		if u, err := uuid.Parse(raw); err == nil {
			return u, true
		}
	}
	return uuid.Nil, false
}

// Outcome is what became of a message, as it is logged.
type Outcome struct {
	MessageID, From, Subject string
	Action, Detail           string
	TaskID, UserID           uuid.NullUUID
	Files                    int
	Auth                     string
	// Duplicate is a message already handled: it is marked read again, and not logged again.
	Duplicate bool
}

// Processor acts on messages.
type Processor struct {
	Pool  *pgxpool.Pool
	Apps  *apps.Registry
	Files *storage.Manager
	// LookupTXT is DNS for DKIM and DMARC (nil: the system's resolver).
	LookupTXT func(string) ([]string, error)
}

func refuse(o Outcome, format string, args ...any) Outcome {
	o.Action, o.Detail = ActionRefused, fmt.Sprintf(format, args...)
	return o
}

func ignore(o Outcome, detail string) Outcome {
	o.Action, o.Detail = ActionIgnored, detail
	return o
}

// Handle works out what a message is and acts on it. It never returns an error for a message's own faults: those are
// outcomes, logged. An error means trying the message again later (the database is unreachable).
func (p *Processor) Handle(ctx context.Context, cfg settings.Inbound, raw []byte) (Outcome, error) {
	m, err := Parse(raw)
	if err != nil {
		sum := sha256.Sum256(raw)
		return Outcome{MessageID: "sha256:" + hex.EncodeToString(sum[:]), Action: ActionFailed, Detail: err.Error()}, nil
	}
	o := Outcome{MessageID: m.MessageID, From: m.From, Subject: clipRunes(m.Subject, 1000)}
	if o.MessageID == "" {
		sum := sha256.Sum256(raw)
		o.MessageID = "sha256:" + hex.EncodeToString(sum[:])
	}
	var seen bool
	if err := db.WithSystem(ctx, p.Pool, func(tx pgx.Tx) error {
		var err error
		seen, err = dbgen.New(tx).InboundSeen(ctx, o.MessageID)
		return err
	}); err != nil {
		return o, err
	}
	switch {
	case seen:
		o.Duplicate = true
		return o, nil
	case m.Automatic:
		return ignore(o, "sent by a program (an auto-reply, a bounce or a mailing list), so not answered"), nil
	case m.From == "":
		return ignore(o, "it has no sender"), nil
	case strings.EqualFold(m.From, cfg.Address):
		return ignore(o, "it is from ServiceLater's own mailbox"), nil
	}
	auth := Verifier{LookupTXT: p.LookupTXT, TrustedAuthServ: cfg.TrustedAuthServ}.Check(raw, m)
	o.Auth = auth.String()
	if id, ok := notificationFor(m); ok {
		return p.reply(ctx, o, m, auth, id)
	}
	return p.newIncident(ctx, cfg, o, m, auth)
}

// reply comments on the record a notification was about, as the person it was sent to.
func (p *Processor) reply(ctx context.Context, o Outcome, m *Mail, auth Auth, notification uuid.UUID) (Outcome, error) {
	var n dbgen.GetNotificationForReplyRow
	err := db.WithSystem(ctx, p.Pool, func(tx pgx.Tx) error {
		var err error
		n, err = dbgen.New(tx).GetNotificationForReply(ctx, notification)
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ignore(o, "it answers a notification that no longer exists"), nil
	case err != nil:
		return o, err
	}
	o.UserID = uuid.NullUUID{UUID: n.UserID, Valid: true}
	switch {
	case !strings.EqualFold(n.Email, m.From):
		return refuse(o, "it was sent from %s, not the address the notification went to", m.From), nil
	case n.DeactivatedAt.Valid:
		return refuse(o, "the person it is from is deactivated"), nil
	case auth.DMARC == DMARCFail:
		return refuse(o, "the sender's domain does not vouch for it (DMARC fail)"), nil
	case !n.TaskID.Valid:
		return ignore(o, "it answers an approval request; approvals are decided in ServiceLater"), nil
	}
	o.TaskID = n.TaskID
	text := Reply(m.Text)
	if text == "" && len(m.Files) == 0 {
		return ignore(o, "the reply is empty (only the quoted message)"), nil
	}
	err = db.WithUser(ctx, p.Pool, n.UserID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		t, err := q.GetTask(ctx, n.TaskID.UUID)
		if err != nil {
			return err
		}
		if o.Files, err = p.attach(ctx, q, t, m.Files); err != nil {
			return err
		}
		if text == "" {
			return nil
		}
		// As the person would comment in the app: the rules on: [comment] tell the assignee.
		a, err := records.ActorIn(ctx, q, n.UserID)
		if err != nil {
			return err
		}
		svc := &records.Service{Apps: p.Apps, Pool: p.Pool}
		_, err = svc.Comment(ctx, q, a, ruleruns.Cause{}, t.ID, records.CommentInput{Body: clipRunes(text+filesNote(m.Dropped), records.MaxComment)})
		return err
	})
	if err != nil {
		if denied(err) {
			return refuse(o, "the person it is from may not comment on that record any more"), nil
		}
		return o, err
	}
	o.Action, o.Detail = ActionComment, "added to the record as a comment"
	if text == "" {
		o.Detail = "its files were attached to the record"
	}
	return o, nil
}

// newIncident files an email that answers nothing as an incident, when every check passes.
func (p *Processor) newIncident(ctx context.Context, cfg settings.Inbound, o Outcome, m *Mail, auth Auth) (Outcome, error) {
	domain := m.Domain()
	switch {
	case !cfg.CreateIncidents:
		return ignore(o, "it answers no notification, and filing incidents by email is off"), nil
	case !slices.Contains(cfg.AllowedDomains, strings.ToLower(domain)):
		return refuse(o, "%s is not a domain allowed to file incidents by email", domain), nil
	case auth.DMARC != DMARCPass:
		return refuse(o, "the sender's domain does not vouch for it (DMARC %s)", auth.DMARC), nil
	}
	var userID uuid.UUID
	var recent int64
	err := db.WithSystem(ctx, p.Pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var err error
		if recent, err = q.RecentInboundIncidents(ctx, m.From); err != nil || recent >= int64(cfg.MaxPerHour) {
			return err
		}
		u, err := q.GetSenderByEmail(ctx, m.From)
		switch {
		case errors.Is(err, pgx.ErrNoRows) && cfg.CreateAccounts:
			name := strings.TrimSpace(m.FromName)
			if name == "" {
				name = m.From
			}
			nu, err := q.CreateUser(ctx, dbgen.CreateUserParams{Email: m.From, Name: clipRunes(name, 200), Role: "end_user"})
			userID = nu.ID
			return err
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return err
		case u.DeactivatedAt.Valid:
			return nil
		}
		userID = u.ID
		return nil
	})
	switch {
	case err != nil:
		return o, err
	case recent >= int64(cfg.MaxPerHour):
		return refuse(o, "%s already filed %d incidents by email this hour", m.From, recent), nil
	case userID == uuid.Nil:
		return refuse(o, "there is no active ServiceLater account for %s", m.From), nil
	}
	o.UserID = uuid.NullUUID{UUID: userID, Valid: true}
	svc := &records.Service{Apps: p.Apps, Pool: p.Pool}
	title := strings.TrimSpace(strings.Join(strings.Fields(m.Subject), " "))
	if title == "" {
		title = "Email from " + m.From
	}
	desc := strings.TrimSpace(m.Text)
	err = db.WithUser(ctx, p.Pool, userID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		perms, err := q.PermissionsOf(ctx, userID)
		if err != nil {
			return err
		}
		t, err := svc.Create(ctx, q, records.Actor{ID: userID, Perms: authz.Parse("," + strings.Join(perms, ",") + ",")}, ruleruns.Cause{},
			records.CreateInput{Type: "incident", Title: clipRunes(title, records.MaxTitle), Description: clipRunes(desc+filesNote(m.Dropped), records.MaxDescription)})
		if err != nil {
			return err
		}
		o.TaskID = uuid.NullUUID{UUID: t.ID, Valid: true}
		task, err := q.GetTask(ctx, t.ID)
		if err != nil {
			return err
		}
		o.Files, err = p.attach(ctx, q, task, m.Files)
		return err
	})
	if err != nil {
		var re *records.Error
		if errors.As(err, &re) || denied(err) {
			return refuse(o, "the incident could not be filed: %v", err), nil
		}
		return o, err
	}
	o.Action, o.Detail = ActionIncident, "filed as an incident"
	return o, nil
}

func denied(err error) bool {
	return errors.Is(err, pgx.ErrNoRows) || strings.Contains(err.Error(), "SQLSTATE 42501")
}

func filesNote(dropped int) string {
	if dropped == 0 {
		return ""
	}
	return fmt.Sprintf("\n\n(%d attached file(s) were left out: too many or too large.)", dropped)
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// attach stores an email's files and attaches them to the record, as the person the email is from (the same storage,
// size limit and permission as an upload: the requester, or someone who may update it).
func (p *Processor) attach(ctx context.Context, q *dbgen.Queries, t dbgen.Task, files []File) (int, error) {
	if len(files) == 0 || p.Files == nil {
		return 0, nil
	}
	store, cfg, err := p.Files.Current(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range files {
		if int64(len(f.Data)) > cfg.MaxSize || len(f.Data) == 0 {
			continue
		}
		id := uuid.New()
		prefix := ""
		if cfg.Backend == settings.BackendS3 {
			prefix = cfg.S3Prefix
		}
		key := storage.Key(prefix, id)
		ct := storage.ContentType(f.ContentType)
		partSize, parts := storage.Plan(int64(len(f.Data)))
		upload, err := store.Begin(ctx, key, ct)
		if err != nil {
			return n, err
		}
		var done []storage.Part
		for i := 0; i < parts; i++ {
			from := int64(i) * partSize
			to := min(from+partSize, int64(len(f.Data)))
			etag, err := store.PutPart(ctx, key, upload, i+1, from, to-from, bytes.NewReader(f.Data[from:to]))
			if err != nil {
				_ = store.Abort(context.WithoutCancel(ctx), key, upload)
				return n, err
			}
			done = append(done, storage.Part{Number: i + 1, ETag: etag})
		}
		if err := store.Complete(ctx, key, upload, done, int64(len(f.Data))); err != nil {
			return n, err
		}
		if _, err := q.CreateAttachment(ctx, dbgen.CreateAttachmentParams{ID: id, TaskID: t.ID, Filename: cleanName(f.Name),
			ContentType: ct, Size: int64(len(f.Data)), Backend: cfg.Backend, Key: key, PartSize: partSize}); err != nil {
			_ = store.Delete(context.WithoutCancel(ctx), key)
			return n, err
		}
		if _, err := q.FinishAttachment(ctx, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// cleanName is a file's name without folders or control characters.
func cleanName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name))
	if name == "" || name == "." || name == ".." {
		name = "attachment"
	}
	if len(name) > 255 {
		name = name[:255]
	}
	return name
}
