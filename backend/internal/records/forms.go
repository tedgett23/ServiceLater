package records

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/rules"
)

// formBroken is what a person sees when a form's condition could not be checked: the save is refused, the way a
// guard that cannot be checked blocks its move.
const formBroken = "This record's form has a condition that could not be checked, so the change was not saved. Ask an administrator to review the app that defines it."

// createTouched lists the fields a person filled in when filing, by form field name, for the read-only check.
func createTouched(in CreateInput) []string {
	var out []string
	if in.Priority != nil {
		out = append(out, "priority")
	}
	if c := in.Change; c != nil {
		out = append(out, Patch{Change: c}.touched()...)
	}
	for name, v := range in.Fields {
		if v != nil {
			out = append(out, name)
		}
	}
	return out
}

// checkValues checks the extra fields that changed: each must be a field the form declares, holding a value of its
// type within its limits. It returns next with them as they are stored.
func checkValues(form *apps.Form, cur *Ticket, next Ticket) (Ticket, error) {
	var before map[string]any
	if cur != nil {
		before = cur.Fields
	}
	fields := cloneFields(next.Fields)
	for name, v := range next.Fields {
		if was, ok := before[name]; ok && sameValue(was, v) {
			continue // as it was: a value the form no longer allows stays until someone changes it
		}
		fd, ok := form.Extra(name)
		if !ok {
			return next, Invalid(fmt.Sprintf("%s records have no field %q", next.Type, name))
		}
		norm, msg := fd.Check(v)
		if msg != "" {
			return next, Invalid(fd.Label + " " + msg)
		}
		fields[name] = norm
	}
	next.Fields = fields
	return next, nil
}

// checkReadOnly refuses a request that changes a field while the form makes it read-only. It is judged on the record
// as the request would leave it, before the rules run: a rule may change a read-only field (that is often why it is
// read-only), and must not hide that the person's own change was refused.
func (s *Service) checkReadOnly(ctx context.Context, q *dbgen.Queries, a Actor, event string, cur *Ticket, requested Ticket, touched []string) error {
	form := s.Apps.FormOf(requested.Type, requested.CatalogItem)
	if len(touched) == 0 || !form.HasReadOnly() {
		return nil
	}
	requested, err := checkValues(form, cur, requested)
	if err != nil {
		return err
	}
	in, err := s.ruleInput(ctx, q, a, event, cur, requested)
	if err != nil {
		return err
	}
	states, err := form.States(ctx, in)
	if err != nil {
		slog.Error("form condition", "type", requested.Type, "app", form.App, "ticket", requested.Reference, "err", err)
		return Invalid(formBroken)
	}
	for _, fd := range form.Placed() {
		if states[fd.Name].ReadOnly && slices.Contains(touched, fd.Name) && (cur == nil || !sameField(*cur, requested, fd)) {
			return Invalid(fd.Label + " cannot be changed now")
		}
	}
	return nil
}

// checkForm holds a save to its record type's form, after the before rules have had their say. Extra fields that
// changed must be fields the form declares and hold values of their type; each placed field the form makes required
// must be filled in (unless it is hidden); and the form's own checks must pass. It returns next with the extra fields
// as they are stored.
func (s *Service) checkForm(ctx context.Context, q *dbgen.Queries, a Actor, event string, cur *Ticket, next Ticket) (Ticket, error) {
	form := s.Apps.FormOf(next.Type, next.CatalogItem)
	next, err := checkValues(form, cur, next)
	if err != nil {
		return next, err
	}
	var before map[string]any
	if cur != nil {
		before = cur.Fields
	}
	in, err := s.ruleInput(ctx, q, a, event, cur, next)
	if err != nil {
		return next, err
	}
	states, err := form.States(ctx, in)
	if err != nil {
		slog.Error("form condition", "type", next.Type, "app", form.App, "ticket", next.Reference, "err", err)
		return next, Invalid(formBroken)
	}
	for _, fd := range form.Placed() {
		st := states[fd.Name]
		if st.Required && isEmpty(next, fd) {
			return next, Invalid(fd.Label + " is required")
		}
		if st.Options != nil && !fd.Core() {
			if v, ok := next.Fields[fd.Name].(string); ok && (cur == nil || !sameValue(before[fd.Name], v)) && !slices.Contains(st.Options, v) {
				return next, Invalid(fmt.Sprintf("%s cannot be %q here", fd.Label, v))
			}
		}
	}
	msg, err := form.Checks(ctx, in)
	if err != nil {
		slog.Error("form check", "type", next.Type, "app", form.App, "ticket", next.Reference, "err", err)
		return next, Invalid(formBroken)
	}
	if msg != "" {
		return next, Invalid(msg)
	}
	return next, nil
}

