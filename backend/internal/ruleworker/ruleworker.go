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
		outcome, detail, err := Outcome(Run(ctx, pool, svc, rule, p.RecordID, p.UserID, p.Event, p.Cause))
		return done(outcome, detail, err)
	}
}

// Run carries out an async rule's actions on a record, in one transaction, as the rule's app's principal acting for
// a person: the one whose change set it off, or nobody (uuid.Nil) for an SLA's warning or breach, which no change
// did. `user` in the rule's expressions is that person, or the app when there is none.
func Run(ctx context.Context, pool *pgxpool.Pool, svc *records.Service, rule *apps.Rule, recordID, person uuid.UUID, event string, cause ruleruns.Cause) error {
	// What it may do is what the app's manifest declares (approved at install), not what that person may.
	var principal uuid.UUID
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		var err error
		principal, err = dbgen.New(tx).AppPrincipal(ctx, rule.App)
		return err
	}); err != nil {
		return err
	}
	return db.WithPrincipal(ctx, pool, principal, person, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		who := person
		if who == uuid.Nil {
			who = principal
		}
		u, err := q.GetUser(ctx, who)
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
		task, err := q.GetTask(ctx, recordID)
		if err != nil {
			return err
		}
		if actor.CanRecord(authz.Update, task.Type) {
			if _, err := q.GetTaskForUpdate(ctx, recordID); err != nil {
				return err
			}
		}
		t, err := records.Load(ctx, q, recordID)
		if err != nil {
			return err
		}
		rec := t.ExprRecord()
		// The old record is not kept: the rule's own filter was decided at the save, with both. Action filters
		// see the ticket as it is, twice.
		in := expr.Input{Record: rec, Old: rec, Event: event, Now: time.Now(), Count: records.Counter(q),
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
			if _, err := q.CreateComment(ctx, dbgen.CreateCommentParams{TaskID: recordID, AuthorID: principal, Body: body, Internal: c.Internal}); err != nil {
				return err
			}
		}
		// Started after the comments and before the update, so a workflow that begins on this record begins on it
		// as it is when the rule ran, and is not raced by the rule's own edit (its steps read the record afresh).
		for _, st := range fx.Starts {
			if err := svc.StartFromAsyncRule(ctx, q, actor, cause, t, st); err != nil {
				return err
			}
		}
		for _, n := range fx.Notifies {
			if err := svc.NotifyFromAsyncRule(ctx, q, t, n); err != nil {
				return err
			}
		}
		for _, l := range fx.Linked {
			if err := queueLinked(ctx, q, t, l, person, cause); err != nil {
				return err
			}
		}
		patch, err := patchFrom(ctx, q, t, res)
		if err != nil || patch.Empty() {
			return err
		}
		_, err = svc.Update(ctx, q, actor, cause, recordID, patch)
		return err
	})
}

// Outcome is how a run's error is audited and whether its job is tried again.
func Outcome(err error) (outcome, detail string, retry error) {
	var pe *pgconn.PgError
	var re *records.Error
	switch {
	case err == nil:
		return ruleruns.OK, "", nil
	case errors.Is(err, pgx.ErrNoRows):
		return ruleruns.Skipped, "the ticket no longer exists, or the app may not see it (its manifest lacks records:read for the type)", nil
	case errors.As(err, &pe) && pe.Code == "42501":
		return ruleruns.Failed, "the app is not allowed to do what the rule does: add the permission to its manifest", jobs.Permanent(err)
	case errors.As(err, &re):
		// The save refused the rule's change (a before rule said no, a value was not allowed). Trying again
		// would be refused the same way.
		return ruleruns.Failed, "the change was refused: " + re.Msg, jobs.Permanent(err)
	default:
		return ruleruns.Failed, err.Error(), err
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
