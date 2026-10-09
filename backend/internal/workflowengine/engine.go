// Package workflowengine runs workflow executions (docs/apps-platform.md §7). An execution is a row: where it is in its
// graph and its variables. The engine moves it one step at a time, each step in a transaction of its own together with
// what the step did, so a step's effect and the record that it happened commit or roll back as one and a retry never
// applies an effect twice. Waiting is the row's wake-up time (a timer queues it when due), never a goroutine.
package workflowengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/workflow"
)

const (
	// stepsPerRun is how many steps one job runs before handing the execution back to the queue, so a long workflow
	// cannot hold a worker while others wait.
	stepsPerRun = 25
	// stepTimeout bounds one step's work.
	stepTimeout = 30 * time.Second
	// maxSteps stops an execution that somehow keeps going (a workflow is a DAG, so this is a backstop).
	maxSteps = 1000
	// DefaultPollEvery is how often a wait on a condition looks again even if nothing touched the record, because a
	// condition may depend on the clock.
	DefaultPollEvery = 30 * time.Second
)

// Engine runs executions. It is safe for concurrent use: executions share nothing, and the queue hands each job to
// one worker.
type Engine struct {
	Pool *pgxpool.Pool
	Svc  *records.Service
	// PollEvery is how often a condition wait re-checks on its own (default 30s).
	PollEvery time.Duration

	mu    sync.Mutex
	cache map[string]*apps.Workflow
}

// New returns an engine that saves tickets through svc.
func New(pool *pgxpool.Pool, svc *records.Service) *Engine {
	return &Engine{Pool: pool, Svc: svc, PollEvery: DefaultPollEvery, cache: map[string]*apps.Workflow{}}
}

// stepFailure is a step that failed for a reason retrying will not change: a value that was not allowed, an expression
// that could not be evaluated, a record that is gone. It ends the execution and is recorded on the step.
type stepFailure struct{ msg string }

func (f *stepFailure) Error() string { return f.msg }

func failf(format string, args ...any) error { return &stepFailure{fmt.Sprintf(format, args...)} }

// Handler runs a queued execution forward until it finishes, waits, or has used its share of steps.
func (e *Engine) Handler() jobs.Handler {
	return func(ctx context.Context, job jobs.Job) error {
		var p workflow.RunPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return jobs.Permanent(fmt.Errorf("unreadable payload: %w", err))
		}
		// The steps run as the workflow's app, for the person who started it, so who those are must be known before a
		// transaction opens.
		var owner runAs
		var status string
		err := db.WithSystem(ctx, e.Pool, func(tx pgx.Tx) error {
			row, err := dbgen.New(tx).GetExecutionOwner(ctx, p.ExecutionID)
			owner, status = runAs{principal: row.RunAs, person: row.StartedBy}, row.Status
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && workflow.Finished(status) {
			return nil // purged, or cancelled while the job waited
		}
		if err != nil {
			return err
		}
		for i := 0; ; i++ {
			more, err := e.step(ctx, owner, p.ExecutionID, i == stepsPerRun-1)
			var f *stepFailure
			switch {
			case errors.As(err, &f):
				return e.fail(ctx, owner, p.ExecutionID, f.msg)
			case err != nil:
				// Out of attempts: the job goes to the dead letter, and the execution must not stay queued forever.
				if job.Attempt >= job.MaxAttempts {
					_ = e.fail(ctx, owner, p.ExecutionID, "gave up after "+fmt.Sprint(job.Attempt)+" attempts: "+err.Error())
				}
				return err
			case !more:
				return nil
			}
		}
	}
}

// outcome is what one step decided.
type outcome struct {
	kind   string         // ok, waiting or timeout
	detail map[string]any // what happened, for the history
	next   string         // the step to go to; empty when the workflow ends here
	vars   map[string]any
	wait   *waiting
}

type waiting struct {
	started, deadline, wake time.Time
	until                   bool
}

// runAs is who a workflow's steps run as: its app's principal, on behalf of the person who started it.
type runAs struct {
	principal uuid.NullUUID // NULL only for an execution older than app principals
	person    uuid.UUID
}

func (e *Engine) inTx(ctx context.Context, who runAs, fn func(pgx.Tx) error) error {
	// A step's notifications may reach the person it runs for: what it does is news to them (app_notify).
	marked := func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.workflow_step', 'on', true)"); err != nil {
			return err
		}
		return fn(tx)
	}
	if !who.principal.Valid {
		return db.WithUser(ctx, e.Pool, who.person, marked)
	}
	return db.WithPrincipal(ctx, e.Pool, who.principal.UUID, who.person, marked)
}

