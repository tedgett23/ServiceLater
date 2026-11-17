package records

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/rules"
)

// CannotCheck is what a person sees when a lifecycle guard could not be evaluated.
const CannotCheck = "This move is blocked because its rule could not be checked. Ask an administrator to review the app that defines it."

const ruleBroken = "A rule could not be checked, so this change was not saved. Ask an administrator to review the app that defines it."

// Service saves tickets. Apps supplies lifecycles and rules; Pool is used only for the audit, which is written on a
// connection of its own so that it survives the rollback of the save it describes.
type Service struct {
	Apps *apps.Registry
	Pool *pgxpool.Pool
}

// Actor is who is saving. Rules and permissions are judged as them, and the database applies RLS as them.
type Actor struct {
	ID    uuid.UUID
	Perms authz.Set
	// OnBehalfOf is set when the actor is an app's principal acting for a person (an async rule, a workflow, a
	// run_as: app rule): the person whose change set it off.
	OnBehalfOf uuid.UUID
}

// Can reports whether the actor has a permission.
func (a Actor) Can(perm string) bool { return a.Perms.Has(perm) }

// CanRecord reports whether the actor may do verb to records of a type.
func (a Actor) CanRecord(verb, recordType string) bool { return a.Perms.CanRecord(verb, recordType) }

// needs is the refusal for an actor without verb on a record type.
func needs(doing, recordType, verb string) error {
	return Forbidden(fmt.Sprintf("%s needs the %s permission", doing, authz.Record(verb, recordType)))
}

// Person is who a change is for: the person acting, or the one an app acts for.
func (a Actor) Person() uuid.UUID {
	if a.OnBehalfOf != uuid.Nil {
		return a.OnBehalfOf
	}
	return a.ID
}

// ActorIn is who a transaction made by db.WithUser or db.WithPrincipal runs as, with the permissions it loaded. The
// worker and the workflow engine use it; a request already knows its caller's.
func ActorIn(ctx context.Context, q *dbgen.Queries, id uuid.UUID) (Actor, error) {
	s, err := q.CurrentPermissions(ctx)
	if err != nil {
		return Actor{}, err
	}
	behalf, err := q.CurrentOnBehalfOf(ctx)
	return Actor{ID: id, Perms: authz.Parse(s), OnBehalfOf: behalf}, err
}

// assignable reports whether someone may be given a ticket: they must be able to work records of its type.
func assignable(ctx context.Context, q *dbgen.Queries, id uuid.UUID, recordType string) (bool, error) {
	return q.UserCanRecord(ctx, dbgen.UserCanRecordParams{UserID: id, Verb: authz.Update, RecordType: recordType})
}

// CreateInput is a new record as asked for.
type CreateInput struct {
	// Type is the record type (incident, change...). It must be in use: an installed app defines its lifecycle.
	Type               string
	Title, Description string
	Priority           *int16
	RequesterID        *uuid.UUID // staff may file on behalf of someone else
	// Change is what to fill in of a change's own fields; only a change has them.
	Change *ChangePatch
	// Fields are extra fields to fill in, by name; a nil value is left out. The form's defaults fill the rest.
	Fields map[string]any
	// CIs are configuration items to link it to (needs cmdb:read).
	CIs []uuid.UUID
	// ParentID makes it part of another record (a task under a request); the parent must be one the actor can see.
	ParentID *uuid.UUID
	// AssignmentGroupID routes it to a team as it is filed.
	AssignmentGroupID *uuid.UUID
	// ProblemID links a new incident to the problem behind it.
	ProblemID *uuid.UUID
	// CatalogItem orders a request from the catalog: the item's form asks the questions (Fields), its title is the
	// request's, and its workflow starts once the request is filed. Title is then ignored.
	CatalogItem string
}

// ChangePatch is a change to a change's own fields. A nil field is left alone; SetPlannedStart with a nil
// PlannedStart clears the start, and likewise the end.
type ChangePatch struct {
	ChangeType, Risk, Impact              *string
	SetPlannedStart, SetPlannedEnd        bool
	PlannedStart, PlannedEnd              *time.Time
	ImplementationPlan, BackoutPlan, Test *string
}

func (p *ChangePatch) empty() bool {
	return p == nil || p.ChangeType == nil && p.Risk == nil && p.Impact == nil && !p.SetPlannedStart && !p.SetPlannedEnd &&
		p.ImplementationPlan == nil && p.BackoutPlan == nil && p.Test == nil
}

// apply returns c with the patch applied.
func (p *ChangePatch) apply(c *Change) *Change {
	out := c.clone()
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&out.ChangeType, p.ChangeType)
	set(&out.Risk, p.Risk)
	set(&out.Impact, p.Impact)
	set(&out.ImplementationPlan, p.ImplementationPlan)
	set(&out.BackoutPlan, p.BackoutPlan)
	set(&out.TestPlan, p.Test)
	if p.SetPlannedStart {
		out.PlannedStart = p.PlannedStart
	}
	if p.SetPlannedEnd {
		out.PlannedEnd = p.PlannedEnd
	}
	return out
}

// changeBetween is the patch that turns a into b.
func changeBetween(a, b *Change) *ChangePatch {
	if a == nil || b == nil || sameChange(a, b) {
		return nil
	}
	p := &ChangePatch{}
	diff := func(x, y string) *string {
		if x == y {
			return nil
		}
		return &y
	}
	p.ChangeType, p.Risk, p.Impact = diff(a.ChangeType, b.ChangeType), diff(a.Risk, b.Risk), diff(a.Impact, b.Impact)
	p.ImplementationPlan, p.BackoutPlan, p.Test = diff(a.ImplementationPlan, b.ImplementationPlan), diff(a.BackoutPlan, b.BackoutPlan), diff(a.TestPlan, b.TestPlan)
	if !sameTime(a.PlannedStart, b.PlannedStart) {
		p.SetPlannedStart, p.PlannedStart = true, b.PlannedStart
	}
	if !sameTime(a.PlannedEnd, b.PlannedEnd) {
		p.SetPlannedEnd, p.PlannedEnd = true, b.PlannedEnd
	}
	return p
}

