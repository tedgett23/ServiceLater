package records

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/rules"
	"github.com/servicelater/servicelater/backend/internal/workflow"
)

// StartWorkflow starts a workflow on a ticket as the actor, inside the transaction q is built on. It refuses a
// workflow for another kind of record, and a chain of runs that is already too deep.
func (s *Service) StartWorkflow(ctx context.Context, q *dbgen.Queries, a Actor, cause ruleruns.Cause, t Ticket, wf *apps.Workflow) (uuid.UUID, error) {
	if wf.RecordType != t.Type && wf.RecordType != "task" {
		return uuid.Nil, Invalid(fmt.Sprintf("the workflow %s is for %s records, and %s is a %s", wf.Name, wf.RecordType, t.Reference, t.Type))
	}
	if why := cause.BlocksWorkflow(); why != "" {
		return uuid.Nil, Invalid("the workflow was not started: " + why)
	}
	return workflow.Start(ctx, q, workflow.StartParams{Def: wf, RecordID: &t.ID, UserID: a.ID, Cause: cause})
}

// startFromRule starts what an after or async rule asked for. A start that cannot happen never blocks the save: the
// rule was valid when it was installed, so this is a workflow removed since, or a chain cut short, and the audit says so.
func (s *Service) startFromRule(ctx context.Context, q *dbgen.Queries, a Actor, cause ruleruns.Cause, timing string, t Ticket, st rules.Start) error {
	run := ruleruns.Run{App: st.App, Rule: st.Rule, Timing: timing, Event: "workflow.start", RecordID: &t.ID}
	wf := s.Apps.Workflow(st.App, st.Workflow)
	if wf == nil {
		run.Outcome, run.Detail = ruleruns.Failed, "the workflow "+st.Workflow+" is not installed"
		slog.Error("rule starts a workflow that is not installed", "app", st.App, "rule", st.Rule, "workflow", st.Workflow)
		ruleruns.Record(ctx, s.Pool, run)
		return nil
	}
	if _, err := s.StartWorkflow(ctx, q, a, cause, t, wf); err != nil {
		var e *Error
		if errors.As(err, &e) {
			run.Outcome, run.Detail = ruleruns.Skipped, "not started: "+e.Msg
			ruleruns.Record(ctx, s.Pool, run)
			return nil
		}
		return err
	}
	return nil
}

// StartFromAsyncRule is startFromRule for the worker, which runs an async rule outside a save.
func (s *Service) StartFromAsyncRule(ctx context.Context, q *dbgen.Queries, a Actor, cause ruleruns.Cause, t Ticket, st rules.Start) error {
	return s.startFromRule(ctx, q, a, cause, apps.TimingAsync, t, st)
}
