package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// Channel is the NOTIFY channel that wakes idle workers when a job is enqueued.
const Channel = "jobs"

// Options tunes a worker. The zero value is usable: every field has a default.
type Options struct {
	Concurrency  int           // executors running at once (default 4)
	PollInterval time.Duration // safety-net poll when no notification arrives (default 5s)
	JobTimeout   time.Duration // one attempt may run this long (default 60s)
	BaseBackoff  time.Duration // first retry delay, doubling each time (default 5s)
	MaxBackoff   time.Duration // ceiling on the retry delay (default 10m)
	StuckAfter   time.Duration // a job running longer than this is presumed orphaned (default 10m)
	ReapEvery    time.Duration // how often to recover orphans and purge old jobs (default 30s)
	KeepDone     time.Duration // finished jobs are deleted after this (default 7 days)
	KeepFailed   time.Duration // dead letters are kept this long (default 30 days)
	KeepRuns     time.Duration // the rule-run audit is kept this long (default 30 days)
	Grace        time.Duration // on shutdown, wait this long for running jobs before cancelling them (default 30s)

	// DSN, when set, is used to LISTEN for new jobs so they start within milliseconds. Without it the worker polls.
	DSN string
}

func (o Options) withDefaults() Options {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	def(&o.PollInterval, 5*time.Second)
	def(&o.JobTimeout, 60*time.Second)
	def(&o.BaseBackoff, 5*time.Second)
	def(&o.MaxBackoff, 10*time.Minute)
	def(&o.StuckAfter, 10*time.Minute)
	def(&o.ReapEvery, 30*time.Second)
	def(&o.KeepDone, 7*24*time.Hour)
	def(&o.KeepFailed, 30*24*time.Hour)
	def(&o.KeepRuns, 30*24*time.Hour)
	def(&o.Grace, 30*time.Second)
	return o
}

// Worker pulls jobs and runs them on a bounded pool of executors. The executors share nothing and do not
// coordinate: the queue hands each one different work.
type Worker struct {
	pool     *pgxpool.Pool
	opt      Options
	id       string
	mu       sync.RWMutex
	handlers map[string]Handler
	wake     chan struct{}
}

func NewWorker(pool *pgxpool.Pool, opt Options) *Worker {
	opt = opt.withDefaults()
	host, _ := os.Hostname()
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	return &Worker{
		pool:     pool,
		opt:      opt,
		id:       fmt.Sprintf("%s/%d/%s", host, os.Getpid(), hex.EncodeToString(suffix)),
		handlers: map[string]Handler{},
		wake:     make(chan struct{}, opt.Concurrency),
	}
}

// Register sets the handler for a kind of job. Register before Run.
func (w *Worker) Register(kind string, h Handler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers[kind] = h
}

// Run works until ctx ends, then stops claiming new jobs, lets running ones finish for up to Grace, and returns.
func (w *Worker) Run(ctx context.Context) {
	// Running jobs are cancelled only after the grace period, not the moment shutdown starts.
	jobCtx, killJobs := context.WithCancel(context.Background())
	defer killJobs()

	var wg sync.WaitGroup
	for i := 0; i < w.opt.Concurrency; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w.executor(ctx, jobCtx, fmt.Sprintf("%s#%d", w.id, n))
		}(i)
	}
	if w.opt.DSN != "" {
		go db.Listen(ctx, w.opt.DSN, []string{Channel},
			func(bool) { w.signal() }, // after a (re)connect, look for anything missed
			func(string, string) { w.signal() })
	}
	go w.maintenance(ctx)

	<-ctx.Done()
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(w.opt.Grace):
		slog.Warn("jobs still running after the grace period, cancelling them", "grace", w.opt.Grace)
		killJobs()
		<-drained
	}
}

// signal wakes idle executors. It never blocks: a full channel means enough are already awake.
func (w *Worker) signal() {
	for i := 0; i < cap(w.wake); i++ {
		select {
		case w.wake <- struct{}{}:
		default:
			return
		}
	}
}

func (w *Worker) executor(ctx, jobCtx context.Context, lockerID string) {
	for ctx.Err() == nil {
		job, ok := w.claim(ctx, lockerID)
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-w.wake:
			case <-time.After(w.opt.PollInterval):
			}
			continue
		}
		w.execute(jobCtx, lockerID, job)
	}
}

func (w *Worker) claim(ctx context.Context, lockerID string) (Job, bool) {
	var row dbgen.Job
	err := db.WithSystem(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		row, err = dbgen.New(tx).ClaimJob(ctx, pgTextPtr(lockerID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false
	}
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("claiming a job", "err", err)
			time.Sleep(time.Second) // do not spin on a database problem
		}
		return Job{}, false
	}
	return Job{ID: row.ID, Kind: row.Kind, Payload: row.Payload, Attempt: int(row.Attempts), MaxAttempts: int(row.MaxAttempts)}, true
}