// Patch is a change to a ticket. A nil field is left alone; SetAssignee with a nil AssigneeID unassigns, and
// SetAssignmentGroup with a nil AssignmentGroupID takes the ticket out of its group.
type Patch struct {
	Title, Description *string
	Priority           *int16
	SetAssignee        bool
	AssigneeID         *uuid.UUID
	SetAssignmentGroup bool
	AssignmentGroupID  *uuid.UUID
	Resolution         *string
	Change             *ChangePatch
	// Fields sets extra fields by name; a nil value clears one.
	Fields map[string]any
	// SetCIs replaces the configuration items linked to it with CIs (needs cmdb:read). Linking or unlinking an item
	// is an update like any other, so rules run on it.
	SetCIs bool
	CIs    []uuid.UUID
	// SetProblem links an incident to ProblemID, or unlinks it when ProblemID is nil.
	SetProblem bool
	ProblemID  *uuid.UUID
}

// Empty reports whether the patch changes nothing.
func (p Patch) Empty() bool {
	return p.Title == nil && p.Description == nil && p.Priority == nil && !p.SetAssignee && !p.SetAssignmentGroup &&
		p.Resolution == nil && p.Change.empty() && len(p.Fields) == 0 && !p.SetCIs && !p.SetProblem
}

// touched lists the fields a person's patch writes, by form field name, for the read-only check.
func (p Patch) touched() []string {
	var out []string
	add := func(name string, set bool) {
		if set {
			out = append(out, name)
		}
	}
	add("title", p.Title != nil)
	add("description", p.Description != nil)
	add("priority", p.Priority != nil)
	add("resolution", p.Resolution != nil)
	add("problem", p.SetProblem)
	if c := p.Change; c != nil {
		add("change_type", c.ChangeType != nil)
		add("risk", c.Risk != nil)
		add("impact", c.Impact != nil)
		add("planned_start", c.SetPlannedStart)
		add("planned_end", c.SetPlannedEnd)
		add("implementation_plan", c.ImplementationPlan != nil)
		add("backout_plan", c.BackoutPlan != nil)
		add("test_plan", c.Test != nil)
	}
	for name := range p.Fields {
		out = append(out, name)
	}
	return out
}

// PatchBetween is the patch that turns t into want, for the fields a patch can change.
func PatchBetween(t, want Ticket) Patch {
	var p Patch
	if want.Title != t.Title {
		p.Title = &want.Title
	}
	if want.Description != t.Description {
		p.Description = &want.Description
	}
	if want.Priority != t.Priority {
		p.Priority = &want.Priority
	}
	if !sameUUID(want.AssigneeID, t.AssigneeID) {
		p.SetAssignee, p.AssigneeID = true, want.AssigneeID
	}
	if !sameUUID(want.AssignmentGroupID, t.AssignmentGroupID) {
		p.SetAssignmentGroup, p.AssignmentGroupID = true, want.AssignmentGroupID
	}
	if want.Resolution != t.Resolution {
		p.Resolution = &want.Resolution
	}
	if !sameUUID(want.ProblemID, t.ProblemID) {
		p.SetProblem, p.ProblemID = true, want.ProblemID
	}
	p.Change = changeBetween(t.Change, want.Change)
	for k, v := range want.Fields {
		if w, ok := t.Fields[k]; !ok || !sameValue(v, w) {
			if p.Fields == nil {
				p.Fields = map[string]any{}
			}
			p.Fields[k] = v
		}
	}
	for k := range t.Fields {
		if _, ok := want.Fields[k]; !ok {
			if p.Fields == nil {
				p.Fields = map[string]any{}
			}
			p.Fields[k] = nil
		}
	}
	return p
}

// q must be built on the transaction of the save (db.WithUser), so the ticket, its comments, the jobs it queues and
// what the rules did all commit or roll back together.

