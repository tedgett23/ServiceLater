package workflowengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
)

// run performs one step against the execution as it is now. Anything it changes, it changes through q, in the step's
// transaction. A returned *stepFailure ends the execution; any other error is retried.
func (e *Engine) run(ctx context.Context, q *dbgen.Queries, ex dbgen.WorkflowExecution, st *apps.Step, vars map[string]any) (outcome, error) {
	if err := st.Err(); err != nil {
		return outcome{}, failf("this step cannot run: %v", err)
	}
	user, err := q.GetUser(ctx, ex.StartedBy)
	if err != nil {
		return outcome{}, err
	}
	in := expr.Input{User: &expr.User{ID: user.ID.String(), Name: user.Name, Email: user.Email, Role: user.Role}, Vars: vars, Now: time.Now(), Record: &expr.Record{}}
	var ticket *records.Ticket
	if ex.RecordID.Valid {
		t, err := records.Load(ctx, q, ex.RecordID.UUID)
		if errors.Is(err, pgx.ErrNoRows) {
			return outcome{}, failf("the ticket no longer exists, or %s can no longer see it", user.Name)
		}
		if err != nil {
			return outcome{}, err
		}
		ticket = &t
		in.Record = t.ExprRecord()
	}
	out := outcome{kind: "ok", vars: vars, detail: map[string]any{}, next: st.Next}
	switch st.Type {
	case apps.StepSet:
		return e.doSet(ctx, st, in, out)
	case apps.StepCondition:
		prog, err := st.Condition()
		if err != nil {
			return out, failf("the condition cannot run: %v", err)
		}
		ok, err := prog.Eval(ctx, in)
		if err != nil {
			return out, failf("the condition could not be evaluated: %v", err)
		}
		out.next = st.Else
		if ok {
			out.next = st.Then
		}
		out.detail["result"], out.detail["went_to"] = ok, out.next
		return out, nil
	case apps.StepWait:
		return e.doWait(ctx, ex, st, in, out)
	case apps.StepRecordUpdate:
		if ticket == nil {
			return out, failf("record.update needs a ticket, and this execution has none")
		}
		return e.doUpdate(ctx, q, ex, st, in, *ticket, out)
	case apps.StepCommentAdd:
		if ticket == nil {
			return out, failf("comment.add needs a ticket, and this execution has none")
		}
		return e.doComment(ctx, q, ex, st, in, out)
	case apps.StepEnd:
		out.next = ""
		return out, nil
	}
	return out, failf("unknown step type %q", st.Type)
}

// value evaluates a literal or an expression.
func value(ctx context.Context, v *apps.Value, in expr.Input) (any, error) {
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
	return p.EvalValue(ctx, in)
}

// storable turns what an expression produced into something a variable can hold and JSON can carry.
func storable(x any) (any, error) {
	switch v := x.(type) {
	case string, bool, int64, float64:
		return v, nil
	case uint64:
		return int64(v), nil //nolint:gosec // an expression result that fits a whole number
	case time.Time:
		return v.UTC().Format(time.RFC3339), nil
	}
	return nil, fmt.Errorf("a variable holds text, a number or true/false, not %T", x)
}

func (e *Engine) doSet(ctx context.Context, st *apps.Step, in expr.Input, out outcome) (outcome, error) {
	// A copy, so a variable set here is visible to the ones after it and the stored vars change only if the step succeeds.
	next := make(map[string]any, len(in.Vars)+len(st.Vars))
	for k, v := range in.Vars {
		next[k] = v
	}
	in.Vars = next
	set := map[string]any{}
	for _, nv := range st.Vars {
		x, err := value(ctx, nv.Value, in)
		if err != nil {
			return out, failf("vars.%s could not be evaluated: %v", nv.Name, err)
		}
		if x, err = storable(x); err != nil {
			return out, failf("vars.%s: %v", nv.Name, err)
		}
		next[nv.Name], set[nv.Name] = x, x
	}
	out.vars, out.detail["set"] = next, set
	return out, nil
}

func (e *Engine) pollEvery() time.Duration {
	if e.PollEvery > 0 {
		return e.PollEvery
	}
	return DefaultPollEvery
}

// doWait sleeps, or finishes sleeping. The first visit starts the wait (or skips it: a condition that is already true
// needs no waiting); a later visit is the timer, or a change to the record, waking it.
func (e *Engine) doWait(ctx context.Context, ex dbgen.WorkflowExecution, st *apps.Step, in expr.Input, out outcome) (outcome, error) {
	now := time.Now()
	started := ex.WaitStartedAt.Valid
	if st.Until == "" { // a fixed time
		if started {
			out.detail["waited"] = now.Sub(ex.WaitStartedAt.Time).Round(time.Second).String()
			return out, nil
		}
		d := st.ForDuration()
		out.kind, out.detail["for"] = "waiting", st.For
		out.wait = &waiting{started: now, deadline: now.Add(d), wake: now.Add(d)}
		return out, nil
	}
	prog, err := st.UntilCondition()
	if err != nil {
		return out, failf("the wait's condition cannot run: %v", err)
	}
	ok, err := prog.Eval(ctx, in)
	if err != nil {
		return out, failf("the wait's condition could not be evaluated: %v", err)
	}
	if ok {
		out.detail["until"] = "true"
		if started {
			out.detail["waited"] = now.Sub(ex.WaitStartedAt.Time).Round(time.Second).String()
		}
		return out, nil
	}
	deadline := now.Add(st.TimeoutDuration())
	startedAt := now
	if started {
		deadline, startedAt = ex.WaitDeadline.Time, ex.WaitStartedAt.Time
	}
	if !now.Before(deadline) {
		out.kind, out.detail["timed_out_after"] = "timeout", st.Timeout
		if st.OnTimeout == "" {
			return out, failf("timed out after %s waiting until %s", st.Timeout, st.Until)
		}
		out.next = st.OnTimeout
		return out, nil
	}
	// Look again when the record changes (the trigger brings the wake-up forward), at the latest after the poll
	// interval, and never later than the deadline.
	wake := now.Add(e.pollEvery())
	if wake.After(deadline) {
		wake = deadline
	}
	out.kind, out.detail["until"] = "waiting", st.Until
	out.wait = &waiting{started: startedAt, deadline: deadline, wake: wake, until: true}
	return out, nil
}

