package workflowengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
)

// noTimeout is the deadline an approval with no timeout waits until: far enough that only a decision (through the
// wake trigger) or a cancel ever moves it, never the timer.
const noTimeout = 100 * 365 * 24 * time.Hour

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
	in := expr.Input{User: &expr.User{ID: user.ID.String(), Name: user.Name, Email: user.Email, Role: user.Role}, Vars: vars, Now: time.Now(),
		Record: &expr.Record{}, Groups: groupMembers(q), Roles: roleHolders(q)}
	var ticket *records.Ticket
	if ex.RecordID.Valid {
		t, err := records.Load(ctx, q, ex.RecordID.UUID)
		if errors.Is(err, pgx.ErrNoRows) {
			if ex.RunAs.Valid {
				return outcome{}, failf("the ticket no longer exists, or the %s app may not see it (its manifest needs records:read for the type)", ex.App)
			}
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
	case apps.StepRecordTransition:
		if ticket == nil {
			return out, failf("record.transition needs a ticket, and this execution has none")
		}
		return e.doTransition(ctx, q, ex, st, *ticket, out)
	case apps.StepCommentAdd:
		if ticket == nil {
			return out, failf("comment.add needs a ticket, and this execution has none")
		}
		return e.doComment(ctx, q, ex, st, in, out)
	case apps.StepApproval:
		return e.doApproval(ctx, q, ex, st, in, out)
	case apps.StepTaskCreate:
		if ticket == nil {
			return out, failf("task.create needs a record to put the task under, and this execution has none")
		}
		return e.doTaskCreate(ctx, q, ex, st, in, *ticket, out)
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

// doTransition moves the record through records.Service.Transition, as the workflow runs (the app, for the person),
// so the move must exist and its guard must agree, exactly as for a person; automatic moves are allowed here.
func (e *Engine) doTransition(ctx context.Context, q *dbgen.Queries, ex dbgen.WorkflowExecution, st *apps.Step, t records.Ticket, out outcome) (outcome, error) {
	out.detail["from"], out.detail["to"] = t.Status, st.To
	if t.Status == st.To {
		return out, nil // already there: a retry after the move committed, or someone moved it first
	}
	var cause ruleruns.Cause
	_ = json.Unmarshal(ex.Cause, &cause)
	user, err := q.GetUser(ctx, ex.StartedBy)
	if err != nil {
		return out, err
	}
	runner := ex.StartedBy
	if ex.RunAs.Valid {
		runner = ex.RunAs.UUID
	}
	actor, err := records.ActorIn(ctx, q, runner)
	if err != nil {
		return out, err
	}
	if _, err := e.Svc.Transition(ctx, q, actor, cause, t.ID, st.To); err != nil {
		var re *records.Error
		if errors.As(err, &re) {
			return out, failf("the move to %s was refused: %s", st.To, re.Msg)
		}
		if isForbidden(err) {
			return out, failf("the %s app is not allowed to move this ticket for %s: add the permission it needs to its manifest", ex.App, user.Name)
		}
		return out, err
	}
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
	want, err := records.FromRecord(ctx, q, t, &rec)
	if err != nil {
		return out, failf("%v", err)
	}
	patch := records.PatchBetween(t, want)
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
	runner := ex.StartedBy
	if ex.RunAs.Valid {
		runner = ex.RunAs.UUID
	}
	actor, err := records.ActorIn(ctx, q, runner)
	if err != nil {
		return out, err
	}
	if _, err := e.Svc.Update(ctx, q, actor, cause, t.ID, patch); err != nil {
		var re *records.Error
		if errors.As(err, &re) {
			return out, failf("the change was refused: %s", re.Msg)
		}
		if isForbidden(err) {
			return out, failf("the %s app is not allowed to make this change for %s: add the permission it needs to its manifest", ex.App, user.Name)
		}
		return out, err
	}
	return out, nil
}

// assign sets one field of the working record, checking the kind of value it was given.
func assign(rec *expr.Record, field string, x any) error {
	if name, ok := strings.CutPrefix(field, "fields."); ok {
		rec.SetField(name, x)
		return nil
	}
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
	case "risk":
		rec.Risk = s
	case "impact":
		rec.Impact = s
	case "assignee_id":
		if s == "" {
			rec.AssigneeID, rec.AssigneeName = nil, nil
		} else {
			rec.AssigneeID, rec.AssigneeName = &s, nil
		}
	case "assignment_group":
		if s == "" {
			rec.AssignmentGroup, rec.AssignmentGroupID = nil, nil
		} else {
			rec.AssignmentGroup, rec.AssignmentGroupID = &s, nil
		}
	default:
		return fmt.Errorf("cannot set %q", field)
	}
	return nil
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
	add("assignment_group", p.SetAssignmentGroup)
	add("resolution", p.Resolution != nil)
	add("risk", p.Change != nil && p.Change.Risk != nil)
	add("impact", p.Change != nil && p.Change.Impact != nil)
	names := make([]string, 0, len(p.Fields))
	for name := range p.Fields {
		names = append(names, "fields."+name)
	}
	slices.Sort(names)
	return append(out, names...)
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
	// Written by the app, for the person who started the workflow (on_behalf_of is filled in by the database).
	author := ex.StartedBy
	if ex.RunAs.Valid {
		author = ex.RunAs.UUID
	}
	if _, err := q.CreateComment(ctx, dbgen.CreateCommentParams{TaskID: ex.RecordID.UUID, AuthorID: author, Body: body, Internal: st.Internal}); err != nil {
		if isForbidden(err) {
			return out, failf("the %s app may not write this comment: add the permission it needs to its manifest", ex.App)
		}
		return out, err
	}
	out.detail["internal"] = st.Internal
	return out, nil
}

// doTaskCreate files a task under the record for a team, then (unless the step says not to) waits until it is done
// or cancelled. The task's id is kept in vars as it is filed, in the same transaction, so a later visit (the task
// changed, or the time limit came) finds it and a retry never files a second one.
func (e *Engine) doTaskCreate(ctx context.Context, q *dbgen.Queries, ex dbgen.WorkflowExecution, st *apps.Step, in expr.Input, parent records.Ticket, out outcome) (outcome, error) {
	idVar, refVar, outcomeVar := st.ID+"_task_id", st.ID+"_task", st.ID+"_outcome"
	id, _ := in.Vars[idVar].(string)
	now := time.Now()
	if id == "" {
		text := func(name string, v *apps.Value) (string, error) {
			if v == nil {
				return "", nil
			}
			x, err := value(ctx, v, in)
			if err != nil {
				return "", failf("%s could not be evaluated: %v", name, err)
			}
			s, ok := x.(string)
			if !ok {
				return "", failf("%s must be text, got %T", name, x)
			}
			return strings.TrimSpace(s), nil
		}
		title, err := text("title", st.Title)
		if err != nil {
			return out, err
		}
		desc, err := text("description", st.Description)
		if err != nil {
			return out, err
		}
		groupName, err := text("group", st.Group)
		if err != nil {
			return out, err
		}
		input := records.CreateInput{Type: "work_task", Title: title, Description: desc, RequesterID: &parent.RequesterID, ParentID: &parent.ID}
		// A team that does not exist on this instance (a shipped workflow names one nobody has made yet) leaves the
		// task unrouted, where the Unassigned queue shows it, rather than stopping the request.
		if groupName != "" {
			g, err := q.GetGroupByName(ctx, groupName)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				out.detail["unrouted"] = fmt.Sprintf("there is no group named %q, so the task was filed without a team", groupName)
			case err != nil:
				return out, err
			default:
				input.AssignmentGroupID = &g.ID
			}
		}
		var cause ruleruns.Cause
		_ = json.Unmarshal(ex.Cause, &cause)
		runner := ex.StartedBy
		if ex.RunAs.Valid {
			runner = ex.RunAs.UUID
		}
		actor, err := records.ActorIn(ctx, q, runner)
		if err != nil {
			return out, err
		}
		task, err := e.Svc.Create(ctx, q, actor, cause, input)
		if err != nil {
			var re *records.Error
			if errors.As(err, &re) {
				return out, failf("the task could not be filed: %s", re.Msg)
			}
			if isForbidden(err) {
				return out, failf("the %s app is not allowed to file tasks: add records:create:work_task and records:update:work_task to its manifest", ex.App)
			}
			return out, err
		}
		next := make(map[string]any, len(in.Vars)+2)
		for k, v := range in.Vars {
			next[k] = v
		}
		next[idVar], next[refVar] = task.ID.String(), task.Reference
		out.vars, in.Vars = next, next
		out.detail["task"], out.detail["group"] = task.Reference, groupName
		if !st.Waits() {
			return out, nil
		}
		id = task.ID.String()
	} else {
		out.detail["task"] = in.Vars[refVar]
	}
	taskID, err := uuid.Parse(id)
	if err != nil {
		return out, failf("vars.%s is not a task id", idVar)
	}
	task, err := records.Load(ctx, q, taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, failf("the task %v no longer exists, or the %s app may not see it (its manifest needs records:read:work_task)", in.Vars[refVar], ex.App)
	}
	if err != nil {
		return out, err
	}
	finish := func(result string) outcome {
		next := make(map[string]any, len(out.vars)+1)
		for k, v := range out.vars {
			next[k] = v
		}
		next[outcomeVar] = result
		out.vars, out.detail["outcome"] = next, result
		return out
	}
	switch task.StateCategory {
	case "done":
		return finish("done"), nil
	case "closed":
		return finish("cancelled"), nil
	}
	started := ex.WaitStartedAt.Valid && ex.CurrentStep == st.ID
	startedAt, deadline := now, now.Add(noTimeout)
	if st.Timeout != "" {
		deadline = now.Add(st.TimeoutDuration())
	}
	if started {
		startedAt, deadline = ex.WaitStartedAt.Time, ex.WaitDeadline.Time
	}
	if st.Timeout != "" && !now.Before(deadline) {
		out = finish("timeout")
		out.kind, out.detail["timed_out_after"] = "timeout", st.Timeout
		if st.OnTimeout == "" {
			return out, failf("timed out after %s waiting for %v", st.Timeout, in.Vars[refVar])
		}
		out.next = st.OnTimeout
		return out, nil
	}
	// The task changing wakes this (the record-change trigger also wakes a parent's waits); the deadline is the backstop.
	out.kind = "waiting"
	out.wait = &waiting{started: startedAt, deadline: deadline, wake: deadline, until: true}
	return out, nil
}

