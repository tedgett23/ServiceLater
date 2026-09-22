// Package ruleruns is the low-level half of async rules: the job they are queued as, the rules that stop a chain of
// them looping, and the audit of what rules did (docs/apps-platform.md §6). Running them is internal/ruleworker,
// which sits above internal/records; this package sits below it, because a save queues async rules.
package ruleruns

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/jobs"
)

// Kind is the job kind async rules are queued under.
const Kind = "rule.async"

// Outcomes recorded in action_runs.
const (
	OK      = "ok"
	Aborted = "aborted"
	Failed  = "failed"
	Skipped = "skipped"
)

// Payload is what a queued async run carries: which rule, for which record, and who made the change. It holds ids
// only. The worker reads the ticket itself, as that person, so what it sees is what they may see.
type Payload struct {
	App      string    `json:"app"`
	Rule     string    `json:"rule"`
	RecordID uuid.UUID `json:"record_id"`
	Event    string    `json:"event"`
	UserID   uuid.UUID `json:"user_id"`
	// Cause is the chain this run belongs to: what set it off, and what set that off. Anything the run saves carries it.
	Cause Cause `json:"cause"`
}

// MaxDepth is how many async runs may be chained, each caused by the save of the one before.
const MaxDepth = 5

// Cause is the chain of async rules behind a save. A save made by a person has none (the zero value). A save made by
// an async rule's update carries that rule and the ones before it, so that what the save triggers can be judged.
type Cause struct {
	Depth int      `json:"depth,omitempty"`
	Chain []string `json:"chain,omitempty"` // "app/rule", oldest first
}

func ruleKey(r *apps.Rule) string { return r.App + "/" + r.Name }

// Blocks says why a rule may not be set off by a save with this cause, or "" if it may. Two things stop a loop: a
// rule cannot fire on a change made by its own app (an app's rules do not trigger each other), and a chain is cut at
// MaxDepth however many apps it crosses.
func (c Cause) Blocks(r *apps.Rule) string {
	for _, k := range c.Chain {
		if app, _, _ := strings.Cut(k, "/"); app == r.App {
			return fmt.Sprintf("this change was made by %s, an app of the same rule", k)
		}
	}
	if c.Depth+1 > MaxDepth {
		return fmt.Sprintf("the chain of rules setting off rules is already %d deep", c.Depth)
	}
	return ""
}

// BlocksWorkflow says why a save with this cause may not start a workflow, or "" if it may. Starting a workflow is
// not a reaction to a change, so an app's own rule can start its own workflow; only the depth of the chain limits it.
func (c Cause) BlocksWorkflow() string {
	if c.Depth+1 > MaxDepth {
		return fmt.Sprintf("the chain of rules and workflows setting each other off is already %d deep", c.Depth)
	}
	return ""
}

// ThroughKey is the cause of what a run saves, for a run named by key ("app/workflow").
func (c Cause) ThroughKey(key string) Cause {
	return Cause{Depth: c.Depth + 1, Chain: append(append([]string(nil), c.Chain...), key)}
}

// through is the cause of what a run of this rule saves.
func (c Cause) through(r *apps.Rule) Cause {
	return Cause{Depth: c.Depth + 1, Chain: append(append([]string(nil), c.Chain...), ruleKey(r))}
}

// Enqueue queues the fired rules inside the save's transaction (q is built on it): if the save rolls back, the jobs
// go with it.
//
// Rules the cause blocks are not queued: each is recorded as skipped, with the reason, so a loop that is being cut
// leaves a trace instead of vanishing.
func Enqueue(ctx context.Context, q *dbgen.Queries, pool *pgxpool.Pool, fired []*apps.Rule, event string, recordID, userID uuid.UUID, cause Cause) error {
	for _, r := range fired {
		if why := cause.Blocks(r); why != "" {
			Record(ctx, pool, Run{App: r.App, Rule: r.Name, Timing: apps.TimingAsync, Event: event, RecordID: &recordID, Outcome: Skipped, Detail: "not run: " + why})
			continue
		}
		p := Payload{App: r.App, Rule: r.Name, RecordID: recordID, Event: event, UserID: userID, Cause: cause.through(r)}
		if _, err := jobs.Enqueue(ctx, q, Kind, p, jobs.EnqueueOptions{}); err != nil {
			return fmt.Errorf("queueing rule %s: %w", r.Name, err)
		}
	}
	return nil
}

// Run is one line of the audit.
type Run struct {
	App, Rule, Timing, Event string
	RecordID                 *uuid.UUID
	Outcome, Detail          string
	Duration                 time.Duration
	JobID                    int64
	Attempt                  int
}

// Record writes an audit line from a connection of its own, so it survives the rollback of the save it is about.
// A failure to record is logged and never changes what the caller is doing.
func Record(ctx context.Context, pool *pgxpool.Pool, r Run) {
	if r.Attempt == 0 {
		r.Attempt = 1
	}
	arg := dbgen.InsertActionRunParams{
		App: r.App, Rule: r.Rule, Timing: r.Timing, Event: r.Event, Outcome: r.Outcome, Detail: truncate(r.Detail),
		DurationMs: int32(min(r.Duration.Milliseconds(), 1<<30)), Attempt: int32(min(r.Attempt, 1<<30)), //nolint:gosec // clamped just above
	}
	if r.RecordID != nil {
		arg.RecordID = uuid.NullUUID{UUID: *r.RecordID, Valid: true}
	}
	if r.JobID != 0 {
		arg.JobID = pgtype.Int8{Int64: r.JobID, Valid: true}
	}
	// A canceled request must not lose the record of why it was refused.
	ctx = context.WithoutCancel(ctx)
	err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error { return dbgen.New(tx).InsertActionRun(ctx, arg) })
	if err != nil {
		slog.Error("recording a rule run", "rule", r.Rule, "err", err)
	}
}

func truncate(s string) string {
	if len(s) > 2000 {
		return s[:2000]
	}
	return s
}
