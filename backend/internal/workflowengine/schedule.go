package workflowengine

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/settings"
	"github.com/servicelater/servicelater/backend/internal/workflow"
)

// RunSchedules starts scheduled workflows when they are due, checking every interval until ctx ends. Every worker
// may run it: each due time is claimed in the database, so only one starts it.
func (e *Engine) RunSchedules(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			if _, err := e.ScheduleTick(ctx, now); err != nil && ctx.Err() == nil {
				slog.Error("starting scheduled workflows", "err", err)
			}
		}
	}
}

// ScheduleTick starts every scheduled workflow whose latest due time (at or before now) has not been started yet,
// and returns how many runs it started. A schedule seen for the first time only notes the time: it starts from the
// next one, so installing or upgrading an app never fires a backlog.
func (e *Engine) ScheduleTick(ctx context.Context, now time.Time) (int, error) {
	started := 0
	var hold settings.Clone
	if err := db.WithSystem(ctx, e.Pool, func(tx pgx.Tx) error {
		var err error
		hold, err = settings.ReadClone(ctx, dbgen.New(tx))
		return err
	}); err != nil {
		return 0, err
	}
	if hold.HoldSchedules {
		// The safety switch after a clone. Schedules start counting again when it is lifted: the first look after
		// that only notes the time, like a schedule seen for the first time (a clone empties their marks).
		return 0, nil
	}
	for _, wf := range e.Svc.Apps.Workflows() {
		if wf.Schedule == nil {
			continue
		}
		due := wf.Schedule.Prev(now)
		if due.IsZero() {
			continue
		}
		var claimed bool
		var principal uuid.UUID
		err := db.WithSystem(ctx, e.Pool, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			at := pgtype.Timestamptz{Time: due, Valid: true}
			last, err := q.GetScheduleLastDue(ctx, dbgen.GetScheduleLastDueParams{App: wf.App, Workflow: wf.Name})
			first := errors.Is(err, pgx.ErrNoRows)
			if err != nil && !first {
				return err
			}
			if !first && !due.After(last.Time) {
				return nil
			}
			_, err = q.ClaimSchedule(ctx, dbgen.ClaimScheduleParams{App: wf.App, Workflow: wf.Name, LastDue: at})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // another worker took it
			}
			if err != nil || first {
				return err
			}
			claimed = true
			principal, err = q.AppPrincipal(ctx, wf.App)
			return err
		})
		if err != nil {
			return started, err
		}
		if !claimed {
			continue
		}
		n, err := e.startScheduled(ctx, wf, principal, now)
		started += n
		if err != nil {
			// The time is taken: a run that could not start is logged, not retried into a pile-up.
			slog.Error("a scheduled workflow could not start", "app", wf.App, "workflow", wf.Name, "due", due, "err", err)
		}
	}
	return started, nil
}

// startScheduled starts a scheduled workflow as its app, for nobody: once, or once per record its for_each finds (as
// the app sees them).
func (e *Engine) startScheduled(ctx context.Context, wf *apps.Workflow, principal uuid.UUID, now time.Time) (int, error) {
	n := 0
	err := db.WithPrincipal(ctx, e.Pool, principal, uuid.Nil, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if wf.Schedule.ForEach == nil {
			_, err := workflow.Start(ctx, q, workflow.StartParams{Def: wf, UserID: principal})
			n = 1
			return err
		}
		ids, err := Find(ctx, q, wf.Schedule.ForEach, expr.Input{Now: now, Record: &expr.Record{}, Vars: map[string]any{}})
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := workflow.Start(ctx, q, workflow.StartParams{Def: wf, RecordID: &id, UserID: principal}); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