// doApproval asks a person (or several, or groups), or finishes asking. The first visit resolves the approvers and
// files one pending request per approver, and one per member of each group; a later visit is the timer, or someone deciding, waking it. Whether the step is done,
// and which way, is computed fresh from the request rows every time, so it never disagrees with what is stored.
func (e *Engine) doApproval(ctx context.Context, q *dbgen.Queries, ex dbgen.WorkflowExecution, st *apps.Step, in expr.Input, out outcome) (outcome, error) {
	rows, err := q.ListApprovalsForStep(ctx, dbgen.ListApprovalsForStepParams{ExecutionID: ex.ID, StepID: st.ID})
	if err != nil {
		return out, err
	}
	if len(rows) == 0 {
		asked, err := resolveApprovers(ctx, q, st, in)
		if err != nil {
			return out, err
		}
		for _, a := range asked {
			if err := q.CreateApproval(ctx, dbgen.CreateApprovalParams{ExecutionID: ex.ID, StepID: st.ID, ApproverID: a.user,
				GroupName: pgtype.Text{String: a.group, Valid: a.group != ""}}); err != nil {
				return out, err
			}
		}
		if rows, err = q.ListApprovalsForStep(ctx, dbgen.ListApprovalsForStepParams{ExecutionID: ex.ID, StepID: st.ID}); err != nil {
			return out, err
		}
	}
	if decision, comment, decider, done := evaluateApprovals(st.ApprovalRule(), rows); done {
		decidedBy := ""
		if decider.ApproverID != uuid.Nil {
			if u, err := q.GetUser(ctx, decider.ApproverID); err == nil {
				decidedBy = u.Name
			}
		}
		return approvalDone(out, st, in.Vars, decision, comment, decidedBy), nil
	}
	now := time.Now()
	started := ex.WaitStartedAt.Valid
	startedAt, deadline := now, now.Add(noTimeout)
	if st.Timeout != "" {
		deadline = now.Add(st.TimeoutDuration())
	}
	if started {
		startedAt, deadline = ex.WaitStartedAt.Time, ex.WaitDeadline.Time
	}
	if st.Timeout != "" && !now.Before(deadline) {
		out.kind, out.detail["timed_out_after"] = "timeout", st.Timeout
		out = approvalDone(out, st, in.Vars, "timeout", "", "")
		if st.OnTimeout == "" {
			return out, failf("timed out after %s waiting for approval", st.Timeout)
		}
		out.next = st.OnTimeout
		return out, nil
	}
	wake := deadline
	if w := now.Add(e.pollEvery()); st.Timeout != "" && w.Before(deadline) {
		wake = w
	}
	out.kind, out.detail["pending"] = "waiting", pendingApprovers(rows)
	out.wait = &waiting{started: startedAt, deadline: deadline, wake: wake, until: true}
	return out, nil
}