// value is a field's value on a ticket, core or extra, and whether it is empty.
func value(t Ticket, fd apps.FormField) (any, bool) {
	if !fd.Core() {
		v, ok := t.Fields[fd.Name]
		return v, !ok || v == nil || v == ""
	}
	c := t.Change
	if c == nil {
		c = &Change{}
	}
	switch fd.Name {
	case "title":
		return t.Title, t.Title == ""
	case "description":
		return t.Description, t.Description == ""
	case "priority":
		return t.Priority, false
	case "resolution":
		return t.Resolution, t.Resolution == ""
	case "change_type":
		return c.ChangeType, c.ChangeType == ""
	case "risk":
		return c.Risk, c.Risk == ""
	case "impact":
		return c.Impact, c.Impact == ""
	case "planned_start":
		return c.PlannedStart, c.PlannedStart == nil
	case "planned_end":
		return c.PlannedEnd, c.PlannedEnd == nil
	case "implementation_plan":
		return c.ImplementationPlan, c.ImplementationPlan == ""
	case "backout_plan":
		return c.BackoutPlan, c.BackoutPlan == ""
	case "test_plan":
		return c.TestPlan, c.TestPlan == ""
	}
	return nil, true
}

func isEmpty(t Ticket, fd apps.FormField) bool {
	_, empty := value(t, fd)
	return empty
}

func sameField(a, b Ticket, fd apps.FormField) bool {
	x, xe := value(a, fd)
	y, ye := value(b, fd)
	if xe || ye {
		return xe == ye
	}
	if xt, ok := x.(*time.Time); ok {
		yt, _ := y.(*time.Time)
		return sameTime(xt, yt)
	}
	return sameValue(x, y)
}

// FieldStates is each field's state on a saved ticket, as `actor` sees it now. A condition that cannot be evaluated
// leaves the states out (the record still shows; a save would be refused and say why).
func (s *Service) FieldStates(ctx context.Context, q *dbgen.Queries, a Actor, t Ticket) map[string]apps.FieldState {
	cur := t
	in, err := s.ruleInput(ctx, q, a, apps.EventUpdate, &cur, t)
	if err != nil {
		return nil
	}
	form := s.Apps.FormOf(t.Type, t.CatalogItem)
	states, err := form.States(ctx, in)
	if err != nil {
		slog.Error("form condition", "type", t.Type, "app", form.App, "ticket", t.Reference, "err", err)
		return nil
	}
	return states
}

// DraftStates is each field's state on a record being filed, before it is saved: what the form for filing shows as
// the person fills it in. The before rules are tried on the draft (nothing is written), so the states are those of
// the record the save would make, and Preview has the values the rules would give the form's fields (a priority
// worked out from impact and urgency). The save checks everything again.
func (s *Service) DraftStates(ctx context.Context, q *dbgen.Queries, a Actor, in CreateInput) (states map[string]apps.FieldState, preview map[string]any, err error) {
	form := s.Apps.FormOf(in.Type, in.CatalogItem)
	lc := s.Apps.Lifecycle(in.Type)
	category, _ := lc.Category(lc.Initial)
	prio := int16(3)
	if in.Priority != nil {
		prio = *in.Priority
	}
	draft := Ticket{Type: in.Type, Title: in.Title, Description: in.Description, Priority: prio, RequesterID: a.ID,
		Status: lc.Initial, StateCategory: string(category), Fields: form.Defaults(), CatalogItem: in.CatalogItem}
	if in.Type == "change" {
		draft.Change = &Change{ChangeType: "normal"}
		if !in.Change.empty() {
			draft.Change = in.Change.apply(draft.Change)
		}
	}
	for k, v := range in.Fields {
		if fd, ok := form.Extra(k); ok && v != nil {
			if norm, msg := fd.Check(v); msg == "" {
				draft.Fields[k] = norm
			}
		}
	}
	// A rule that would refuse the draft (or cannot run) changes nothing here, and is not audited: the save will say so.
	if ruled, ok := s.tryBefore(ctx, q, a, draft); ok {
		preview = map[string]any{}
		for _, fd := range form.Placed() {
			if !sameField(draft, ruled, fd) {
				v, _ := value(ruled, fd)
				preview[fd.Name] = v
			}
		}
		draft = ruled
	}
	input, err := s.ruleInput(ctx, q, a, apps.EventCreate, nil, draft)
	if err != nil {
		return nil, nil, err
	}
	states, err = form.States(ctx, input)
	if err != nil {
		slog.Error("form condition", "type", in.Type, "app", form.App, "err", err)
		return nil, nil, Invalid(formBroken)
	}
	return states, preview, nil
}

// tryBefore runs the before rules for filing over a draft, writing nothing (not even the audit), and reports whether
// they would let it through.
func (s *Service) tryBefore(ctx context.Context, q *dbgen.Queries, a Actor, draft Ticket) (Ticket, bool) {
	list := s.Apps.Rules(draft.Type, apps.TimingBefore, apps.EventCreate)
	if len(list) == 0 {
		return draft, true
	}
	in, err := s.ruleInput(ctx, q, a, apps.EventCreate, nil, draft)
	if err != nil {
		return draft, false
	}
	res, err := rules.Before(ctx, list, in)
	if err != nil {
		return draft, false
	}
	out, err := FromRecord(ctx, q, draft, res)
	return out, err == nil
}
