package workflowengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/workflow"
)

// doChildren runs a subflow (one child run) or a foreach (one per item, Parallel at a time), and waits until they
// are over. The child rows are the truth: every visit counts them, starts the next ones, and finishes when none is
// left, so a retry never starts a child twice (the children start in the step's transaction). A child that finishes
// wakes this run (a trigger); the time limit is the backstop.
//
// It leaves, in vars: for a subflow, <step>_vars (the child's variables when it ended) and <step>_outcome (done,
// failed or timeout); for a foreach, <step>_results (each child's variables, null for one that did not finish),
// <step>_failed (how many failed) and <step>_outcome.
func (e *Engine) doChildren(ctx context.Context, q *dbgen.Queries, ex dbgen.WorkflowExecution, st *apps.Step, in expr.Input, ticket *records.Ticket, out outcome) (outcome, error) {
	foreach := st.Type == apps.StepForeach
	itemsVar := st.ID + "_items"
	rows, err := q.ListChildExecutions(ctx, dbgen.ListChildExecutionsParams{ParentID: uuid.NullUUID{UUID: ex.ID, Valid: true}, ParentStep: pgText(st.ID)})
	if err != nil {
		return out, err
	}
	setVar := func(name string, v any) {
		next := make(map[string]any, len(out.vars)+1)
		for k, x := range out.vars {
			next[k] = x
		}
		next[name] = v
		out.vars = next
	}

	// The list is worked out once, on the first visit, and kept: later visits start the rest of the same list.
	var items []any
	if foreach {
		if kept, ok := in.Vars[itemsVar].([]any); ok && (len(rows) > 0 || ex.WaitStartedAt.Valid) {
			items = kept
		} else {
			x, err := jsonValue(ctx, st.ItemsValue(), in)
			if err != nil {
				return out, failf("items could not be evaluated: %v", err)
			}
			list, ok := x.([]any)
			if !ok {
				return out, failf("items must be a list, got %T", x)
			}
			if len(list) > apps.MaxForeachItems {
				return out, failf("items has %d entries; a foreach runs for at most %d", len(list), apps.MaxForeachItems)
			}
			items = list
			stored := make([]any, len(list))
			for i, it := range list {
				if stored[i], err = storableAny(it); err != nil {
					return out, failf("items[%d]: %v", i, err)
				}
			}
			items = stored
			setVar(itemsVar, stored)
			in.Vars = out.vars
		}
	} else {
		items = []any{nil}
	}

	// Count what is running, finished and failed.
	running, failed := 0, 0
	var firstError string
	for _, r := range rows {
		switch r.Status {
		case workflow.Done:
		case workflow.Failed, workflow.Cancelled:
			failed++
			if firstError == "" {
				firstError = r.Error
			}
		default:
			running++
		}
	}
	stopping := failed > 0 && !st.KeepGoing

	// Start what can start now.
	if !stopping && len(rows) < len(items) {
		if int(ex.Depth)+1 > apps.MaxSubflowDepth {
			return out, failf("subflows may nest %d deep; this would be deeper", apps.MaxSubflowDepth)
		}
		callee := e.Svc.Apps.Workflow(ex.App, st.Workflow)
		if callee == nil {
			return out, failf("the %s app has no workflow %q", ex.App, st.Workflow)
		}
		parallel := max(st.Parallel, 1)
		var cause ruleruns.Cause
		_ = json.Unmarshal(ex.Cause, &cause)
		for i := len(rows); i < len(items) && running < parallel; i++ {
			vars := map[string]any{}
			for _, nv := range st.Inputs {
				x, err := jsonValue(ctx, nv.Value, in)
				if err != nil {
					return out, failf("inputs.%s could not be evaluated: %v", nv.Name, err)
				}
				if vars[nv.Name], err = storableAny(x); err != nil {
					return out, failf("inputs.%s: %v", nv.Name, err)
				}
			}
			var recordID *uuid.UUID
			if ticket != nil {
				recordID = &ticket.ID
			}
			if foreach {
				vars["item"], vars["index"] = items[i], int64(i)
				if st.Each == "record" {
					id, err := e.recordOf(ctx, q, items[i])
					if err != nil {
						return out, failf("items[%d]: %v", i, err)
					}
					recordID = &id
				}
			} else if st.Record != nil {
				x, err := value(ctx, st.Record, in)
				if err != nil {
					return out, failf("record could not be evaluated: %v", err)
				}
				id, err := e.recordOf(ctx, q, x)
				if err != nil {
					return out, failf("record: %v", err)
				}
				recordID = &id
			}
			if recordID != nil && callee.RecordType != "task" {
				t, err := q.GetTask(ctx, *recordID)
				if err != nil {
					return out, failf("the record for %s cannot be found, or the %s app may not see it", st.Workflow, ex.App)
				}
				if t.Type != callee.RecordType {
					return out, failf("workflow %s is for %s records, not %s ones", st.Workflow, callee.RecordType, t.Type)
				}
			}
			if _, err := workflow.StartChild(ctx, q, workflow.ChildParams{Def: callee, RecordID: recordID, UserID: ex.StartedBy, Cause: cause,
				Vars: vars, Parent: ex.ID, Step: st.ID, Index: i, Depth: int(ex.Depth) + 1}); err != nil {
				return out, err
			}
			running++
		}
	}

	finish := func(result string) outcome {
		out.detail["outcome"] = result
		if foreach {
			results := make([]any, len(items))
			for _, r := range rows {
				if r.Status == workflow.Done && r.ParentIndex.Valid && int(r.ParentIndex.Int32) < len(results) {
					var v map[string]any
					_ = json.Unmarshal(r.Vars, &v)
					results[r.ParentIndex.Int32] = wholeJSON(v)
				}
			}
			setVar(st.ID+"_results", results)
			setVar(st.ID+"_failed", int64(failed))
			out.detail["runs"], out.detail["failed"] = len(rows), failed
		} else if len(rows) > 0 {
			var v map[string]any
			_ = json.Unmarshal(rows[0].Vars, &v)
			setVar(st.ID+"_vars", wholeJSON(v))
		}
		setVar(st.ID+"_outcome", result)
		return out
	}

	// Over when nothing is running and nothing more will start.
	if running == 0 && (stopping || len(rows) >= len(items)) {
		if stopping {
			out = finish("failed")
			out.kind = "failed"
			if st.OnError != "" {
				out.next = st.OnError
				return out, nil
			}
			return out, failf("%s %s failed: %s", st.Type, st.Workflow, firstError)
		}
		return finish("done"), nil
	}

	// Waiting: for the children (each wakes this run as it ends), or the time limit.
	now := time.Now()
	started := ex.WaitStartedAt.Valid && ex.CurrentStep == st.ID
	startedAt, deadline := now, now.Add(noTimeout)
	if st.Timeout != "" {
		deadline = now.Add(st.TimeoutDuration())
	}
	if started {
		startedAt, deadline = ex.WaitStartedAt.Time, ex.WaitDeadline.Time
	}
	if st.Timeout != "" && !now.Before(deadline) {
		if err := q.CancelChildExecutions(ctx, dbgen.CancelChildExecutionsParams{ParentID: uuid.NullUUID{UUID: ex.ID, Valid: true}, ParentStep: pgText(st.ID)}); err != nil {
			return out, err
		}
		out = finish("timeout")
		out.kind, out.detail["timed_out_after"] = "timeout", st.Timeout
		if st.OnTimeout == "" {
			return out, failf("timed out after %s waiting for %s", st.Timeout, st.Workflow)
		}
		out.next = st.OnTimeout
		return out, nil
	}
	out.kind = "waiting"
	out.detail["running"], out.detail["started"] = running, len(rows)
	out.wait = &waiting{started: startedAt, deadline: deadline, wake: deadline, until: true}
	return out, nil
}

