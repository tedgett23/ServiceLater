package ruleworker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/rules"
)

// LinkedKind is the job kind of one linked.update: one linked record changed by an async rule of the record it is
// linked to (an incident resolved with its problem). Each record is its own job, so one that refuses the change (a
// guard, a form) is recorded on its own and does not hold back the others.
const LinkedKind = "rule.linked"

// LinkedPayload is one linked record to change and what to do to it, worked out when the rule ran.
type LinkedPayload struct {
	App  string `json:"app"`
	Rule string `json:"rule"`
	// SourceID is the record the rule ran on (the problem) and RecordID the linked one to change (an incident).
	SourceID uuid.UUID      `json:"source_id"`
	RecordID uuid.UUID      `json:"record_id"`
	Linked   string         `json:"linked"`
	UserID   uuid.UUID      `json:"user_id"`
	Cause    ruleruns.Cause `json:"cause"`
	Status   string         `json:"status,omitempty"`
	Set      map[string]any `json:"set,omitempty"`
	Comment  string         `json:"comment,omitempty"`
	Internal bool           `json:"internal,omitempty"`
}

// queueLinked queues a job for each open or waiting record linked to t, as the app sees them, in the rule run's
// transaction.
func queueLinked(ctx context.Context, q *dbgen.Queries, t records.Ticket, l rules.Linked, person uuid.UUID, cause ruleruns.Cause) error {
	link, ok := apps.LinkedRecords[l.Linked]
	if !ok || t.Type != link.From {
		return jobs.Permanent(fmt.Errorf("%s are not linked to %s records", l.Linked, t.Type))
	}
	if len(l.Comment) > records.MaxComment {
		return jobs.Permanent(fmt.Errorf("the rule wrote a comment that is too long"))
	}
	ids, err := q.ListOpenProblemIncidentIDs(ctx, uuid.NullUUID{UUID: t.ID, Valid: true})
	if err != nil {
		return err
	}
	for _, id := range ids {
		p := LinkedPayload{App: l.App, Rule: l.Rule, SourceID: t.ID, RecordID: id, Linked: l.Linked, UserID: person, Cause: cause,
			Status: l.Status, Set: l.Set, Comment: strings.TrimSpace(l.Comment), Internal: l.Internal}
		if _, err := jobs.Enqueue(ctx, q, LinkedKind, p, jobs.EnqueueOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// LinkedHandler changes one linked record, as the rule's app's principal acting for the person whose change set the
// rule off: it writes the comment, saves the fields and makes the move as one save, through records.Service, so the
// record's own rules, guards and form apply. A record that has since settled or been unlinked is left alone.
func LinkedHandler(pool *pgxpool.Pool, reg *apps.Registry) jobs.Handler {
	svc := &records.Service{Apps: reg, Pool: pool}
	return func(ctx context.Context, job jobs.Job) error {
		var p LinkedPayload
		dec := json.NewDecoder(bytes.NewReader(job.Payload))
		dec.UseNumber()
		if err := dec.Decode(&p); err != nil {
			return jobs.Permanent(fmt.Errorf("unreadable payload: %w", err))
		}
		start := time.Now()
		run := ruleruns.Run{App: p.App, Rule: p.Rule, Timing: apps.TimingAsync, Event: apps.EventUpdate, RecordID: &p.RecordID, JobID: job.ID, Attempt: job.Attempt}
		done := func(outcome, detail string, err error) error {
			run.Outcome, run.Detail, run.Duration = outcome, detail, time.Since(start)
			ruleruns.Record(ctx, pool, run)
			return err
		}
		skipped, err := changeLinked(ctx, pool, svc, p)
		if skipped != "" {
			return done(ruleruns.Skipped, skipped, nil)
		}
		outcome, detail, err := Outcome(err)
		return done(outcome, detail, err)
	}
}

// changeLinked carries out one linked record's change. It returns why it did nothing, when it did nothing on purpose.
func changeLinked(ctx context.Context, pool *pgxpool.Pool, svc *records.Service, p LinkedPayload) (string, error) {
	var principal uuid.UUID
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		var err error
		principal, err = dbgen.New(tx).AppPrincipal(ctx, p.App)
		return err
	}); err != nil {
		return "", err
	}
	var skipped string
	err := db.WithPrincipal(ctx, pool, principal, p.UserID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		actor, err := records.ActorIn(ctx, q, principal)
		if err != nil {
			return err
		}
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
		switch {
		case t.ProblemID == nil || *t.ProblemID != p.SourceID:
			skipped = "the record is no longer linked to " + p.SourceID.String()
			return nil
		case t.StateCategory != "open" && t.StateCategory != "waiting":
			skipped = "the record was already " + svc.Apps.Lifecycle(t.Type).Label(t.Status)
			return nil
		}
		if p.Comment != "" {
			if _, err := q.CreateComment(ctx, dbgen.CreateCommentParams{TaskID: t.ID, AuthorID: principal, Body: p.Comment, Internal: p.Internal}); err != nil {
				return err
			}
		}
		if len(p.Set) > 0 {
			rec := t.ExprRecord()
			for field, v := range p.Set {
				if err := rules.Assign(rec, field, fromJSON(v)); err != nil {
					return jobs.Permanent(err)
				}
			}
			patch, err := patchFrom(ctx, q, t, rec)
			if err != nil {
				return err
			}
			if !patch.Empty() {
				if _, err := svc.Update(ctx, q, actor, p.Cause, t.ID, patch); err != nil {
					return err
				}
			}
		}
		if p.Status != "" && p.Status != t.Status {
			if _, err := svc.Transition(ctx, q, actor, p.Cause, t.ID, p.Status); err != nil {
				return err
			}
		}
		return nil
	})
	return skipped, err
}

// fromJSON turns a value read back from a payload into what a rule gave: whole numbers as int64.
func fromJSON(v any) any {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		f, _ := n.Float64()
		return f
	}
	return v
}
