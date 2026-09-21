// Package jobs is the durable background job queue (docs/apps-platform.md §6).
//
// Jobs live in Postgres and are claimed with FOR UPDATE SKIP LOCKED, so any number of workers, in any number of
// processes, can pull from it at once without waiting on each other. Delivery is at-least-once: a handler may run
// more than once for the same job (a retry, or a worker that died), so handlers must be safe to repeat.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// Job is what a handler receives.
type Job struct {
	ID          int64
	Kind        string
	Payload     json.RawMessage
	Attempt     int // 1 on the first try
	MaxAttempts int
}

// Handler runs a job. Returning nil completes it, an error retries it with backoff (until attempts run out, then it
// is dead-lettered), and Permanent(err) dead-letters it at once.
type Handler func(ctx context.Context, job Job) error

type permanent struct{ err error }

func (p permanent) Error() string { return p.err.Error() }
func (p permanent) Unwrap() error { return p.err }

// Permanent marks an error that retrying cannot fix (for example a payload that will never parse), so the job goes
// straight to the dead letter instead of failing five times.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanent{err}
}

// EnqueueOptions controls one enqueue.
type EnqueueOptions struct {
	RunAt     time.Time // not before this time; zero means now
	DedupeKey string    // a second enqueue with the same key does nothing
}

// Enqueue adds a job inside the caller's transaction (q must be built on that transaction). If the transaction rolls
// back, the job disappears with it, so a change that never happened never triggers anything. It returns 0 when the
// dedupe key was already used.
func Enqueue(ctx context.Context, q *dbgen.Queries, kind string, payload any, opt EnqueueOptions) (int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("jobs: payload for %s: %w", kind, err)
	}
	run := opt.RunAt
	if run.IsZero() {
		run = time.Now()
	}
	arg := dbgen.EnqueueJobParams{Kind: kind, Payload: raw, RunAt: pgtype.Timestamptz{Time: run, Valid: true}}
	if opt.DedupeKey != "" {
		arg.DedupeKey = pgtype.Text{String: opt.DedupeKey, Valid: true}
	}
	id, err := q.EnqueueJob(ctx, arg)
	if err != nil {
		return 0, fmt.Errorf("jobs: enqueue %s: %w", kind, err)
	}
	return id, nil
}

// Decode reads a job's payload into v.
func (j Job) Decode(v any) error {
	if err := json.Unmarshal(j.Payload, v); err != nil {
		return Permanent(fmt.Errorf("job %d (%s): unreadable payload: %w", j.ID, j.Kind, err))
	}
	return nil
}

var errLostLease = errors.New("lost the lease on the job")