// Create files a record of any type in use.
func (s *Service) Create(ctx context.Context, q *dbgen.Queries, a Actor, cause ruleruns.Cause, in CreateInput) (Ticket, error) {
	tt, err := q.GetTaskType(ctx, in.Type)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !s.Apps.Provides(in.Type)) {
		return Ticket{}, Invalid(fmt.Sprintf("%q is not a record type in use", in.Type))
	} else if err != nil {
		return Ticket{}, err
	}
	// Anyone may file a record of an open type (incidents); any other type needs records:create:<type>. A type ordered
	// from the catalog (requests) is open to anyone only through an item they may order.
	var item *apps.CatalogItem
	switch {
	case in.CatalogItem != "":
		if !tt.FromCatalog {
			return Ticket{}, Invalid("only a request is ordered from the catalog")
		}
		if item = s.Apps.CatalogItem(in.CatalogItem); item == nil {
			return Ticket{}, Invalid(fmt.Sprintf("there is no catalog item %q", in.CatalogItem))
		}
		ok, err := MayOrder(ctx, q, a.Person(), item)
		if err != nil {
			return Ticket{}, err
		}
		if !ok {
			return Ticket{}, Forbidden(fmt.Sprintf("%s is not available to you", item.Label))
		}
		in.Title = item.Label // worked out from the answers below
	case tt.FromCatalog && !a.CanRecord(authz.Create, tt.Type):
		return Ticket{}, needs("filing a "+strings.ToLower(tt.Label)+" outside the catalog", tt.Type, authz.Create)
	case !tt.AnyoneMayFile && !a.CanRecord(authz.Create, tt.Type):
		return Ticket{}, needs("filing a "+strings.ToLower(tt.Label), tt.Type, authz.Create)
	}
	if err := validTitle(in.Title); err != nil {
		return Ticket{}, err
	}
	if len(in.Description) > MaxDescription {
		return Ticket{}, Invalid("description too long")
	}
	prio := int16(3)
	if in.Priority != nil {
		prio = *in.Priority
	}
	if err := validPriority(prio); err != nil {
		return Ticket{}, err
	}
	requester := a.ID
	if in.RequesterID != nil && *in.RequesterID != a.ID {
		if !a.CanRecord(authz.Update, tt.Type) {
			return Ticket{}, needs("filing a "+strings.ToLower(tt.Label)+" for someone else", tt.Type, authz.Update)
		}
		requester = *in.RequesterID
	}
	lc := s.Apps.Lifecycle(tt.Type)
	category, _ := lc.Category(lc.Initial)
	next := Ticket{Type: tt.Type, Title: strings.TrimSpace(in.Title), Description: in.Description, Priority: prio,
		RequesterID: requester, Status: lc.Initial, StateCategory: string(category)}
	if tt.Type == "change" {
		next.Change = &Change{ChangeType: "normal"}
		if !in.Change.empty() {
			next.Change = in.Change.apply(next.Change)
		}
		if err := validChange(next.Change); err != nil {
			return Ticket{}, err
		}
	} else if !in.Change.empty() {
		return Ticket{}, Invalid("only a change has change fields")
	}
	if len(in.CIs) > 0 {
		if next.CIs, err = resolveCIs(ctx, q, a, in.CIs); err != nil {
			return Ticket{}, err
		}
	}
	if in.ParentID != nil {
		parent, err := q.GetTask(ctx, *in.ParentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return Ticket{}, Invalid("there is no record to put it under")
		} else if err != nil {
			return Ticket{}, err
		}
		if parent.ParentID.Valid {
			return Ticket{}, Invalid("a record under another cannot have records of its own")
		}
		next.ParentID = &parent.ID
	}
	if in.ProblemID != nil {
		if next.ProblemID, err = problemFor(ctx, q, tt.Type, *in.ProblemID); err != nil {
			return Ticket{}, err
		}
	}
	if in.AssignmentGroupID != nil {
		g, err := q.GetGroup(ctx, *in.AssignmentGroupID)
		if errors.Is(err, pgx.ErrNoRows) {
			return Ticket{}, Invalid("assignment group does not exist")
		} else if err != nil {
			return Ticket{}, err
		}
		next.AssignmentGroupID, next.AssignmentGroup = &g.ID, &g.Name
	}
	next.CatalogItem = in.CatalogItem
	form := s.Apps.FormOf(tt.Type, in.CatalogItem)
	next.Fields = form.Defaults()
	for k, v := range in.Fields {
		if v != nil {
			next.Fields[k] = v
		}
	}
	if item != nil {
		// The answers as given, normalised as the form check will have them, so the title reads what is saved.
		draft := next
		draft.Fields = cloneFields(next.Fields)
		for k, v := range draft.Fields {
			if fd, ok := form.Extra(k); ok {
				if norm, msg := fd.Check(v); msg == "" {
					draft.Fields[k] = norm
				}
			}
		}
		if next.Title, err = itemTitle(ctx, q, a, item, draft); err != nil {
			return Ticket{}, err
		}
	}
	if err := s.checkReadOnly(ctx, q, a, apps.EventCreate, nil, next, createTouched(in)); err != nil {
		return Ticket{}, err
	}
	// Before rules see the ticket as it is about to be written and may change it or stop the save.
	next, err = s.runBefore(ctx, q, a, apps.EventCreate, nil, next)
	if err != nil {
		return Ticket{}, err
	}
	if next, err = s.checkForm(ctx, q, a, apps.EventCreate, nil, next); err != nil {
		return Ticket{}, err
	}
	// A ticket is a task plus its type's own row. Both are written in this transaction, or neither is. Its id is
	// marked as being saved first, so a record filed straight into a restricted group still comes back to its filer.
	newID := uuid.New()
	if err := q.MarkSaving(ctx, newID); err != nil {
		return Ticket{}, err
	}
	t, err := q.CreateTask(ctx, dbgen.CreateTaskParams{
		ID: uuid.NullUUID{UUID: newID, Valid: true}, Type: next.Type, Title: strings.TrimSpace(next.Title), Description: next.Description, Priority: next.Priority, RequesterID: requester,
		Status: lc.Initial, StateCategory: string(category), AssignmentGroupID: nullUUID(next.AssignmentGroupID),
		Fields: EncodeFields(next.Fields), ParentID: nullUUID(next.ParentID)})
	if err != nil {
		return Ticket{}, err
	}
	if err := createExtension(ctx, q, t, in.CatalogItem); err != nil {
		return Ticket{}, err
	}
	// What the rules changed beyond the columns the insert takes is written now.
	created, err := Load(ctx, q, t.ID)
	if err != nil {
		return Ticket{}, err
	}
	final := withRuleFields(created, next)
	final.CIs = next.CIs
	if err := writeChanges(ctx, q, created, final); err != nil {
		return Ticket{}, err
	}
	out, err := Load(ctx, q, t.ID)
	if err != nil {
		return Ticket{}, err
	}
	if err := s.runAfter(ctx, q, a, cause, apps.EventCreate, nil, out); err != nil {
		return Ticket{}, err
	}
	if item != nil {
		return out, s.startItemWorkflow(ctx, q, a, cause, item, out)
	}
	return out, nil
}

