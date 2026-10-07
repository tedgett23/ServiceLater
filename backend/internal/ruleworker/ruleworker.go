// Package ruleworker runs queued async rules (docs/apps-platform.md §6). It sits above internal/records: a rule's
// record.update is saved through the same path as a person's edit, so before and after rules, validation, the audit
// and the queuing of further async rules all apply to it, judged as the person whose change set the rule off.
package ruleworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/rules"
)

// Handler runs a queued async rule. It locks and reads the ticket as it is now, as the person who made the change,
// runs the rule's actions, writes the comments they asked for and saves the record.update changes as one edit, all
// in one transaction, then records the run.
//
// What it will not do is retry what cannot succeed: a ticket that is gone or hidden, a rule that was uninstalled,
// an action the person is not allowed to take, or a change the rules of the save refuse are recorded and finished.
// Anything else (the database, a timeout, data that made an expression fail this time) is retried with backoff and
// dead-lettered at the end.
func Handler(pool *pgxpool.Pool, reg *apps.Registry) jobs.Handler {
	svc := &records.Service{Apps: reg, Pool: pool}
	return func(ctx context.Context, job jobs.Job) error {
		var p ruleruns.Payload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return jobs.Permanent(fmt.Errorf("unreadable payload: %w", err))
		}
		start := time.Now()
		run := ruleruns.Run{App: p.App, Rule: p.Rule, Timing: apps.TimingAsync, Event: p.Event, RecordID: &p.RecordID, JobID: job.ID, Attempt: job.Attempt}
		done := func(outcome, detail string, err error) error {
			run.Outcome, run.Detail, run.Duration = outcome, detail, time.Since(start)
			ruleruns.Record(ctx, pool, run)
			return err
		}

		rule := reg.Rule(p.App, p.Rule)
		if rule == nil || rule.Timing != apps.TimingAsync {
			return done(ruleruns.Skipped, "the rule is no longer installed", nil)
		}
		// The rule runs as its app's principal, on behalf of the person whose change set it off: what it may do is
		// what the app's manifest declares (approved at install), not what that person may.
		var principal uuid.UUID
		if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
			var err error
			principal, err = dbgen.New(tx).AppPrincipal(ctx, p.App)
			return err
		}); err != nil {
			return done(ruleruns.Failed, err.Error(), err)
		}
		err := db.WithPrincipal(ctx, pool, principal, p.UserID, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			// `user` in the rule's expressions is still the person.
			u, err := q.GetUser(ctx, p.UserID)
			if err != nil {
				return err
			}
			actor, err := records.ActorIn(ctx, q, principal)
			if err != nil {
				return err
			}
			// Locked before it is read, so what the rule sees is what its update is applied to. Only an app that may
			// edit tickets of this type can lock one under RLS; any other's run reads it plainly and any update it
			// asks for is refused by the save.
			task, err := q.GetTask(ctx, p.RecordID)
			if err != nil {
				return err
			}
			if actor.CanRecord(authz.Update, task.Type) {
				if _, err := q.GetTaskForUpdate(ctx, p.RecordID); err != nil {
					return err
				}
			}
			t, err := records.Load(ctx, q, p.RecordID)
			if err != nil {
				return err
			}
			rec := t.ExprRecord()
			// The old record is not kept: the rule's own filter was decided at the save, with both. Action filters
			// see the ticket as it is, twice.
			in := expr.Input{Record: rec, Old: rec, Event: p.Event, Now: time.Now(),
				User: &expr.User{ID: u.ID.String(), Name: u.Name, Email: u.Email, Role: u.Role}}
			res, fx, err := rules.RunAsync(ctx, rule, in)
			if err != nil {
				return err
			}
			for _, c := range fx.Comments {
				body := strings.TrimSpace(c.Body)
				if body == "" || len(body) > records.MaxComment {
					return jobs.Permanent(errors.New("the rule wrote a comment that is empty or too long"))
				}
				if _, err := q.CreateComment(ctx, dbgen.CreateCommentParams{TaskID: p.RecordID, AuthorID: principal, Body: body, Internal: c.Internal}); err != nil {
					return err
				}
			}
			// Started after the comments and before the update, so a workflow that begins on this record begins on it
			// as it is when the rule ran, and is not raced by the rule's own edit (its steps read the record afresh).
			for _, st := range fx.Starts {
				if err := svc.StartFromAsyncRule(ctx, q, actor, p.Cause, t, st); err != nil {
					return err
				}
			}
			for _, n := range fx.Notifies {
				if err := svc.NotifyFromAsyncRule(ctx, q, t, n); err != nil {
					return err
				}
			}
			patch, err := patchFrom(ctx, q, t, res)
			if err != nil || patch.Empty() {
				return err
			}
			_, err = svc.Update(ctx, q, actor, p.Cause, p.RecordID, patch)
			return err
		})

		var pe *pgconn.PgError
		var re *records.Error
		switch {
		case err == nil:
			return done(ruleruns.OK, "", nil)
		case errors.Is(err, pgx.ErrNoRows):
			return done(ruleruns.Skipped, "the ticket no longer exists, or the app may not see it (its manifest lacks records:read for the type)", nil)
		case errors.As(err, &pe) && pe.Code == "42501":
			return done(ruleruns.Failed, "the app is not allowed to do what the rule does: add the permission to its manifest", jobs.Permanent(err))
		case errors.As(err, &re):
			// The save refused the rule's change (a before rule said no, a value was not allowed). Trying again
			// would be refused the same way.
			return done(ruleruns.Failed, "the change was refused: "+re.Msg, jobs.Permanent(err))
		default:
			return done(ruleruns.Failed, err.Error(), err)
		}
	}
}

// patchFrom is what record.update changed: the fields of res that differ from the ticket the rule started with.
func patchFrom(ctx context.Context, q *dbgen.Queries, t records.Ticket, res *expr.Record) (records.Patch, error) {
	want, err := records.FromRecord(ctx, q, t, res)
	if err != nil {
		return records.Patch{}, err
	}
	return records.PatchBetween(t, want), nil
}
