package workflowengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/script"
)

// maxScriptVars is the most a script's variables may come to, as JSON.
const maxScriptVars = 64 << 10

// doScript runs a script step: the script reads what it needs and asks for changes, which are made here, in the
// step's transaction, as the run's actor (so a retry after a failure never makes them twice, and a refused change
// undoes the rest). Its vars join the run's; it leaves by next, or by the branch it names.
func (e *Engine) doScript(ctx context.Context, q *dbgen.Queries, ex dbgen.WorkflowExecution, st *apps.Step, in expr.Input, ticket *records.Ticket, out outcome) (outcome, error) {
	prog, err := st.Program()
	if err != nil {
		return out, failf("the script cannot run: %v", err)
	}
	env := script.Env{User: in.User, Vars: in.Vars, Now: in.Now, Reader: records.ScriptReader(q), Count: records.Counter(q),
		Limits: script.RunLimits, Allow: script.Allow{Own: ticket != nil, Others: true, Comments: true, Abort: true}}
	if ticket != nil {
		env.Record = in.Record
	}
	res, err := prog.Run(ctx, env)
	var ab *script.Abort
	var se *script.Error
	switch {
	case errors.As(err, &ab):
		return out, failf("the script stopped: %s", ab.Message)
	case errors.As(err, &se):
		return out, failf("the script failed at %v", se)
	case err != nil:
		return out, err // our side (the database): try again
	}
	if len(res.Printed) > 0 {
		out.detail["printed"] = res.Printed[:min(len(res.Printed), 20)]
	}
	out.detail["changes"], out.detail["comments"] = len(res.Updates), len(res.Comments)

	if res.Next != "" {
		to, ok := st.Branches[res.Next]
		if !ok {
			return out, failf("the script chose %q, which is not one of its branches (%s)", res.Next, strings.Join(st.BranchNames(), ", "))
		}
		out.next, out.detail["branch"] = to, res.Next
	}
	if len(res.Vars) > 0 {
		next := make(map[string]any, len(in.Vars)+len(res.Vars))
		for k, v := range in.Vars {
			next[k] = v
		}
		for k, v := range res.Vars {
			next[k] = v
		}
		if raw, err := json.Marshal(next); err != nil || len(raw) > maxScriptVars {
			return out, failf("the script's variables come to more than %d KB", maxScriptVars>>10)
		}
		out.vars, out.detail["set"] = next, res.Vars
	}

	if len(res.Updates)+len(res.Comments) == 0 {
		return out, nil
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
	if err := e.Svc.ApplyScript(ctx, q, actor, cause, res); err != nil {
		var sc *records.ScriptError
		var re *records.Error
		switch {
		case errors.As(err, &re):
			return out, failf("the script's change to %s was refused: %s", refOf(err), re.Msg)
		case isForbidden(err):
			return out, failf("the %s app may not make the script's change to %s: add the permission it needs to its manifest", ex.App, refOf(err))
		case errors.As(err, &sc):
			return out, failf("the script's change to %s failed: %v", sc.Reference, sc.Err)
		}
		return out, err
	}
	return out, nil
}

func refOf(err error) string {
	var sc *records.ScriptError
	if errors.As(err, &sc) {
		return sc.Reference
	}
	return "a record"
}