// execute runs one attempt and records its outcome. Recording uses its own short deadline and is not cancelled by
// shutdown, so a job that finished is never left looking unfinished.
func (w *Worker) execute(jobCtx context.Context, lockerID string, job Job) {
	w.mu.RLock()
	h, known := w.handlers[job.Kind]
	w.mu.RUnlock()

	start := time.Now()
	var err error
	if !known {
		// A newer version may have enqueued a kind this one lacks; retrying lets an upgraded worker pick it up.
		err = fmt.Errorf("no handler is registered for job kind %q", job.Kind)
	} else {
		err = w.call(jobCtx, h, job)
	}

	rec, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch {
	case err == nil:
		w.record(rec, job, "complete", func(q *dbgen.Queries) (int64, error) {
			return q.CompleteJob(rec, dbgen.CompleteJobParams{ID: job.ID, LockedBy: pgTextPtr(lockerID)})
		})
		slog.Debug("job done", "id", job.ID, "kind", job.Kind, "attempt", job.Attempt, "took", time.Since(start))
	case isPermanent(err):
		slog.Error("job failed permanently", "id", job.ID, "kind", job.Kind, "err", err)
		w.record(rec, job, "dead-letter", func(q *dbgen.Queries) (int64, error) {
			return q.DeadLetterJob(rec, dbgen.DeadLetterJobParams{ID: job.ID, LockedBy: pgTextPtr(lockerID), LastError: pgTextPtr(truncate(err.Error()))})
		})
	default:
		level := slog.LevelWarn
		if job.Attempt >= job.MaxAttempts {
			level = slog.LevelError
		}
		slog.Log(rec, level, "job failed", "id", job.ID, "kind", job.Kind, "attempt", job.Attempt, "of", job.MaxAttempts, "err", err)
		w.record(rec, job, "retry", func(q *dbgen.Queries) (int64, error) {
			return q.FailJob(rec, dbgen.FailJobParams{
				ID: job.ID, LockedBy: pgTextPtr(lockerID), Error: truncate(err.Error()),
				BaseBackoffSecs: w.opt.BaseBackoff.Seconds(), MaxBackoffSecs: w.opt.MaxBackoff.Seconds(),
			})
		})
	}
}

// call runs the handler with a timeout, turning a panic into an error so one bad job cannot take a worker down.
func (w *Worker) call(ctx context.Context, h Handler, job Job) (err error) {
	ctx, cancel := context.WithTimeout(ctx, w.opt.JobTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return h(ctx, job)
}

func (w *Worker) record(ctx context.Context, job Job, what string, fn func(*dbgen.Queries) (int64, error)) {
	var n int64
	err := db.WithSystem(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		n, err = fn(dbgen.New(tx))
		return err
	})
	switch {
	case err != nil:
		slog.Error("recording a job outcome", "id", job.ID, "outcome", what, "err", err)
	case n == 0:
		// The job was presumed orphaned and given to someone else while this ran. Their outcome stands.
		slog.Warn("job outcome ignored: "+errLostLease.Error(), "id", job.ID, "outcome", what)
	}
}

// maintenance recovers orphaned jobs and purges old ones. It is idempotent, so every worker may run it.
func (w *Worker) maintenance(ctx context.Context) {
	t := time.NewTicker(w.opt.ReapEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.sweep(ctx)
		}
	}
}

func (w *Worker) sweep(ctx context.Context) {
	err := db.WithSystem(ctx, w.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		reaped, err := q.ReapStuckJobs(ctx, w.opt.StuckAfter.Seconds())
		if err != nil {
			return err
		}
		purged, err := q.PurgeFinishedJobs(ctx, dbgen.PurgeFinishedJobsParams{
			DoneAfterSecs: w.opt.KeepDone.Seconds(), FailedAfterSecs: w.opt.KeepFailed.Seconds()})
		if err != nil {
			return err
		}
		if _, err := q.PurgeActionRuns(ctx, w.opt.KeepRuns.Seconds()); err != nil {
			return err
		}
		if reaped > 0 {
			slog.Warn("recovered jobs from a lost worker", "jobs", reaped)
			w.signal()
		}
		if purged > 0 {
			slog.Info("purged finished jobs", "jobs", purged)
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		slog.Error("job maintenance", "err", err)
	}
}

// IsPermanent reports whether err was marked with Permanent, so it goes straight to the dead letter.
func IsPermanent(err error) bool { return isPermanent(err) }

func isPermanent(err error) bool {
	var p permanent
	return errors.As(err, &p)
}

// truncate keeps an error message from bloating a row.
func truncate(s string) string {
	const max = 4000
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func pgTextPtr(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
