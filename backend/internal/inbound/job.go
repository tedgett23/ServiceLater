package inbound

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// FetchKind is the job that reads the mailbox once (queued every minute by RunTimer while incoming mail is on).
const FetchKind = "inbound.fetch"

// perFetch is how many messages one run reads; the rest wait for the next minute.
const perFetch = 25

// Config is the incoming-mail settings and the IMAP password or Graph client secret, read as the system.
func Config(ctx context.Context, q *dbgen.Queries, k *secrets.Keyring) (settings.Inbound, string, error) {
	rows, err := q.GetSystemSettings(ctx, settings.InboundPrefix)
	if err != nil {
		return settings.Inbound{}, "", err
	}
	stored := map[string][]byte{}
	for _, r := range rows {
		stored[r.Name] = r.Value
	}
	cfg := settings.InboundFrom(stored)
	name := settings.InboundPassword
	if cfg.Mode == settings.InboundGraph {
		name = settings.InboundGraphSecret
	}
	secret, err := secrets.Get(ctx, q, k, name)
	if errors.Is(err, secrets.ErrNotSet) {
		return cfg, "", nil
	}
	return cfg, secret, err
}

// Reader reads the mailbox and hands each message to the processor.
type Reader struct {
	Pool      *pgxpool.Pool
	Keys      *secrets.Keyring
	Processor *Processor
	// TLSConfig is for IMAP (tests trust their own server); nil verifies against the system's roots.
	TLSConfig *tls.Config
	// Open connects to the mailbox; nil is the real Open.
	Open func(ctx context.Context, cfg settings.Inbound, secret string, tlsConfig *tls.Config) (Mailbox, error)
}

// Handler reads up to perFetch unread messages, acts on each and marks it read. A message whose handling hits a
// passing fault (the database) is left unread for the next run; one that cannot be acted on is logged and marked
// read, so it is not read again. A mailbox that cannot be reached is retried by the job queue.
func (r *Reader) Handler() jobs.Handler {
	return func(ctx context.Context, _ jobs.Job) error {
		var cfg settings.Inbound
		var secret string
		if err := db.WithSystem(ctx, r.Pool, func(tx pgx.Tx) error {
			var err error
			cfg, secret, err = Config(ctx, dbgen.New(tx), r.Keys)
			return err
		}); err != nil {
			return err
		}
		if !cfg.Enabled {
			return nil
		}
		open := r.Open
		if open == nil {
			open = Open
		}
		box, err := open(ctx, cfg, secret, r.TLSConfig)
		if err != nil {
			return fmt.Errorf("opening the mailbox: %w", err)
		}
		defer func() { _ = box.Close() }()
		msgs, err := box.Unread(ctx, perFetch)
		for _, m := range msgs {
			o := Outcome{MessageID: "too-large:" + m.ID, Action: ActionFailed, Detail: fmt.Sprintf("the message is %d MB, more than ServiceLater reads", m.Size>>20)}
			if m.Raw != nil {
				var herr error
				if o, herr = r.Processor.Handle(ctx, cfg, m.Raw); herr != nil {
					slog.Error("handling an email", "err", herr)
					continue // left unread for the next run
				}
			}
			if !o.Duplicate {
				if lerr := r.log(ctx, o); lerr != nil {
					return lerr
				}
			}
			if derr := box.Done(ctx, m.ID); derr != nil {
				return fmt.Errorf("marking a message read: %w", derr)
			}
		}
		return err
	}
}

func (r *Reader) log(ctx context.Context, o Outcome) error {
	return db.WithSystem(context.WithoutCancel(ctx), r.Pool, func(tx pgx.Tx) error {
		return dbgen.New(tx).LogInbound(ctx, dbgen.LogInboundParams{MessageID: o.MessageID, FromAddress: o.From,
			Subject: clipRunes(o.Subject, 1000), Action: o.Action, Detail: clipRunes(o.Detail, 2000), TaskID: o.TaskID,
			UserID: o.UserID, Attachments: int32(o.Files), Authentication: o.Auth}) //nolint:gosec // at most maxFiles
	})
}

// RunTimer queues a mailbox read every interval while incoming mail is on, until ctx ends. Each interval's job has
// its own dedupe key, so several workers queue it once.
func RunTimer(ctx context.Context, pool *pgxpool.Pool, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
				q := dbgen.New(tx)
				rows, err := q.GetSystemSettings(ctx, settings.InboundPrefix+"enabled")
				if err != nil || len(rows) == 0 || string(rows[0].Value) != "true" {
					return err
				}
				_, err = jobs.Enqueue(ctx, q, FetchKind, struct{}{}, jobs.EnqueueOptions{
					DedupeKey: fmt.Sprintf("inbound-fetch:%d", now.Unix()/int64(interval.Seconds()))})
				return err
			})
			if err != nil && ctx.Err() == nil {
				slog.Error("queueing a mailbox read", "err", err)
			}
		}
	}
}