// Update edits a ticket. It locks the row first, so rules see the ticket as it is and two edits cannot both act on
// the same state.
func (s *Service) Update(ctx context.Context, q *dbgen.Queries, a Actor, cause ruleruns.Cause, id uuid.UUID, p Patch) (Ticket, error) {
	if err := s.mayUpdate(ctx, q, a, id, "editing"); err != nil {
		return Ticket{}, err
	}
	if p.Title != nil {
		if err := validTitle(*p.Title); err != nil {
			return Ticket{}, err
		}
	}
	if p.Description != nil && len(*p.Description) > MaxDescription {
		return Ticket{}, Invalid("description too long")
	}
	if p.Priority != nil {
		if err := validPriority(*p.Priority); err != nil {
			return Ticket{}, err
		}
	}
	if p.Resolution != nil && len(*p.Resolution) > MaxResolution {
		return Ticket{}, Invalid("resolution notes are too long")
	}
	if _, err := q.GetTaskForUpdate(ctx, id); err != nil {
		return Ticket{}, err
	}
	// Read under RLS just now, so the caller sees it: it stays in view for the rest of this save even if the save
	// routes it to a restricted group they are not in.
	if err := q.MarkSaving(ctx, id); err != nil {
		return Ticket{}, err
	}
	cur, err := Load(ctx, q, id)
	if err != nil {
		return Ticket{}, err
	}
	next := cur
	if p.Title != nil {
		next.Title = strings.TrimSpace(*p.Title)
	}
	if p.Description != nil {
		next.Description = *p.Description
	}
	if p.Priority != nil {
		next.Priority = *p.Priority
	}
	if p.SetAssignee {
		next.AssigneeID = nil
		if p.AssigneeID != nil {
			u, err := q.GetUser(ctx, *p.AssigneeID)
			if err != nil {
				return Ticket{}, Invalid("assignee does not exist")
			}
			if ok, err := assignable(ctx, q, u.ID, cur.Type); err != nil {
				return Ticket{}, err
			} else if !ok {
				return Ticket{}, Invalid("assignee must be someone who works tickets of this type")
			}
			if u.DeactivatedAt.Valid {
				return Ticket{}, Invalid("assignee is deactivated")
			}
			next.AssigneeID = p.AssigneeID
		}
	}
	if p.SetAssignmentGroup {
		next.AssignmentGroupID, next.AssignmentGroup = nil, nil
		if p.AssignmentGroupID != nil {
			g, err := q.GetGroup(ctx, *p.AssignmentGroupID)
			if err != nil {
				return Ticket{}, Invalid("assignment group does not exist")
			}
			next.AssignmentGroupID, next.AssignmentGroup = &g.ID, &g.Name
		}
	}
	if p.Resolution != nil {
		// The resolution belongs to the incident's own row, so only tickets of that type have one.
		if cur.Type != "incident" {
			return Ticket{}, Invalid("this kind of ticket has no resolution notes")
		}
		next.Resolution = *p.Resolution
	}
	if !p.Change.empty() {
		if cur.Change == nil {
			return Ticket{}, Invalid("only a change has change fields")
		}
		next.Change = p.Change.apply(cur.Change)
		if err := validChange(next.Change); err != nil {
			return Ticket{}, err
		}
	}
	if len(p.Fields) > 0 {
		next.Fields = cloneFields(cur.Fields)
		for k, v := range p.Fields {
			if v == nil || v == "" {
				delete(next.Fields, k)
			} else {
				next.Fields[k] = v
			}
		}
	}
	if p.SetCIs {
		if next.CIs, err = resolveCIs(ctx, q, a, p.CIs); err != nil {
			return Ticket{}, err
		}
	}
	if p.SetProblem {
		next.ProblemID = nil
		if p.ProblemID != nil {
			if next.ProblemID, err = problemFor(ctx, q, cur.Type, *p.ProblemID); err != nil {
				return Ticket{}, err
			}
		} else if cur.Type != "incident" {
			return Ticket{}, Invalid("only an incident is linked to a problem")
		}
	}
	if err := s.checkReadOnly(ctx, q, a, apps.EventUpdate, &cur, next, p.touched()); err != nil {
		return Ticket{}, err
	}
	final, err := s.runBefore(ctx, q, a, apps.EventUpdate, &cur, next)
	if err != nil {
		return Ticket{}, err
	}
	if final, err = s.checkForm(ctx, q, a, apps.EventUpdate, &cur, final); err != nil {
		return Ticket{}, err
	}
	if err := writeChanges(ctx, q, cur, final); err != nil {
		return Ticket{}, err
	}
	out, err := Load(ctx, q, id)
	if err != nil {
		return Ticket{}, err
	}
	return out, s.runAfter(ctx, q, a, cause, apps.EventUpdate, &cur, out)
}