// step runs the execution's current step in one transaction. It reports whether the execution has more to do right now.
func (e *Engine) step(ctx context.Context, owner runAs, id uuid.UUID, last bool) (more bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	err = e.inTx(ctx, owner, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		ex, err := q.GetExecutionForUpdate(ctx, id)
		if err != nil {
			return err
		}
		// Only a queued or running execution moves: a waiting one is the timer's, and a finished one is over. A second
		// job for the same execution (a retry that raced) finds it in a state that says so and does nothing.
		if ex.Status != workflow.Queued && ex.Status != workflow.Running {
			more = false
			return nil
		}
		if ex.StepsRun >= maxSteps {
			return failf("the execution ran %d steps without finishing", maxSteps)
		}
		wf, err := e.workflowOf(ex)
		if err != nil {
			return failf("the workflow definition could not be read: %v", err)
		}
		st := wf.Step(ex.CurrentStep)
		if st == nil {
			return failf("the workflow has no step %q", ex.CurrentStep)
		}
		began := time.Now()
		var vars map[string]any
		if err := json.Unmarshal(ex.Vars, &vars); err != nil || vars == nil {
			vars = map[string]any{}
		}
		out, err := e.run(ctx, q, ex, st, vars)
		if err != nil {
			return err
		}
		detail, _ := json.Marshal(out.detail)
		seq := ex.StepsRun + 1
		if err := q.InsertStep(ctx, dbgen.InsertStepParams{ExecutionID: id, Seq: seq, StepID: st.ID, Type: st.Type, Outcome: out.kind,
			Detail: detail, DurationMs: int32(min(time.Since(began).Milliseconds(), 1<<30))}); err != nil { //nolint:gosec // clamped
			return err
		}
		save := dbgen.SaveExecutionParams{ID: id, Status: workflow.Running, CurrentStep: out.next, StepsRun: seq}
		save.Vars, _ = json.Marshal(out.vars)
		switch {
		case out.wait != nil:
			w := out.wait
			save.Status, save.CurrentStep, save.WaitUntil = workflow.Waiting, st.ID, w.until
			save.WaitStartedAt = pgtype.Timestamptz{Time: w.started, Valid: true}
			save.WaitDeadline = pgtype.Timestamptz{Time: w.deadline, Valid: true}
			save.WakeAt = pgtype.Timestamptz{Time: w.wake, Valid: true}
		case out.next == "":
			save.Status, save.CurrentStep = workflow.Done, st.ID
		case last:
			// This job has used its share: hand the execution back so other work gets its turn.
			save.Status = workflow.Queued
			if _, err := jobs.Enqueue(ctx, q, workflow.RunKind, workflow.RunPayload{ExecutionID: id}, jobs.EnqueueOptions{}); err != nil {
				return err
			}
		}
		more = save.Status == workflow.Running
		return q.SaveExecution(ctx, save)
	})
	return more, err
}

// fail ends an execution and records where. It runs in a transaction of its own, because the step's was rolled back.
func (e *Engine) fail(ctx context.Context, owner runAs, id uuid.UUID, msg string) error {
	return e.inTx(ctx, owner, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		ex, err := q.GetExecutionForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && workflow.Finished(ex.Status) {
			return nil
		}
		if err != nil {
			return err
		}
		slog.Warn("workflow failed", "app", ex.App, "workflow", ex.Workflow, "execution", id, "step", ex.CurrentStep, "err", msg)
		stepType := ""
		if wf, err := e.workflowOf(ex); err == nil {
			if st := wf.Step(ex.CurrentStep); st != nil {
				stepType = st.Type
			}
		}
		if err := q.InsertStep(ctx, dbgen.InsertStepParams{ExecutionID: id, Seq: ex.StepsRun + 1, StepID: ex.CurrentStep, Type: stepType,
			Outcome: "failed", Detail: []byte("{}"), Error: truncate(msg)}); err != nil {
			return err
		}
		return q.SaveExecution(ctx, dbgen.SaveExecutionParams{ID: id, Status: workflow.Failed, CurrentStep: ex.CurrentStep, Vars: ex.Vars,
			StepsRun: ex.StepsRun + 1, Error: truncate(msg)})
	})
}

func truncate(s string) string {
	if len(s) > 2000 {
		return s[:2000]
	}
	return s
}

// workflowOf reads the definition pinned in the execution and compiles it, once per distinct definition.
func (e *Engine) workflowOf(ex dbgen.WorkflowExecution) (*apps.Workflow, error) {
	sum := sha256.Sum256(ex.Definition)
	key := hex.EncodeToString(sum[:])
	e.mu.Lock()
	wf := e.cache[key]
	e.mu.Unlock()
	if wf != nil {
		return wf, nil
	}
	wf = &apps.Workflow{}
	if err := json.Unmarshal(ex.Definition, wf); err != nil {
		return nil, err
	}
	wf.App, wf.Version = ex.App, ex.AppVersion
	wf.Compile()
	e.mu.Lock()
	if len(e.cache) > 500 {
		e.cache = map[string]*apps.Workflow{}
	}
	e.cache[key] = wf
	e.mu.Unlock()
	return wf, nil
}

// RunTimer wakes sleeping executions whose time has come, every interval, until ctx ends. Every worker may run it:
// the wake-up takes rows with SKIP LOCKED, so two timers never queue the same execution twice.
func (e *Engine) RunTimer(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := e.WakeDue(ctx); err != nil && ctx.Err() == nil {
				slog.Error("workflow timer", "err", err)
			} else if n > 0 {
				slog.Debug("woke workflows", "n", n)
			}
		}
	}
}

// WakeDue queues a run for every waiting execution that is due, and reports how many.
func (e *Engine) WakeDue(ctx context.Context) (int, error) {
	n := 0
	err := db.WithSystem(ctx, e.Pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		ids, err := q.WakeDueExecutions(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := jobs.Enqueue(ctx, q, workflow.RunKind, workflow.RunPayload{ExecutionID: id}, jobs.EnqueueOptions{}); err != nil {
				return err
			}
		}
		n = len(ids)
		return nil
	})
	return n, err
}

// isForbidden reports a database refusal, which is the person's rights and not something to retry.
func isForbidden(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42501"
}
