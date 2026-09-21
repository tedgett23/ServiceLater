package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
	"github.com/servicelater/servicelater/backend/internal/jobs"
)

func enqueue(t *testing.T, pool *pgxpool.Pool, kind string, payload any, opt jobs.EnqueueOptions) int64 {
	t.Helper()
	var id int64
	err := db.WithSystem(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		id, err = jobs.Enqueue(context.Background(), dbgen.New(tx), kind, payload, opt)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// fast options: short delays so tests do not wait on production timings.
func fast(o jobs.Options) jobs.Options {
	if o.PollInterval == 0 {
		o.PollInterval = 20 * time.Millisecond
	}
	if o.BaseBackoff == 0 {
		o.BaseBackoff = 20 * time.Millisecond
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = time.Second
	}
	if o.ReapEvery == 0 {
		o.ReapEvery = time.Hour
	}
	if o.Grace == 0 {
		o.Grace = 2 * time.Second
	}
	return o
}

// run starts a worker and stops it, waiting for it to drain, when the test ends.
func run(t *testing.T, pool *pgxpool.Pool, opt jobs.Options, register func(*jobs.Worker)) (stop func()) {
	t.Helper()
	w := jobs.NewWorker(pool, fast(opt))
	register(w)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the worker did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type row struct {
	State     string
	Attempts  int
	LastError string
}

func jobRow(t *testing.T, pool *pgxpool.Pool, id int64) row {
	t.Helper()
	var r row
	var lastErr pgtype.Text
	if err := pool.QueryRow(context.Background(), `SELECT state, attempts, last_error FROM jobs WHERE id = $1`, id).Scan(&r.State, &r.Attempts, &lastErr); err != nil {
		t.Fatalf("job %d: %v", id, err)
	}
	r.LastError = lastErr.String
	return r
}

func TestEveryJobRunsExactlyOnceUnderContention(t *testing.T) {
	pool := dbtest.Pool(t)
	const total, concurrency = 300, 8

	var mu sync.Mutex
	seen := map[int64]int{}
	var inFlight, peak atomic.Int32
	run(t, pool, jobs.Options{Concurrency: concurrency, DSN: dbtest.DSN(t)}, func(w *jobs.Worker) {
		w.Register("count.me", func(ctx context.Context, j jobs.Job) error {
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			inFlight.Add(-1)
			mu.Lock()
			seen[j.ID]++
			mu.Unlock()
			return nil
		})
	})
	for i := 0; i < total; i++ {
		enqueue(t, pool, "count.me", map[string]int{"n": i}, jobs.EnqueueOptions{})
	}

	waitFor(t, 20*time.Second, "all jobs to finish", func() bool {
		var n int
		_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE state = 'done'`).Scan(&n)
		return n == total
	})
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != total {
		t.Errorf("%d distinct jobs ran, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("job %d ran %d times, want exactly once", id, n)
		}
	}
	if p := peak.Load(); p > concurrency {
		t.Errorf("%d handlers ran at once, the limit is %d", p, concurrency)
	} else if p < 2 {
		t.Errorf("only %d handler ever ran at once: the pool is not concurrent", p)
	}
}

func TestAFailingJobRetriesWithGrowingBackoffThenIsDeadLettered(t *testing.T) {
	pool := dbtest.Pool(t)
	var mu sync.Mutex
	var times []time.Time
	run(t, pool, jobs.Options{Concurrency: 1, BaseBackoff: 30 * time.Millisecond}, func(w *jobs.Worker) {
		w.Register("always.fails", func(context.Context, jobs.Job) error {
			mu.Lock()
			times = append(times, time.Now())
			mu.Unlock()
			return errors.New("boom")
		})
	})
	id := enqueue(t, pool, "always.fails", nil, jobs.EnqueueOptions{})

	waitFor(t, 10*time.Second, "the job to be dead-lettered", func() bool { return jobRow(t, pool, id).State == "failed" })
	r := jobRow(t, pool, id)
	if r.Attempts != 5 || r.LastError != "boom" {
		t.Errorf("attempts %d, last error %q", r.Attempts, r.LastError)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(times) != 5 {
		t.Fatalf("the handler ran %d times, want 5", len(times))
	}
	// Delays are 30, 60, 120, 240 ms: each at least a little more than the one before.
	for i := 2; i < len(times); i++ {
		prev, cur := times[i-1].Sub(times[i-2]), times[i].Sub(times[i-1])
		if cur < prev+prev/3 {
			t.Errorf("retry %d came after %v, not clearly later than the previous gap of %v", i, cur, prev)
		}
	}
}

func TestAJobThatFailsOnceThenSucceeds(t *testing.T) {
	pool := dbtest.Pool(t)
	var calls atomic.Int32
	run(t, pool, jobs.Options{}, func(w *jobs.Worker) {
		w.Register("flaky", func(_ context.Context, j jobs.Job) error {
			if calls.Add(1) == 1 {
				return errors.New("first time fails")
			}
			return nil
		})
	})
	id := enqueue(t, pool, "flaky", nil, jobs.EnqueueOptions{})
	waitFor(t, 5*time.Second, "the retry to succeed", func() bool { return jobRow(t, pool, id).State == "done" })
	if r := jobRow(t, pool, id); r.Attempts != 2 {
		t.Errorf("attempts %d, want 2", r.Attempts)
	}
}

func TestAPermanentErrorSkipsTheRetries(t *testing.T) {
	pool := dbtest.Pool(t)
	var calls atomic.Int32
	run(t, pool, jobs.Options{}, func(w *jobs.Worker) {
		w.Register("hopeless", func(_ context.Context, j jobs.Job) error {
			calls.Add(1)
			var v struct{ N int }
			return j.Decode(&v) // an unreadable payload can never succeed
		})
	})
	id := enqueue(t, pool, "hopeless", "not an object", jobs.EnqueueOptions{})
	waitFor(t, 5*time.Second, "the job to be dead-lettered", func() bool { return jobRow(t, pool, id).State == "failed" })
	if calls.Load() != 1 {
		t.Errorf("a permanent failure was tried %d times", calls.Load())
	}
	if r := jobRow(t, pool, id); r.Attempts != 1 || r.LastError == "" {
		t.Errorf("%+v", r)
	}
}

// One bad job must not take a worker down, or stop it serving the others.
func TestAPanicIsRecoveredAndTheWorkerCarriesOn(t *testing.T) {
	pool := dbtest.Pool(t)
	var panicked atomic.Bool
	var fine atomic.Int32
	run(t, pool, jobs.Options{Concurrency: 1}, func(w *jobs.Worker) {
		w.Register("bomb", func(context.Context, jobs.Job) error {
			if panicked.CompareAndSwap(false, true) {
				panic("kaboom")
			}
			return nil
		})
		w.Register("fine", func(context.Context, jobs.Job) error { fine.Add(1); return nil })
	})
	bomb := enqueue(t, pool, "bomb", nil, jobs.EnqueueOptions{})
	ok := enqueue(t, pool, "fine", nil, jobs.EnqueueOptions{})

	waitFor(t, 5*time.Second, "both jobs", func() bool {
		return jobRow(t, pool, bomb).State == "done" && jobRow(t, pool, ok).State == "done"
	})
	if r := jobRow(t, pool, bomb); r.Attempts != 2 || r.LastError != "" && !contains(r.LastError, "kaboom") {
		t.Errorf("the panicking job should have been retried once: %+v", r)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The outbox property: a job is enqueued in the transaction that causes it, and only exists if that commits.
func TestNothingRunsForARolledBackTransaction(t *testing.T) {
	pool := dbtest.Pool(t)
	var ran atomic.Int32
	run(t, pool, jobs.Options{}, func(w *jobs.Worker) {
		w.Register("outbox", func(context.Context, jobs.Job) error { ran.Add(1); return nil })
	})

	rollback := errors.New("the save failed")
	err := db.WithSystem(context.Background(), pool, func(tx pgx.Tx) error {
		if _, err := jobs.Enqueue(context.Background(), dbgen.New(tx), "outbox", nil, jobs.EnqueueOptions{}); err != nil {
			return err
		}
		return rollback // the surrounding save fails
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if ran.Load() != 0 {
		t.Error("a job ran for a transaction that rolled back")
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d jobs exist after a rollback (%v)", n, err)
	}

	enqueue(t, pool, "outbox", nil, jobs.EnqueueOptions{}) // committed this time
	waitFor(t, 5*time.Second, "the committed job", func() bool { return ran.Load() == 1 })
}

func TestTheSameDedupeKeyEnqueuesOnce(t *testing.T) {
	pool := dbtest.Pool(t)
	var ran atomic.Int32
	run(t, pool, jobs.Options{}, func(w *jobs.Worker) {
		w.Register("once", func(context.Context, jobs.Job) error { ran.Add(1); return nil })
	})
	opt := jobs.EnqueueOptions{DedupeKey: "invoice-42-reminder"}
	first := enqueue(t, pool, "once", nil, opt)
	second := enqueue(t, pool, "once", nil, opt)
	if first == 0 || second != 0 {
		t.Errorf("ids %d then %d: the second enqueue should be a no-op that returns 0", first, second)
	}
	waitFor(t, 5*time.Second, "the job", func() bool { return jobRow(t, pool, first).State == "done" })
	if third := enqueue(t, pool, "once", nil, opt); third != 0 {
		t.Error("a finished job's key must still deduplicate")
	}
	time.Sleep(200 * time.Millisecond)
	if ran.Load() != 1 {
		t.Errorf("ran %d times", ran.Load())
	}
}

func TestAFutureJobWaitsForItsTime(t *testing.T) {
	pool := dbtest.Pool(t)
	var ranAt atomic.Int64
	run(t, pool, jobs.Options{}, func(w *jobs.Worker) {
		w.Register("later", func(context.Context, jobs.Job) error { ranAt.Store(time.Now().UnixNano()); return nil })
	})
	due := time.Now().Add(600 * time.Millisecond)
	enqueue(t, pool, "later", nil, jobs.EnqueueOptions{RunAt: due})
	time.Sleep(300 * time.Millisecond)
	if ranAt.Load() != 0 {
		t.Fatal("the job ran before it was due")
	}
	waitFor(t, 5*time.Second, "the job to run", func() bool { return ranAt.Load() != 0 })
	if time.Unix(0, ranAt.Load()).Before(due) {
		t.Error("ran early")
	}
}

// Idle workers must not depend on polling: NOTIFY wakes them at once.
func TestNotifyWakesAnIdleWorkerImmediately(t *testing.T) {
	pool := dbtest.Pool(t)
	got := make(chan time.Time, 1)
	run(t, pool, jobs.Options{Concurrency: 1, PollInterval: time.Hour, DSN: dbtest.DSN(t)}, func(w *jobs.Worker) {
		w.Register("wake", func(context.Context, jobs.Job) error { got <- time.Now(); return nil })
	})
	time.Sleep(300 * time.Millisecond) // the worker is idle and listening
	start := time.Now()
	enqueue(t, pool, "wake", nil, jobs.EnqueueOptions{})
	select {
	case at := <-got:
		if d := at.Sub(start); d > time.Second {
			t.Errorf("took %v to start: the worker fell back to polling", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an hour-long poll interval and a missed notification: the job never ran")
	}
}

func TestAJobFromACrashedWorkerIsRecovered(t *testing.T) {
	pool := dbtest.Pool(t)
	var ran atomic.Int32
	// A job that a worker claimed an hour ago and never finished.
	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO jobs (kind, state, attempts, locked_by, locked_at)
		VALUES ('orphan', 'running', 1, 'dead-host/1/x#0', now() - interval '1 hour') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	run(t, pool, jobs.Options{StuckAfter: time.Second, ReapEvery: 50 * time.Millisecond}, func(w *jobs.Worker) {
		w.Register("orphan", func(context.Context, jobs.Job) error { ran.Add(1); return nil })
	})
	waitFor(t, 5*time.Second, "the orphan to be recovered and run", func() bool { return jobRow(t, pool, id).State == "done" })
	if r := jobRow(t, pool, id); r.Attempts != 2 || !contains(r.LastError, "worker lost") {
		t.Errorf("%+v", r)
	}
}

// The lease check: a worker presumed dead that comes back must not overwrite what the replacement recorded.
func TestAStaleWorkerCannotOverwriteANewerOutcome(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	id := enqueue(t, pool, "lease", nil, jobs.EnqueueOptions{})
	txQ := func(f func(q *dbgen.Queries)) {
		t.Helper()
		if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error { f(dbgen.New(tx)); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

	txQ(func(q *dbgen.Queries) {
		if j, err := q.ClaimJob(ctx, text("A")); err != nil || j.ID != id {
			t.Fatalf("A claim: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `UPDATE jobs SET locked_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	txQ(func(q *dbgen.Queries) {
		if n, err := q.ReapStuckJobs(ctx, 60); err != nil || n != 1 {
			t.Fatalf("reap: %d %v", n, err)
		}
		if _, err := q.ClaimJob(ctx, text("B")); err != nil {
			t.Fatalf("B claim: %v", err)
		}
	})
	txQ(func(q *dbgen.Queries) {
		if n, _ := q.CompleteJob(ctx, dbgen.CompleteJobParams{ID: id, LockedBy: text("B")}); n != 1 {
			t.Error("the current holder must be able to complete")
		}
		if n, _ := q.CompleteJob(ctx, dbgen.CompleteJobParams{ID: id, LockedBy: text("A")}); n != 0 {
			t.Error("the stale worker completed a job it no longer holds")
		}
		if n, _ := q.FailJob(ctx, dbgen.FailJobParams{ID: id, LockedBy: text("A"), Error: "late", BaseBackoffSecs: 1, MaxBackoffSecs: 1}); n != 0 {
			t.Error("the stale worker failed a job it no longer holds")
		}
	})
	if r := jobRow(t, pool, id); r.State != "done" {
		t.Errorf("state %q: the stale worker's write got through", r.State)
	}
}

func TestShutdownLetsARunningJobFinish(t *testing.T) {
	pool := dbtest.Pool(t)
	started, release := make(chan struct{}), make(chan struct{})
	stop := run(t, pool, jobs.Options{Concurrency: 1}, func(w *jobs.Worker) {
		w.Register("slow", func(ctx context.Context, _ jobs.Job) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	id := enqueue(t, pool, "slow", nil, jobs.EnqueueOptions{})
	<-started

	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("the worker stopped while a job was still running")
	case <-time.After(300 * time.Millisecond):
	}
	close(release) // the job finishes within the grace period
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not stop after its job finished")
	}
	if r := jobRow(t, pool, id); r.State != "done" {
		t.Errorf("the job that was running at shutdown is %q, want done", r.State)
	}
}

func TestShutdownCancelsAJobThatOverstaysTheGracePeriod(t *testing.T) {
	pool := dbtest.Pool(t)
	started := make(chan struct{})
	stop := run(t, pool, jobs.Options{Concurrency: 1, Grace: 150 * time.Millisecond}, func(w *jobs.Worker) {
		w.Register("stuck", func(ctx context.Context, _ jobs.Job) error {
			close(started)
			<-ctx.Done() // only cancellation ends it
			return ctx.Err()
		})
	})
	id := enqueue(t, pool, "stuck", nil, jobs.EnqueueOptions{})
	<-started
	begin := time.Now()
	stop()
	if d := time.Since(begin); d > 3*time.Second {
		t.Errorf("shutdown took %v with a 150ms grace period", d)
	}
	// It was cancelled, not lost: it goes back to be retried by whoever runs next.
	if r := jobRow(t, pool, id); r.State != "pending" {
		t.Errorf("a job cancelled at shutdown should be pending again, got %q", r.State)
	}
}

func TestAnUnknownKindIsRetriedThenDeadLettered(t *testing.T) {
	pool := dbtest.Pool(t)
	run(t, pool, jobs.Options{Concurrency: 1}, func(*jobs.Worker) {})
	id := enqueue(t, pool, "nobody.handles.this", nil, jobs.EnqueueOptions{})
	waitFor(t, 10*time.Second, "dead letter", func() bool { return jobRow(t, pool, id).State == "failed" })
	if r := jobRow(t, pool, id); r.Attempts != 5 || !contains(r.LastError, "no handler") {
		t.Errorf("%+v", r)
	}
}

func TestADeadLetteredJobCanBeRetriedAndOldJobsArePurged(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	var healthy atomic.Bool
	run(t, pool, jobs.Options{ReapEvery: 50 * time.Millisecond, KeepDone: time.Hour, KeepFailed: time.Hour}, func(w *jobs.Worker) {
		w.Register("repaired", func(context.Context, jobs.Job) error {
			if !healthy.Load() {
				return errors.New("still broken")
			}
			return nil
		})
	})
	id := enqueue(t, pool, "repaired", nil, jobs.EnqueueOptions{})
	waitFor(t, 10*time.Second, "dead letter", func() bool { return jobRow(t, pool, id).State == "failed" })

	healthy.Store(true) // the cause is fixed; an operator retries the job
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		n, err := dbgen.New(tx).RetryJob(ctx, id)
		if err == nil && n != 1 {
			err = fmt.Errorf("retried %d jobs", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the retried job", func() bool { return jobRow(t, pool, id).State == "done" })

	// Finished work older than the retention period is deleted; recent work stays.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET finished_at = now() - interval '2 hours' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	recent := enqueue(t, pool, "repaired", nil, jobs.EnqueueOptions{})
	waitFor(t, 5*time.Second, "the old job to be purged", func() bool {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, id).Scan(&n)
		return n == 0
	})
	waitFor(t, 5*time.Second, "the recent job", func() bool { return jobRow(t, pool, recent).State == "done" })
}

// Anyone signed in can cause work, but only through the door that validates it, and nobody can read or tamper.
func TestUsersCanEnqueueButCannotReadOrTamperWithJobs(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	var uid uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, name, role) VALUES ('a@x.test', 'A', 'end_user') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	asUser := func(f func(tx pgx.Tx) error) error { return db.WithUser(ctx, pool, uid, f) }

	if err := asUser(func(tx pgx.Tx) error {
		_, err := jobs.Enqueue(ctx, dbgen.New(tx), "user.caused", map[string]string{"why": "a save"}, jobs.EnqueueOptions{})
		return err
	}); err != nil {
		t.Fatalf("a user could not enqueue: %v", err)
	}

	denied := func(what, query string) {
		t.Helper()
		err := asUser(func(tx pgx.Tx) error { _, err := tx.Exec(ctx, query); return err })
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "42501" {
			t.Errorf("%s: want permission denied, got %v", what, err)
		}
	}
	denied("reading the queue", `SELECT * FROM jobs`)
	denied("cancelling a job", `DELETE FROM jobs`)
	denied("rewriting a job", `UPDATE jobs SET payload = '{}'`)
	denied("inserting a job directly", `INSERT INTO jobs (kind) VALUES ('sneaky')`)

	invalid := func(what string, kind string, payload any) {
		t.Helper()
		err := asUser(func(tx pgx.Tx) error {
			_, err := jobs.Enqueue(ctx, dbgen.New(tx), kind, payload, jobs.EnqueueOptions{})
			return err
		})
		if err == nil {
			t.Errorf("%s was accepted", what)
		}
	}
	invalid("a malformed kind", "Bad Kind!", nil)
	invalid("an oversized payload", "big", map[string]string{"x": string(make([]byte, 70000))})
}