// Transition moves a ticket to another status, if its lifecycle allows the move and its guard agrees.
func (s *Service) Transition(ctx context.Context, q *dbgen.Queries, a Actor, cause ruleruns.Cause, id uuid.UUID, to string) (Ticket, error) {
	if err := s.mayUpdate(ctx, q, a, id, "moving"); err != nil {
		return Ticket{}, err
	}
	// Lock the row so two concurrent transitions can't both pass the check against the same state.
	t, err := q.GetTaskForUpdate(ctx, id)
	if err != nil {
		return Ticket{}, err
	}
	if err := q.MarkSaving(ctx, id); err != nil {
		return Ticket{}, err
	}
	// Which lifecycle applies depends on the ticket's type, so it is known only once the ticket is loaded.
	lc := s.Apps.Lifecycle(t.Type)
	if !lc.HasState(to) {
		return Ticket{}, Invalid("unknown status")
	}
	if !lc.CanTransition(t.Status, to) {
		return Ticket{}, Conflict("cannot move a " + lc.Label(t.Status) + " ticket to " + lc.Label(to))
	}
	// Only an app acting in a process (a workflow) makes an automatic move: a person asking for one is refused.
	if lc.Automatic(t.Status, to) && a.OnBehalfOf == uuid.Nil {
		return Ticket{}, Conflict(lc.Label(t.Status) + " to " + lc.Label(to) + " is made by the process, not by hand")
	}
	cur, err := Load(ctx, q, id)
	if err != nil {
		return Ticket{}, err
	}
	// The lifecycle's guard for this move, evaluated against the ticket as it would be after it. Anything other
	// than a definite yes blocks the move.
	if lc.Guarded(t.Status, to) {
		user, err := q.GetUser(ctx, a.ID)
		if err != nil {
			return Ticket{}, err
		}
		ok, message, err := lc.Check(ctx, t.Status, to, GuardInput(q, cur, to, lc, user))
		if err != nil {
			slog.Error("move guard", "ticket", cur.Number, "from", t.Status, "to", to, "err", err)
			return Ticket{}, Invalid(CannotCheck)
		}
		if !ok {
			return Ticket{}, Invalid(message)
		}
	}
	// Rules see the ticket as it would be after the move, exactly as a guard does.
	category, _ := lc.Category(to)
	next := cur
	next.Status, next.StateCategory = to, string(category)
	final, err := s.runBefore(ctx, q, a, apps.EventUpdate, &cur, next)
	if err != nil {
		return Ticket{}, err
	}
	if final, err = s.checkForm(ctx, q, a, apps.EventUpdate, &cur, final); err != nil {
		return Ticket{}, err
	}
	if _, err = q.UpdateTaskStatus(ctx, dbgen.UpdateTaskStatusParams{ID: id, Status: to, StateCategory: string(category)}); err != nil {
		return Ticket{}, err
	}
	if err := writeChanges(ctx, q, cur, final); err != nil {
		return Ticket{}, err
	}
	out, err := Load(ctx, q, id)
	if err != nil {
		return Ticket{}, err
	}
	return out, s.runAfter(ctx, q, a, cause, apps.EventUpdate, &cur, out)
}

// mayUpdate refuses an actor who may not work records of this one's type, with a reason, before the row is locked
// (which, under RLS, would only say the record is not there). A record the actor cannot see is ErrNoRows: a 404.
func (s *Service) mayUpdate(ctx context.Context, q *dbgen.Queries, a Actor, id uuid.UUID, doing string) error {
	t, err := q.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if !a.CanRecord(authz.Update, t.Type) {
		return needs(doing+" "+t.Type+" records", t.Type, authz.Update)
	}
	return nil
}

// createExtension writes the type's own row, for the types that have one, in the save's transaction.
func createExtension(ctx context.Context, q *dbgen.Queries, t dbgen.Task, catalogItem string) error {
	switch t.Type {
	case "incident":
		return q.CreateIncident(ctx, t.ID)
	case "change":
		return q.CreateChange(ctx, t.ID)
	case "request":
		return q.CreateRequest(ctx, dbgen.CreateRequestParams{TaskID: t.ID, CatalogItem: catalogItem})
	}
	return nil
}

// Counter runs lookups in q's transaction, so they count as the person (or app) acting, under RLS.
func Counter(q *dbgen.Queries) func(context.Context, string, []expr.Filter) (int64, error) {
	return func(ctx context.Context, target string, filters []expr.Filter) (int64, error) {
		raw, err := json.Marshal(filters)
		if err != nil {
			return 0, err
		}
		return q.LookupCount(ctx, dbgen.LookupCountParams{Target: target, Filters: raw})
	}
}

// GuardInput is what a transition guard sees: the ticket as it would be after the move, as it is now, who is acting,
// and (through q) the lookups its transition declares.
func GuardInput(q *dbgen.Queries, cur Ticket, to string, lc *apps.Lifecycle, u dbgen.User) expr.Input {
	next := cur
	next.Status = to
	if c, ok := lc.Category(to); ok {
		next.StateCategory = string(c)
	}
	return expr.Input{
		Record: next.ExprRecord(), Old: cur.ExprRecord(), Now: time.Now(), Count: Counter(q),
		User: &expr.User{ID: u.ID.String(), Name: u.Name, Email: u.Email, Role: u.Role},
	}
}

// ---- rules around a save ----

// ruleInput is what rules see: the ticket as it would be (record), as it is (old; empty on create), and who is acting.
func (s *Service) ruleInput(ctx context.Context, q *dbgen.Queries, a Actor, event string, old *Ticket, next Ticket) (expr.Input, error) {
	u, err := q.GetUser(ctx, a.ID)
	if err != nil {
		return expr.Input{}, err
	}
	in := expr.Input{
		Record: next.ExprRecord(), Event: event, Now: time.Now(), Count: Counter(q),
		User: &expr.User{ID: u.ID.String(), Name: u.Name, Email: u.Email, Role: u.Role},
	}
	if old != nil {
		in.Old = old.ExprRecord()
	}
	return in, nil
}

// ruleError turns what the engine reports into what the person sees, and records it for the administrator. A rule
// saying no is an expected failure and carries the rule's own message; a rule that could not run is logged and
// reported plainly. The audit line is written from another connection, because this save is about to roll back.
func (s *Service) ruleError(ctx context.Context, err error, timing, event string, id *uuid.UUID, ref string) error {
	var ab *rules.Abort
	var br *rules.Broken
	switch {
	case errors.As(err, &ab):
		ruleruns.Record(ctx, s.Pool, ruleruns.Run{App: ab.App, Rule: ab.Rule, Timing: timing, Event: event, RecordID: id, Outcome: ruleruns.Aborted, Detail: ab.Message})
		return Invalid(ab.Message)
	case errors.As(err, &br):
		slog.Error("rule could not run", "rule", br.Rule, "ticket", ref, "err", br.Err)
		ruleruns.Record(ctx, s.Pool, ruleruns.Run{App: br.App, Rule: br.Rule, Timing: timing, Event: event, RecordID: id, Outcome: ruleruns.Failed, Detail: br.Err.Error()})
		return Invalid(ruleBroken)
	}
	return err
}

