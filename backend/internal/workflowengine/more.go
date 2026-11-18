package workflowengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/rules"
)

// withVars is out with more variables set (a copy: the stored ones change only if the step succeeds).
func withVars(out outcome, set map[string]any) outcome {
	next := make(map[string]any, len(out.vars)+len(set))
	for k, v := range out.vars {
		next[k] = v
	}
	for k, v := range set {
		next[k] = v
	}
	out.vars = next
	return out
}

// Find runs a query's filters for in, as whoever q runs as: the ids of what it finds.
func Find(ctx context.Context, q *dbgen.Queries, rq *apps.RecordQuery, in expr.Input) ([]uuid.UUID, error) {
	filters, err := rq.Filters(ctx, in)
	if err != nil {
		return nil, failf("find: %v", err)
	}
	if filters == nil {
		filters = []expr.Filter{}
	}
	raw, err := json.Marshal(filters)
	if err != nil {
		return nil, err
	}
	ids, err := q.FindRecords(ctx, dbgen.FindRecordsParams{Type: rq.Type, Filters: raw, Max: int32(rq.Max())}) //nolint:gosec // at most MaxFind
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "22023" {
		return nil, failf("find: %s", pe.Message)
	}
	return ids, err
}

// doQuery finds records and keeps their ids: <step>_ids, <step>_references and <step>_count.
func (e *Engine) doQuery(ctx context.Context, q *dbgen.Queries, st *apps.Step, in expr.Input, out outcome) (outcome, error) {
	ids, err := Find(ctx, q, st.Find, in)
	if err != nil {
		return out, err
	}
	idList, refs := make([]any, len(ids)), make([]any, 0, len(ids))
	for i, id := range ids {
		idList[i] = id.String()
		if t, err := records.Load(ctx, q, id); err == nil {
			refs = append(refs, t.Reference)
		}
	}
	out.detail["found"] = len(ids)
	return withVars(out, map[string]any{st.ID + "_ids": idList, st.ID + "_references": refs, st.ID + "_count": int64(len(ids))}), nil
}

// doRecordCreate files a record of any type through the save path, as the run's actor, and keeps <step>_id and
// <step>_reference. The id is kept in the same transaction, so a retry finds it and never files a second one.
func (e *Engine) doRecordCreate(ctx context.Context, q *dbgen.Queries, ex dbgen.WorkflowExecution, st *apps.Step, in expr.Input, out outcome) (outcome, error) {
	if id, ok := in.Vars[st.ID+"_id"].(string); ok && id != "" {
		out.detail["record"] = in.Vars[st.ID+"_reference"]
		return out, nil
	}
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
	input := records.CreateInput{Type: st.RecordType, Title: title, Description: desc}
	if ex.StartedBy != uuid.Nil {
		person := ex.StartedBy
		input.RequesterID = &person
	}
	if len(st.Fields) > 0 {
		input.Fields = map[string]any{}
		for _, nv := range st.Fields {
			x, err := jsonValue(ctx, nv.Value, in)
			if err != nil {
				return out, failf("fields.%s could not be evaluated: %v", nv.Name, err)
			}
			input.Fields[nv.Name] = x
		}
	}
	if groupName, err := text("group", st.Group); err != nil {
		return out, err
	} else if groupName != "" {
		g, err := q.GetGroupByName(ctx, groupName)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			out.detail["unrouted"] = "there is no group named " + groupName + ", so the record was filed without a team"
		case err != nil:
			return out, err
		default:
			input.AssignmentGroupID = &g.ID
		}
	}
	if st.Parent != nil {
		x, err := value(ctx, st.Parent, in)
		if err != nil {
			return out, failf("parent could not be evaluated: %v", err)
		}
		pid, err := e.recordOf(ctx, q, x)
		if err != nil {
			return out, failf("parent: %v", err)
		}
		input.ParentID = &pid
	}
	var cause ruleruns.Cause
	_ = json.Unmarshal(ex.Cause, &cause)
	actor, err := records.ActorIn(ctx, q, runnerOf(ex))
	if err != nil {
		return out, err
	}
	t, err := e.Svc.Create(ctx, q, actor, cause, input)
	if err != nil {
		var re *records.Error
		if errors.As(err, &re) {
			return out, failf("the %s could not be filed: %s", st.RecordType, re.Msg)
		}
		if isForbidden(err) {
			return out, failf("the %s app may not file %s records: add records:create:%s to its manifest", ex.App, st.RecordType, st.RecordType)
		}
		return out, err
	}
	out.detail["record"] = t.Reference
	return withVars(out, map[string]any{st.ID + "_id": t.ID.String(), st.ID + "_reference": t.Reference}), nil
}

// doNotify sends one of the app's notification templates about the record, to the people the step names, the way a
// rule's notify does (the database still leaves out anyone who may not see the record).
func (e *Engine) doNotify(ctx context.Context, q *dbgen.Queries, ex dbgen.WorkflowExecution, st *apps.Step, in expr.Input, t records.Ticket, out outcome) (outcome, error) {
	n := rules.Notify{App: ex.App, Rule: "workflow " + ex.Workflow + ", step " + st.ID, Template: st.Template}
	progs := st.RecipientPrograms()
	for i, to := range st.Recipients {
		switch to {
		case "assignee":
			if t.AssigneeID != nil {
				n.UserIDs = append(n.UserIDs, t.AssigneeID.String())
			}
		case "requester":
			n.UserIDs = append(n.UserIDs, t.RequesterID.String())
		case "assignment_group":
			if t.AssignmentGroupID != nil {
				n.GroupIDs = append(n.GroupIDs, t.AssignmentGroupID.String())
			}
		default:
			if i >= len(progs) || progs[i] == nil {
				return out, failf("recipients[%d] cannot run", i)
			}
			ids, err := progs[i].EvalStringList(ctx, in)
			if err != nil {
				return out, failf("recipients[%d] could not be evaluated: %v", i, err)
			}
			n.UserIDs = append(n.UserIDs, ids...)
		}
	}
	out.detail["template"], out.detail["people"], out.detail["groups"] = st.Template, len(n.UserIDs), len(n.GroupIDs)
	return out, e.Svc.NotifyFromAsyncRule(ctx, q, t, n)
}

// runnerOf is who a run's steps act as: its app's principal, else the person who started it.
func runnerOf(ex dbgen.WorkflowExecution) uuid.UUID {
	if ex.RunAs.Valid {
		return ex.RunAs.UUID
	}
	return ex.StartedBy
}
