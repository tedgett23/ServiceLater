// Package slatimer is the worker's half of SLAs (docs/apps-platform.md §6.6). A timer finds running clocks whose next
// warning or breach is due, records it and queues the alert; the alert's handler carries out the SLA's actions as its
// app. A save moves clocks itself (internal/records), so what is left here is time passing with nobody saving.
//
// It sits above internal/records and internal/ruleworker, whose runner it uses: an SLA's actions are an async rule's.
package slatimer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/ruleworker"
	"github.com/servicelater/servicelater/backend/internal/slaclock"
)

// batch is how many clocks one pass claims at a time.
const batch = 100

// Run checks for due clocks every interval until ctx ends. Every worker may run it: each clock is claimed with
// SKIP LOCKED, and each alert is queued once (slaclock.Queue).
func Run(ctx context.Context, pool *pgxpool.Pool, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := Tick(ctx, pool, time.Now()); err != nil && ctx.Err() == nil {
			slog.Error("sla timer", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick handles every clock that is due at now, and returns how many it looked at.
func Tick(ctx context.Context, pool *pgxpool.Pool, now time.Time) (int, error) {
	total := 0
	for {
		n, err := tickBatch(ctx, pool, now)
		total += n
		if err != nil || n < batch {
			return total, err
		}
	}
}

// tickBatch claims up to a batch of due clocks and, for each, queues what it owes and schedules the next look, in one
// transaction. It runs as the system: no person is acting, and the clocks of every record are its business.
func tickBatch(ctx context.Context, pool *pgxpool.Pool, now time.Time) (int, error) {
	n := 0
	err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		rows, err := q.ClaimDueSLAs(ctx, batch)
		if err != nil {
			return err
		}
		n = len(rows)
		cals := slaclock.NewCalendars(q)
		for _, row := range rows {
			c := slaclock.FromRow(row)
			if err := advance(ctx, q, cals, &c, now); err != nil {
				return err
			}
			if err := slaclock.Save(ctx, q, c); err != nil {
				return err
			}
		}
		return nil
	})
	return n, err
}

// advance brings one running clock up to now: the alerts it owes are queued, and it is scheduled again with its
// calendar as it is now (a calendar edit asks for this by setting next_alert_at to now).
func advance(ctx context.Context, q *dbgen.Queries, cals *slaclock.Calendars, c *slaclock.Clock, now time.Time) error {
	def, err := apps.SLAFromSnapshot(c.Definition, c.Checksum, c.App)
	if err != nil {
		// Nothing can be worked out; stop looking until a save changes it.
		c.Problem, c.NextAlertAt = err.Error(), time.Time{}
		return nil
	}
	cal, problem, err := cals.Get(ctx, def.Calendar)
	if err != nil {
		return err
	}
	warnings := make([]int, len(def.Warnings))
	for i, w := range def.Warnings {
		warnings[i] = w.At
	}
	fire, breach := c.Owed(now, cal, warnings)
	if err := slaclock.Queue(ctx, q, c.ID, fire, breach); err != nil {
		return err
	}
	c.Problem = problem
	if err := c.Schedule(cal, warnings); err != nil {
		c.Problem, c.NextAlertAt = fmt.Sprintf("the due time cannot be worked out: %v", err), time.Time{}
	}
	return nil
}

// Handler carries out a queued alert: the SLA's warning or breach actions, as its app's principal acting for nobody,
// through the same runner as an async rule. The actions are those of the definition the clock started with.
func Handler(pool *pgxpool.Pool, reg *apps.Registry) jobs.Handler {
	svc := &records.Service{Apps: reg, Pool: pool}
	return func(ctx context.Context, job jobs.Job) error {
		var a slaclock.Alert
		if err := job.Decode(&a); err != nil {
			return err
		}
		var c slaclock.Clock
		err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
			row, err := dbgen.New(tx).GetTaskSLA(ctx, a.ClockID)
			c = slaclock.FromRow(row)
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // the record is gone, and its clock with it
		} else if err != nil {
			return err
		}
		event := "breach"
		if a.Warning != slaclock.Breach {
			event = fmt.Sprintf("warning %d", a.Warning+1)
		}
		start := time.Now()
		run := ruleruns.Run{App: c.App, Rule: c.SLA, Timing: ruleruns.TimingSLA, Event: event, RecordID: &c.TaskID, JobID: job.ID, Attempt: job.Attempt}
		done := func(outcome, detail string, err error) error {
			run.Outcome, run.Detail, run.Duration = outcome, detail, time.Since(start)
			ruleruns.Record(ctx, pool, run)
			return err
		}
		def, err := apps.SLAFromSnapshot(c.Definition, c.Checksum, c.App)
		if err != nil {
			return done(ruleruns.Failed, err.Error(), jobs.Permanent(err))
		}
		rule := def.BreachRule()
		if a.Warning != slaclock.Breach {
			rule = def.WarningRule(a.Warning)
		}
		if rule == nil {
			return nil // nothing to do but show it
		}
		outcome, detail, err := ruleworker.Outcome(ruleworker.Run(ctx, pool, svc, rule, c.TaskID, uuid.Nil, event, ruleruns.Cause{}))
		return done(outcome, detail, err)
	}
}