func idOf(t *Ticket) *uuid.UUID {
	if t == nil {
		return nil
	}
	return &t.ID
}

// runBefore runs the before rules for a save. cur is nil on create. next is the ticket as the request would leave
// it; the result is what to write instead, with the rules' changes checked as strictly as a request's would be.
func (s *Service) runBefore(ctx context.Context, q *dbgen.Queries, a Actor, event string, cur *Ticket, next Ticket) (Ticket, error) {
	list := s.Apps.Rules(next.Type, apps.TimingBefore, event)
	if len(list) == 0 {
		return next, nil
	}
	in, err := s.ruleInput(ctx, q, a, event, cur, next)
	if err != nil {
		return next, err
	}
	res, err := rules.Before(ctx, list, in)
	if err != nil {
		return next, s.ruleError(ctx, err, apps.TimingBefore, event, idOf(cur), next.Reference)
	}
	out, err := FromRecord(ctx, q, next, res)
	if err != nil {
		return next, err
	}
	if err := validateRuleChanges(ctx, q, next, out); err != nil {
		return next, err
	}
	return out, nil
}

// FromRecord is the ticket with the fields rules may set taken from the record the rules left. Values a rule set are
// range-checked here, and an assignment group named by a rule is looked up; whether they are allowed for this ticket
// is validateRuleChanges.
func FromRecord(ctx context.Context, q *dbgen.Queries, base Ticket, res *expr.Record) (Ticket, error) {
	out := base
	// Rules name a group, not its id: an app cannot know the ids of the install it lands in.
	if !sameString(res.AssignmentGroup, base.AssignmentGroup) {
		out.AssignmentGroupID, out.AssignmentGroup = nil, nil
		if res.AssignmentGroup != nil && *res.AssignmentGroup != "" {
			g, err := q.GetGroupByName(ctx, *res.AssignmentGroup)
			if errors.Is(err, pgx.ErrNoRows) {
				return base, Invalid(fmt.Sprintf("a rule set the assignment group to %q, and there is no such group", *res.AssignmentGroup))
			}
			if err != nil {
				return base, err
			}
			out.AssignmentGroupID, out.AssignmentGroup = &g.ID, &g.Name
		}
	}
	out.Title, out.Description, out.Resolution = res.Title, res.Description, res.Resolution
	out.Fields = cloneFields(res.Fields)
	if base.Change != nil {
		out.Change = base.Change.clone()
		out.Change.Risk, out.Change.Impact = res.Risk, res.Impact
	}
	out.AssigneeID = nil
	if res.AssigneeID != nil {
		id, err := uuid.Parse(*res.AssigneeID)
		if err != nil {
			return base, Invalid("a rule set an assignee that is not a user")
		}
		out.AssigneeID = &id
	}
	if res.Priority < 1 || res.Priority > 4 {
		return base, Invalid("a rule set the priority out of range: it must be 1 (critical) to 4 (low)")
	}
	out.Priority = int16(res.Priority)
	return out, nil
}

// validateRuleChanges applies the checks a request goes through to whatever the rules changed.
func validateRuleChanges(ctx context.Context, q *dbgen.Queries, before, after Ticket) error {
	wrap := func(err error) error {
		var e *Error
		if errors.As(err, &e) {
			return Invalid("a rule made a change that is not allowed: " + e.Msg)
		}
		return err
	}
	if after.Title != before.Title {
		if err := validTitle(after.Title); err != nil {
			return wrap(err)
		}
	}
	if after.Description != before.Description && len(after.Description) > MaxDescription {
		return Invalid("a rule made the description too long")
	}
	if after.Change != nil && !sameChange(before.Change, after.Change) {
		if err := validChange(after.Change); err != nil {
			return wrap(err)
		}
	}
	if after.Resolution != before.Resolution {
		if after.Type != "incident" {
			return Invalid("a rule set resolution notes on a ticket that has none")
		}
		if len(after.Resolution) > MaxResolution {
			return Invalid("a rule made the resolution notes too long")
		}
	}
	if after.AssigneeID != nil && (before.AssigneeID == nil || *before.AssigneeID != *after.AssigneeID) {
		u, err := q.GetUser(ctx, *after.AssigneeID)
		if err != nil {
			return Invalid("a rule set an assignee who does not exist")
		}
		if ok, err := assignable(ctx, q, u.ID, after.Type); err != nil {
			return err
		} else if !ok {
			return Invalid("a rule set an assignee who does not work tickets of this type")
		}
	}
	return nil
}

// changes is what must be written to turn cur into final: nothing set means nothing to write.
func changes(cur, final Ticket) (args dbgen.UpdateTaskParams, any bool) {
	if final.Title != cur.Title {
		args.Title, any = pgtype.Text{String: strings.TrimSpace(final.Title), Valid: true}, true
	}
	if final.Description != cur.Description {
		args.Description, any = pgtype.Text{String: final.Description, Valid: true}, true
	}
	if final.Priority != cur.Priority {
		args.Priority, any = pgtype.Int2{Int16: final.Priority, Valid: true}, true
	}
	if !sameUUID(cur.AssigneeID, final.AssigneeID) {
		args.SetAssignee, any = true, true
		if final.AssigneeID != nil {
			args.AssigneeID.UUID, args.AssigneeID.Valid = *final.AssigneeID, true
		}
	}
	if !sameUUID(cur.AssignmentGroupID, final.AssignmentGroupID) {
		args.SetAssignmentGroup, any = true, true
		if final.AssignmentGroupID != nil {
			args.AssignmentGroupID.UUID, args.AssignmentGroupID.Valid = *final.AssignmentGroupID, true
		}
	}
	if !sameFields(cur.Fields, final.Fields) {
		args.Fields, any = EncodeFields(final.Fields), true
	}
	args.ID = cur.ID
	return args, any
}

func sameUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func nullUUID(id *uuid.UUID) uuid.NullUUID {
	if id == nil {
		return uuid.NullUUID{}
	}
	return uuid.NullUUID{UUID: *id, Valid: true}
}

func sameString(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// reconcileAssignment keeps the assignee inside the assignment group. Moving a ticket to another group lets go of an
// assignee who is not in it, the way handing work to another team does; naming an assignee outside the group is an
// error, whether a person or a rule asks for it.
func reconcileAssignment(ctx context.Context, q *dbgen.Queries, cur, final Ticket) (Ticket, error) {
	if final.AssigneeID == nil || final.AssignmentGroupID == nil {
		return final, nil
	}
	assigneeChanged := !sameUUID(cur.AssigneeID, final.AssigneeID)
	if !assigneeChanged && sameUUID(cur.AssignmentGroupID, final.AssignmentGroupID) {
		return final, nil
	}
	member, err := q.IsGroupMember(ctx, dbgen.IsGroupMemberParams{GroupID: *final.AssignmentGroupID, UserID: *final.AssigneeID})
	if err != nil || member {
		return final, err
	}
	if assigneeChanged {
		return final, Invalid("the assignee is not a member of the assignment group")
	}
	final.AssigneeID, final.Assignee = nil, nil
	return final, nil
}

// writeChanges saves final over cur, including an incident's resolution.
func writeChanges(ctx context.Context, q *dbgen.Queries, cur, final Ticket) error {
	final, err := reconcileAssignment(ctx, q, cur, final)
	if err != nil {
		return err
	}
	if args, any := changes(cur, final); any {
		if _, err := q.UpdateTask(ctx, args); err != nil {
			return err
		}
	}
	if !sameCIs(cur.CIs, final.CIs) {
		if err := relinkCIs(ctx, q, cur.ID, cur.CIs, final.CIs); err != nil {
			return err
		}
	}
	if !sameUUID(final.ProblemID, cur.ProblemID) {
		if err := q.SetIncidentProblem(ctx, dbgen.SetIncidentProblemParams{TaskID: cur.ID, ProblemID: nullUUID(final.ProblemID)}); err != nil {
			return err
		}
	}
	if final.Resolution != cur.Resolution {
		if err := q.UpdateIncident(ctx, dbgen.UpdateIncidentParams{TaskID: cur.ID, Resolution: final.Resolution}); err != nil {
			return err
		}
	}
	if c := final.Change; c != nil && !sameChange(cur.Change, c) {
		return q.UpdateChange(ctx, dbgen.UpdateChangeParams{TaskID: cur.ID, ChangeType: c.ChangeType, Risk: c.Risk, Impact: c.Impact,
			PlannedStart: timestamptz(c.PlannedStart), PlannedEnd: timestamptz(c.PlannedEnd),
			ImplementationPlan: c.ImplementationPlan, BackoutPlan: c.BackoutPlan, TestPlan: c.TestPlan})
	}
	return nil
}

// resolveCIs reads the items to link a record to, as the person saving may see them. Each must exist; one named
// twice counts once.
func resolveCIs(ctx context.Context, q *dbgen.Queries, a Actor, ids []uuid.UUID) ([]CI, error) {
	if !a.Can(authz.CMDBRead) {
		return nil, Forbidden(fmt.Sprintf("linking configuration items needs the %s permission", authz.CMDBRead))
	}
	unique := slices.Compact(slices.SortedFunc(slices.Values(ids), func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) }))
	if len(unique) > MaxCIs {
		return nil, Invalid(fmt.Sprintf("a record can be linked to at most %d items", MaxCIs))
	}
	if len(unique) == 0 {
		return nil, nil
	}
	rows, err := q.GetCIsByID(ctx, unique)
	if err != nil {
		return nil, err
	}
	if len(rows) != len(unique) {
		return nil, Invalid("there is no configuration item with that id")
	}
	out := make([]CI, len(rows))
	for i, r := range rows {
		out[i] = ciFrom(r)
	}
	return out, nil
}

// relinkCIs links the items in next that cur lacks and unlinks those it no longer has.
func relinkCIs(ctx context.Context, q *dbgen.Queries, id uuid.UUID, cur, next []CI) error {
	had := map[uuid.UUID]bool{}
	for _, c := range cur {
		had[c.ID] = true
	}
	for _, c := range next {
		if had[c.ID] {
			delete(had, c.ID)
			continue
		}
		if err := q.LinkTaskCI(ctx, dbgen.LinkTaskCIParams{TaskID: id, CiID: c.ID}); err != nil {
			return err
		}
	}
	for ci := range had {
		if _, err := q.UnlinkTaskCI(ctx, dbgen.UnlinkTaskCIParams{TaskID: id, CiID: ci}); err != nil {
			return err
		}
	}
	return nil
}

func timestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// withRuleFields is base with the fields rules may set taken from the other ticket.
func withRuleFields(base, from Ticket) Ticket {
	base.Title, base.Description, base.Priority = strings.TrimSpace(from.Title), from.Description, from.Priority
	base.AssigneeID, base.Resolution, base.Change = from.AssigneeID, from.Resolution, from.Change.clone()
	base.AssignmentGroupID, base.AssignmentGroup = from.AssignmentGroupID, from.AssignmentGroup
	base.Fields, base.ProblemID = from.Fields, from.ProblemID
	return base
}