// groupMembers resolves group('name') in a step's expressions, in the step's transaction and as the person the
// workflow runs as. A group that does not exist fails the step rather than silently asking nobody.
func groupMembers(q *dbgen.Queries) func(ctx context.Context, name string) ([]string, error) {
	return func(ctx context.Context, name string) ([]string, error) {
		if _, err := q.GetGroupByName(ctx, name); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("there is no group named %q", name)
			}
			return nil, err
		}
		ids, err := q.ListGroupMemberIDsByName(ctx, name)
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = id.String()
		}
		return out, err
	}
}

// roleHolders is what role('name') resolves to: everyone active who holds the role. A role that does not exist is an
// error, never an empty list, like a missing group.
func roleHolders(q *dbgen.Queries) func(ctx context.Context, name string) ([]string, error) {
	return func(ctx context.Context, name string) ([]string, error) {
		if _, err := q.GetRole(ctx, name); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("there is no role named %q", name)
			}
			return nil, err
		}
		ids, err := q.ListRoleHolderIDs(ctx, name)
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = id.String()
		}
		return out, err
	}
}

// request is one person asked to decide: for themselves, or for a group (group is its name).
type request struct {
	user  uuid.UUID
	group string
}

// resolveApprovers evaluates an approval step's `approvers` into the distinct users it names, checking each exists,
// and its `groups` into each group's active members. A group that does not exist, or has nobody to ask, fails the
// step: its vote could never be cast.
func resolveApprovers(ctx context.Context, q *dbgen.Queries, st *apps.Step, in expr.Input) ([]request, error) {
	var out []request
	if st.Approvers != "" {
		prog, err := st.ApproversProgram()
		if err != nil {
			return nil, failf("this step cannot run: %v", err)
		}
		raw, err := prog.EvalStringList(ctx, in)
		if err != nil {
			return nil, failf("approvers could not be evaluated: %v", err)
		}
		seen := map[uuid.UUID]bool{}
		for _, s := range raw {
			id, err := uuid.Parse(s)
			if err != nil {
				return nil, failf("approvers: %q is not a user id", s)
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			u, err := q.GetUser(ctx, id)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, failf("approvers: no such user %s", id)
			}
			if err != nil {
				return nil, err
			}
			if u.DeactivatedAt.Valid {
				continue // like group(), which leaves them out: they can no longer sign in to decide
			}
			out = append(out, request{user: id})
		}
	}
	if st.Groups != "" {
		prog, err := st.GroupsProgram()
		if err != nil {
			return nil, failf("this step cannot run: %v", err)
		}
		names, err := prog.EvalStringList(ctx, in)
		if err != nil {
			return nil, failf("groups could not be evaluated: %v", err)
		}
		members := groupMembers(q)
		seen := map[string]bool{}
		for _, name := range names {
			if seen[strings.ToLower(name)] {
				continue
			}
			seen[strings.ToLower(name)] = true
			if _, err := q.GetGroupByName(ctx, name); errors.Is(err, pgx.ErrNoRows) {
				return nil, failf("groups: there is no group named %q", name)
			}
			ids, err := members(ctx, name)
			if err != nil {
				return nil, err
			}
			if len(ids) == 0 {
				return nil, failf("groups: nobody is in the group %q to decide for it", name)
			}
			for _, id := range ids {
				out = append(out, request{user: uuid.MustParse(id), group: name})
			}
		}
	}
	if len(out) == 0 {
		return nil, failf("approval has no approvers")
	}
	return out, nil
}