func (e *Engine) doUpdate(ctx context.Context, q *dbgen.Queries, ex dbgen.WorkflowExecution, st *apps.Step, in expr.Input, t records.Ticket, out outcome) (outcome, error) {
	// Evaluated into a copy of the record, in order, so a later field can use an earlier one; then saved as one edit.
	rec := *in.Record
	in.Record = &rec
	for _, nv := range st.Set {
		x, err := value(ctx, nv.Value, in)
		if err != nil {
			return out, failf("set.%s could not be evaluated: %v", nv.Name, err)
		}
		if err := assign(&rec, nv.Name, x); err != nil {
			return out, failf("set.%s: %v", nv.Name, err)
		}
	}
	want, err := records.FromRecord(t, &rec)
	if err != nil {
		return out, failf("%v", err)
	}
	patch := diff(t, want)
	out.detail["fields"] = changed(patch)
	if patch.Empty() {
		return out, nil
	}
	var cause ruleruns.Cause
	_ = json.Unmarshal(ex.Cause, &cause)
	user, err := q.GetUser(ctx, ex.StartedBy)
	if err != nil {
		return out, err
	}
	if _, err := e.Svc.Update(ctx, q, records.Actor{ID: user.ID, Role: user.Role}, cause, t.ID, patch); err != nil {
		var re *records.Error
		if errors.As(err, &re) {
			return out, failf("the change was refused: %s", re.Msg)
		}
		if isForbidden(err) {
			return out, failf("%s is not allowed to make this change", user.Name)
		}
		return out, err
	}
	return out, nil
}

// assign sets one field of the working record, checking the kind of value it was given.
func assign(rec *expr.Record, field string, x any) error {
	if field == "priority" {
		n, ok := x.(int64)
		if !ok {
			return fmt.Errorf("priority takes a whole number, got %T", x)
		}
		rec.Priority = n
		return nil
	}
	s, ok := x.(string)
	if !ok {
		return fmt.Errorf("%s takes text, got %T", field, x)
	}
	switch field {
	case "title":
		rec.Title = s
	case "description":
		rec.Description = s
	case "resolution":
		rec.Resolution = s
	case "assignee_id":
		if s == "" {
			rec.AssigneeID, rec.AssigneeName = nil, nil
		} else {
			rec.AssigneeID, rec.AssigneeName = &s, nil
		}
	default:
		return fmt.Errorf("cannot set %q", field)
	}
	return nil
}

// diff is the patch that turns t into want.
func diff(t, want records.Ticket) records.Patch {
	var p records.Patch
	if want.Title != t.Title {
		p.Title = &want.Title
	}
	if want.Description != t.Description {
		p.Description = &want.Description
	}
	if want.Priority != t.Priority {
		p.Priority = &want.Priority
	}
	if (want.AssigneeID == nil) != (t.AssigneeID == nil) || (want.AssigneeID != nil && *want.AssigneeID != *t.AssigneeID) {
		p.SetAssignee, p.AssigneeID = true, want.AssigneeID
	}
	if want.Resolution != t.Resolution {
		p.Resolution = &want.Resolution
	}
	return p
}

func changed(p records.Patch) []string {
	out := []string{}
	add := func(name string, on bool) {
		if on {
			out = append(out, name)
		}
	}
	add("title", p.Title != nil)
	add("description", p.Description != nil)
	add("priority", p.Priority != nil)
	add("assignee_id", p.SetAssignee)
	add("resolution", p.Resolution != nil)
	return out
}

func (e *Engine) doComment(ctx context.Context, q *dbgen.Queries, ex dbgen.WorkflowExecution, st *apps.Step, in expr.Input, out outcome) (outcome, error) {
	x, err := value(ctx, st.Body, in)
	if err != nil {
		return out, failf("the comment body could not be evaluated: %v", err)
	}
	body, ok := x.(string)
	if !ok {
		return out, failf("the comment body must be text, got %T", x)
	}
	body = strings.TrimSpace(body)
	if body == "" || len(body) > records.MaxComment {
		return out, failf("the comment is empty or too long")
	}
	if _, err := q.CreateComment(ctx, dbgen.CreateCommentParams{TaskID: ex.RecordID.UUID, AuthorID: ex.StartedBy, Body: body, Internal: st.Internal}); err != nil {
		if isForbidden(err) {
			return out, failf("the person the workflow runs as may not write this comment")
		}
		return out, err
	}
	out.detail["internal"] = st.Internal
	return out, nil
}