// problemFor checks that a record of recordType may be linked to the problem id: only incidents are, and the problem
// must be one the person saving can see.
func problemFor(ctx context.Context, q *dbgen.Queries, recordType string, id uuid.UUID) (*uuid.UUID, error) {
	if recordType != "incident" {
		return nil, Invalid("only an incident is linked to a problem")
	}
	p, err := q.GetTask(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && p.Type != "problem") {
		return nil, Invalid("there is no problem with that id")
	} else if err != nil {
		return nil, err
	}
	return &p.ID, nil
}

// runAfter runs the after rules against the saved ticket, then queues the async ones. An abort undoes the whole
// save (the transaction rolls back); the comments the rules ask for are written as the person acting.
func (s *Service) runAfter(ctx context.Context, q *dbgen.Queries, a Actor, cause ruleruns.Cause, event string, cur *Ticket, saved Ticket) error {
	// The clocks move with the record as saved, before the after rules: they are part of the save, not a reaction to it.
	if err := s.trackSLAs(ctx, q, a, event, cur, saved); err != nil {
		return err
	}
	if list := s.Apps.Rules(saved.Type, apps.TimingAfter, event); len(list) > 0 {
		if err := s.runAfterRules(ctx, q, a, cause, event, cur, saved, list); err != nil {
			return err
		}
	}
	// Last, so that what is queued is only what the whole save, including every after rule, agreed to.
	return s.queueAsync(ctx, q, a, cause, event, cur, saved)
}

func (s *Service) runAfterRules(ctx context.Context, q *dbgen.Queries, a Actor, cause ruleruns.Cause, event string, cur *Ticket, saved Ticket, list []*apps.Rule) error {
	in, err := s.ruleInput(ctx, q, a, event, cur, saved)
	if err != nil {
		return err
	}
	fx, err := rules.After(ctx, list, in)
	if err != nil {
		return s.ruleError(ctx, err, apps.TimingAfter, event, &saved.ID, saved.Reference)
	}
	// Each effect is carried out as its rule says: as the person saving, or (run_as: app) as the rule's app.
	who := func(app, rule string, fn func(Actor) error) error {
		if r := s.Apps.Rule(app, rule); r != nil && r.RunAs == apps.RunAsApp {
			return asApp(ctx, q, a, app, fn)
		}
		return fn(a)
	}
	for _, c := range fx.Comments {
		body := strings.TrimSpace(c.Body)
		if body == "" || len(body) > MaxComment {
			return Invalid(fmt.Sprintf("the rule %s wrote a comment that is empty or too long", c.Rule))
		}
		if err := who(c.App, c.Rule, func(by Actor) error {
			_, err := q.CreateComment(ctx, dbgen.CreateCommentParams{TaskID: saved.ID, AuthorID: by.ID, Body: body, Internal: c.Internal})
			return err
		}); err != nil {
			return err
		}
	}
	for _, st := range fx.Starts {
		if err := who(st.App, st.Rule, func(by Actor) error {
			return s.startFromRule(ctx, q, by, cause, apps.TimingAfter, saved, st)
		}); err != nil {
			return err
		}
	}
	for _, n := range fx.Notifies {
		if err := who(n.App, n.Rule, func(Actor) error {
			return s.notifyFromRule(ctx, q, apps.TimingAfter, saved, n)
		}); err != nil {
			return err
		}
	}
	return nil
}

// asApp runs fn, inside the save's transaction, as an app's principal acting for the person saving, then switches
// the transaction back to whoever it was. RLS checks the app's permissions for what fn does.
func asApp(ctx context.Context, q *dbgen.Queries, a Actor, app string, fn func(Actor) error) error {
	principal, err := q.AppPrincipal(ctx, app)
	if err != nil {
		return err
	}
	if err := q.BeginAs(ctx, dbgen.BeginAsParams{Principal: principal, OnBehalfOf: a.Person()}); err != nil {
		return err
	}
	by, err := ActorIn(ctx, q, principal)
	if err == nil {
		err = fn(by)
	}
	var back error
	if a.OnBehalfOf != uuid.Nil {
		back = q.BeginAs(ctx, dbgen.BeginAsParams{Principal: a.ID, OnBehalfOf: a.OnBehalfOf})
	} else {
		back = q.BeginAsUser(ctx, a.ID)
	}
	if err != nil {
		return err
	}
	return back
}

// queueAsync fires the async rules the change sets off. They are queued in this transaction, so a save that rolls
// back queues nothing, and they run later in a worker, after the save has committed. What an async rule's own save
// sets off is limited by its cause (see ruleruns.Cause).
func (s *Service) queueAsync(ctx context.Context, q *dbgen.Queries, a Actor, cause ruleruns.Cause, event string, cur *Ticket, saved Ticket) error {
	list := s.Apps.Rules(saved.Type, apps.TimingAsync, event)
	if len(list) == 0 {
		return nil
	}
	in, err := s.ruleInput(ctx, q, a, event, cur, saved)
	if err != nil {
		return err
	}
	fired, failed := rules.Fire(ctx, list, in)
	for _, br := range failed {
		// An async rule never blocks a save, but its failure must not be silent.
		slog.Error("async rule could not be evaluated", "rule", br.Rule, "ticket", saved.Reference, "err", br.Err)
		ruleruns.Record(ctx, s.Pool, ruleruns.Run{App: br.App, Rule: br.Rule, Timing: apps.TimingAsync, Event: event, RecordID: &saved.ID, Outcome: ruleruns.Failed, Detail: br.Err.Error()})
	}
	return ruleruns.Enqueue(ctx, q, s.Pool, fired, event, saved.ID, a.ID, cause)
}
