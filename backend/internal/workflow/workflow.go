// Package workflow is the storage half of the workflow engine (docs/apps-platform.md §7): what an execution is, and how
// one is started. Running them is internal/workflowengine, which sits above internal/records because a step's
// record.update goes through the same save path as a person's edit; this package sits below records, because a rule
// starts a workflow in the save that sets it off.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
)

// RunKind is the job kind that moves an execution forward.
const RunKind = "workflow.run"

// RunPayload is what a run job carries: only the execution. Everything else is in its row.
type RunPayload struct {
	ExecutionID uuid.UUID `json:"execution_id"`
}

// Statuses of an execution.
const (
	Queued    = "queued"
	Running   = "running"
	Waiting   = "waiting"
	Done      = "done"
	Failed    = "failed"
	Cancelled = "cancelled"
)

// Finished reports whether a status is one an execution never leaves.
func Finished(status string) bool { return status == Done || status == Failed || status == Cancelled }

// StartParams says what to start and for whom.
type StartParams struct {
	Def      *apps.Workflow
	RecordID *uuid.UUID
	UserID   uuid.UUID // the person it runs as: their permissions, their RLS
	Cause    ruleruns.Cause
}

// Start creates an execution and queues its first run, inside the caller's transaction (q is built on it): if the
// save that started it rolls back, the execution and its job vanish with it. The workflow is copied into the row, so
// the run keeps the version it began on.
func Start(ctx context.Context, q *dbgen.Queries, p StartParams) (uuid.UUID, error) {
	snapshot, err := json.Marshal(p.Def)
	if err != nil {
		return uuid.Nil, err
	}
	cause, _ := json.Marshal(p.Cause.ThroughKey(p.Def.App + "/" + p.Def.Name))
	arg := dbgen.InsertExecutionParams{
		App: p.Def.App, Workflow: p.Def.Name, AppVersion: p.Def.Version, Definition: snapshot,
		StartedBy: p.UserID, Cause: cause, CurrentStep: p.Def.Start,
	}
	if p.RecordID != nil {
		arg.RecordID = uuid.NullUUID{UUID: *p.RecordID, Valid: true}
	}
	id, err := q.InsertExecution(ctx, arg)
	if err != nil {
		return uuid.Nil, fmt.Errorf("starting workflow %s: %w", p.Def.Name, err)
	}
	if _, err := jobs.Enqueue(ctx, q, RunKind, RunPayload{ExecutionID: id}, jobs.EnqueueOptions{}); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}
