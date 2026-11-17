// Package rules runs the rules apps define (docs/apps-platform.md, §6) against a record that is being saved.
//
// The engine does no database work. A before rule may change the record (set) or stop the save (abort); an after
// rule sees the saved record and may stop the save or ask for a comment. The caller applies the result inside the
// same transaction as the write, so an abort undoes everything.
package rules

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/expr"
)

const (
	// Budget is how long all the rules of one save may take. A rule set that cannot finish in this time is broken,
	// not slow: the save waits on it, and so does the person.
	Budget = 250 * time.Millisecond
	// AsyncBudget is how long one async run may take. Nobody waits, but a run still may not hold a worker forever.
	AsyncBudget = 10 * time.Second
)

// Abort is a rule saying no. Message is what the person sees.
type Abort struct {
	App     string
	Rule    string
	Message string
}

func (a *Abort) Error() string { return a.Message }

// Broken is a rule that could not be evaluated (a value was missing, the cost limit was hit, a value had the wrong
// type). Unless the rule says on_error: skip, it stops the save: failing closed.
type Broken struct {
	App  string
	Rule string
	Err  error
}

func (b *Broken) Error() string { return fmt.Sprintf("rule %s: %v", b.Rule, b.Err) }
func (b *Broken) Unwrap() error { return b.Err }

// Comment is a comment an after rule asked for.
type Comment struct {
	Body     string
	Internal bool
	App      string
	Rule     string
}

// Start is a workflow an after or async rule asked to start on the record. The caller starts it, in the save's
// transaction, so it exists only if the save commits.
type Start struct {
	App      string
	Workflow string
	Rule     string
}

// Notify is a notification an after or async rule asked to send about the record. The engine has no database, so
// it hands back the people it could name (UserIDs) and the groups whose members to add (GroupIDs); the caller
// expands those and delivers it, and delivery drops anyone who must not get it.
type Notify struct {
	App      string
	Rule     string
	Template string
	UserIDs  []string
	GroupIDs []string
}

// Linked is a linked.update an async rule asked for: change each open record linked to this one (Linked names the
// link) by setting Set, then moving it to Status, with Comment on it. The values were worked out against the record
// the rule ran on; the caller finds the linked records and saves each one through the save path.
type Linked struct {
	App, Rule, Linked, Status string
	Set                       map[string]any
	Comment                   string
	Internal                  bool
}

// Effects are what rules asked for besides changing the record: comments to write, workflows to start,
// notifications to send and linked records to change.
type Effects struct {
	Comments []Comment
	Starts   []Start
	Notifies []Notify
	Linked   []Linked
}

// Before runs before rules in order against in.Record, which it never changes: the record as the rules left it is
// returned. Later rules and actions see the earlier ones' changes.
func Before(ctx context.Context, list []*apps.Rule, in expr.Input) (*expr.Record, error) {
	rec, _, err := run(ctx, Budget, list, in, false)
	return rec, err
}

// After runs after rules against the saved record and returns what they ask for.
func After(ctx context.Context, list []*apps.Rule, in expr.Input) (Effects, error) {
	_, fx, err := run(ctx, Budget, list, in, false)
	return fx, err
}

// Fire decides which async rules a save sets off: those whose filter is true for the change, judged with the old
// and new record together, which is only possible at the moment of the save. A filter that cannot be evaluated
// fires nothing and is returned as a failure: an async rule never blocks a save, but it must not vanish quietly.
func Fire(ctx context.Context, list []*apps.Rule, in expr.Input) (fired []*apps.Rule, failed []*Broken) {
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	for _, r := range list {
		ok, err := ruleCondition(ctx, r, &in)
		switch {
		case err != nil:
			failed = append(failed, &Broken{App: r.App, Rule: r.Name, Err: err})
		case ok:
			fired = append(fired, r)
		}
	}
	return fired, failed
}

// RunAsync runs one async rule's actions against the ticket as it is now, and returns the record as the rule's
// record.update actions left it (the caller saves the difference, as one save) with the comments it asked for. The rule's own filter was decided when the
// save fired it and is not asked again; each action's filter is judged on the current data, so an action whose
// condition has stopped being true is skipped. A rule with an action that cannot be evaluated fails whole: the job
// is retried, and nothing is half-done, because the comments come back to the caller to write together.
func RunAsync(ctx context.Context, r *apps.Rule, in expr.Input) (*expr.Record, Effects, error) {
	return run(ctx, AsyncBudget, []*apps.Rule{r}, in, true)
}

// run applies rules in order. With asyncRun the rules' own filters are taken as already true and on_error is
// ignored: an error in an async run is always an error.
func run(ctx context.Context, budget time.Duration, list []*apps.Rule, in expr.Input, asyncRun bool) (*expr.Record, Effects, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	// Work on a copy: the caller's record is only replaced when every rule has run.
	cp := *in.Record
	in.Record = &cp
	var fx Effects
	for _, r := range list {
		// The budget is checked between steps as well as inside expressions, so literals cannot outrun it.
		if err := ctx.Err(); err != nil {
			return nil, Effects{}, &Broken{App: r.App, Rule: r.Name, Err: err}
		}
		// The rule's lookups are counted first, against the record as the earlier rules left it.
		ran, err := true, error(nil)
		if in.Lookups, err = r.Lookups.Eval(ctx, in); err == nil && !asyncRun {
			ran, err = condition(ctx, r.Condition, in)
		}
		if err != nil {
			if fail(r, err) != nil {
				return nil, Effects{}, fail(r, err)
			}
			continue
		}
		if !ran {
			continue
		}
		for i := range r.Run {
			if err := ctx.Err(); err != nil {
				return nil, Effects{}, &Broken{App: r.App, Rule: r.Name, Err: err}
			}
			a := &r.Run[i]
			ok, err := condition(ctx, a.Condition, in)
			if err == nil && ok {
				err = perform(ctx, r, a, in, &fx)
			}
			var ab *Abort
			switch {
			case err == nil:
			case errors.As(err, &ab):
				return nil, Effects{}, err
			case fail(r, err) != nil:
				return nil, Effects{}, fail(r, err)
			}
		}
	}
	return in.Record, fx, nil
}