// recordOf is the record an item or a value names: its id, or its reference (INC42).
func (e *Engine) recordOf(ctx context.Context, q *dbgen.Queries, x any) (uuid.UUID, error) {
	s, ok := x.(string)
	if !ok {
		return uuid.Nil, fmt.Errorf("a record is named by its id or reference, not %T", x)
	}
	r, err := records.ScriptReader(q).Get(ctx, s)
	switch {
	case err != nil:
		return uuid.Nil, err
	case r == nil:
		return uuid.Nil, errors.New("there is no record " + s + ", or it cannot be seen")
	}
	return uuid.Parse(r.ID)
}

// storableAny is a value a variable can hold: storable, or a list or map of them (as JSON keeps them).
func storableAny(x any) (any, error) {
	switch v := x.(type) {
	case nil:
		return nil, nil
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			s, err := storableAny(e)
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			s, err := storableAny(e)
			if err != nil {
				return nil, err
			}
			out[k] = s
		}
		return out, nil
	case float64, int:
		return v, nil
	}
	return storable(x)
}

func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// jsonValue evaluates a literal or an expression into a value JSON can hold (lists and maps included).
func jsonValue(ctx context.Context, v *apps.Value, in expr.Input) (any, error) {
	if v == nil {
		return nil, errors.New("no value")
	}
	p, err := v.Compiled()
	if err != nil {
		return nil, err
	}
	if p == nil {
		return v.Literal, nil
	}
	return p.EvalJSON(ctx, in)
}