// approvalDone records an approval step's result in vars, namespaced by the step's id so several approval steps in
// one workflow do not collide: vars.<id>_decision, vars.<id>_comment and vars.<id>_decided_by.
func approvalDone(out outcome, st *apps.Step, vars map[string]any, decision, comment, decidedBy string) outcome {
	next := make(map[string]any, len(vars)+3)
	for k, v := range vars {
		next[k] = v
	}
	next[st.ID+"_decision"], next[st.ID+"_comment"], next[st.ID+"_decided_by"] = decision, comment, decidedBy
	out.vars = next
	out.detail["decision"], out.detail["comment"] = decision, comment
	return out
}

// vote is one say in an approval step: a person asked in their own right, or a group, whose vote is the first
// answer any of its members gives.
type vote struct {
	decision string // approved, rejected, or empty while undecided
	row      dbgen.WorkflowApproval
}

// votes collapses an approval step's requests into its votes, in the order they were asked.
func votes(rows []dbgen.WorkflowApproval) []vote {
	var out []vote
	group := map[string]int{} // group name -> index in out
	for _, r := range rows {
		if !r.GroupName.Valid {
			out = append(out, vote{decision: r.Decision.String, row: r})
			continue
		}
		i, ok := group[r.GroupName.String]
		if !ok {
			group[r.GroupName.String], i = len(out), len(out)
			out = append(out, vote{})
		}
		if r.Decision.Valid && (out[i].decision == "" || r.DecidedAt.Time.Before(out[i].row.DecidedAt.Time)) {
			out[i] = vote{decision: r.Decision.String, row: r}
		}
	}
	return out
}