// fail is what a broken rule does to the save: nothing for on_error: skip, otherwise it stops it. A timeout stops it
// either way, so a rule cannot be skipped into running forever.
func fail(r *apps.Rule, err error) error {
	if r.OnError == apps.OnErrorSkip && !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return &Broken{App: r.App, Rule: r.Name, Err: err}
}

// ruleCondition counts a rule's lookups into in and evaluates its filter.
func ruleCondition(ctx context.Context, r *apps.Rule, in *expr.Input) (bool, error) {
	var err error
	if in.Lookups, err = r.Lookups.Eval(ctx, *in); err != nil {
		return false, err
	}
	return condition(ctx, r.Condition, *in)
}

// condition evaluates a compiled filter: absent means yes; one that could not be compiled or evaluated is an error.
func condition(ctx context.Context, get func() (*expr.Program, error), in expr.Input) (bool, error) {
	p, err := get()
	if err != nil {
		return false, err
	}
	if p == nil {
		return true, nil
	}
	return p.Eval(ctx, in)
}

func perform(ctx context.Context, r *apps.Rule, a *apps.Action, in expr.Input, fx *Effects) error {
	switch a.Action {
	case "abort":
		msg, err := text(ctx, a.Message, in)
		if err != nil || msg == "" {
			msg = "This change was blocked by the rule " + r.Name + "."
		}
		return &Abort{App: r.App, Rule: r.Name, Message: msg}
	case "comment.add":
		body, err := text(ctx, a.Body, in)
		if err != nil {
			return err
		}
		fx.Comments = append(fx.Comments, Comment{Body: body, Internal: a.Internal, App: r.App, Rule: r.Name})
		return nil
	case "workflow.start":
		fx.Starts = append(fx.Starts, Start{App: r.App, Workflow: a.Workflow, Rule: r.Name})
		return nil
	case "notify":
		n := Notify{App: r.App, Rule: r.Name, Template: a.Template}
		rec := in.Record
		for i, to := range a.To {
			switch to {
			case "assignee":
				if rec.AssigneeID != nil {
					n.UserIDs = append(n.UserIDs, *rec.AssigneeID)
				}
			case "requester":
				n.UserIDs = append(n.UserIDs, rec.RequesterID)
			case "assignment_group":
				if rec.AssignmentGroupID != nil {
					n.GroupIDs = append(n.GroupIDs, *rec.AssignmentGroupID)
				}
			default:
				p, err := a.Recipient(i)
				if err != nil {
					return err
				}
				ids, err := p.EvalStringList(ctx, in)
				if err != nil {
					return err
				}
				n.UserIDs = append(n.UserIDs, ids...)
			}
		}
		fx.Notifies = append(fx.Notifies, n)
		return nil
	case "linked.update":
		l := Linked{App: r.App, Rule: r.Name, Linked: a.Linked, Status: a.Status, Internal: a.Internal, Set: map[string]any{}}
		for field, v := range a.Set {
			x, err := value(ctx, v, in)
			if err != nil {
				return fmt.Errorf("set %s: %w", field, err)
			}
			l.Set[field] = x
		}
		if a.Body != nil {
			body, err := text(ctx, a.Body, in)
			if err != nil {
				return err
			}
			l.Comment = body
		}
		fx.Linked = append(fx.Linked, l)
		return nil
	case "set", "record.update":
		// Both change the record in memory. In a before rule that is the save itself; in an async rule the caller
		// saves the difference afterwards, through the same path a person's edit takes.
		return set(ctx, a, in)
	}
	return fmt.Errorf("unknown action %q", a.Action)
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

func text(ctx context.Context, v *apps.Value, in expr.Input) (string, error) {
	x, err := value(ctx, v, in)
	if err != nil {
		return "", err
	}
	s, ok := x.(string)
	if !ok {
		return "", fmt.Errorf("expected text, got %T", x)
	}
	return s, nil
}

// set writes a field of the record the later rules will see. An empty assignee_id unassigns, and an empty
// assignment_group (a group's name) takes the ticket out of its group.
func set(ctx context.Context, a *apps.Action, in expr.Input) error {
	x, err := value(ctx, a.Value, in)
	if err != nil {
		return err
	}
	return Assign(in.Record, a.Field, x)
}

// Assign sets a field of a record to a value a rule worked out: a settable field (apps.SettableFields) or an extra
// field (fields.name).
func Assign(rec *expr.Record, field string, x any) error {
	a := struct{ Field string }{field}
	if name, ok := strings.CutPrefix(a.Field, "fields."); ok {
		// The type was checked when the rule was compiled; the value is checked against the form when the record is saved.
		rec.SetField(name, x)
		return nil
	}
	if a.Field == "priority" {
		n, ok := x.(int64)
		if !ok {
			return fmt.Errorf("priority takes a whole number, got %T", x)
		}
		rec.Priority = n
		return nil
	}
	s, ok := x.(string)
	if !ok {
		return fmt.Errorf("%s takes text, got %T", a.Field, x)
	}
	switch a.Field {
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
		return fmt.Errorf("cannot set %q", a.Field)
	}
	return nil
}