// pendingApprovers is how many of an approval step's votes are not cast yet.
func pendingApprovers(rows []dbgen.WorkflowApproval) int {
	n := 0
	for _, v := range votes(rows) {
		if v.decision == "" {
			n++
		}
	}
	return n
}

// evaluateApprovals decides an approval step from its votes and its rule (any, all or majority), short-circuiting
// as soon as the outcome cannot change however the rest decide. comment and decider are the deciding vote's: the most
// recently cast, which is exactly the one that just settled it.
func evaluateApprovals(rule string, rows []dbgen.WorkflowApproval) (decision, comment string, decider dbgen.WorkflowApproval, done bool) {
	all := votes(rows)
	total := len(all)
	var approved, rejected int
	for _, v := range all {
		switch v.decision {
		case "approved":
			approved++
		case "rejected":
			rejected++
		default:
			continue
		}
		if !decider.DecidedAt.Valid || v.row.DecidedAt.Time.After(decider.DecidedAt.Time) {
			decider = v.row
		}
	}
	pending := total - approved - rejected
	switch rule {
	case apps.RuleAll:
		switch {
		case rejected > 0:
			decision, done = "rejected", true
		case pending == 0:
			decision, done = "approved", true
		}
	case apps.RuleMajority:
		switch {
		case approved*2 > total:
			decision, done = "approved", true
		case (approved+pending)*2 <= total:
			decision, done = "rejected", true
		}
	default: // any
		switch {
		case approved > 0:
			decision, done = "approved", true
		case pending == 0:
			decision, done = "rejected", true
		}
	}
	if done {
		comment = decider.Comment
	}
	return decision, comment, decider, done
}
