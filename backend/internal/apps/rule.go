package apps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"cel.dev/cel-go/cel"
	"gopkg.in/yaml.v3"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

// Rule timings and events. Delete events arrive later.
const (
	TimingBefore = "before" // inside the save, before the write: can abort or set fields
	TimingAfter  = "after"  // inside the same transaction, after the write: can abort or add a comment
	TimingAsync  = "async"  // after the save commits, in a worker: can add a comment, and cannot stop anything

	EventCreate = "create"
	EventUpdate = "update"

	RunAsApp = "app" // run_as: an after rule acting as its app's principal

	OnErrorBlock = "block" // a rule that cannot be evaluated stops the save (the default: fail closed)
	OnErrorSkip  = "skip"  // ...or is skipped, for rules that are nice to have

	maxRuleActions      = 20  // before and after rules run inside a save, so they stay small
	maxAsyncRuleActions = 100 // an async rule waits for nobody
	defaultOrder        = 100
	maxOrder            = 1000
)

var ruleNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// SettableFields are the fields rules and workflows may set, and the type each takes.
var SettableFields = map[string]string{
	"title": "text", "description": "text", "priority": "number", "assignee_id": "text", "resolution": "text",
	"assignment_group": "text", "risk": "text", "impact": "text",
}

// FieldOwners are the settable fields that live in one record type's own table, so only rules and workflows for
// that type may set them.
var FieldOwners = map[string]string{"resolution": "incident", "risk": "change", "impact": "change"}

// FieldValues are the values an enumerated field accepts. The database checks the same lists.
var FieldValues = map[string][]string{"risk": Levels, "impact": Levels, "change_type": ChangeTypes}

// Levels rate a change's risk and impact; empty means not assessed yet.
var Levels = []string{"", "low", "medium", "high"}

// ChangeTypes are the kinds of change: standard ones are pre-approved, normal ones go to the change advisory board,
// emergency ones to a smaller board, fast.
var ChangeTypes = []string{"standard", "normal", "emergency"}

// ownerProblem says why a definition for recordType may not set field, or "".
func ownerProblem(field, recordType, what string) string {
	if owner := FieldOwners[field]; owner != "" && recordType != owner {
		return fmt.Sprintf("%s belongs to %s records, so %s that sets it must be for record_type %s", field, owner, what, owner)
	}
	return ""
}

// Value is written in YAML as a literal (`5`, `"text"`, `true`) or, with a leading `=`, as an expression
// (`=record.priority + 1`). Exactly one of Expr and Literal is set.
type Value struct {
	Expr    string `json:"expr,omitempty"`
	Literal any    `json:"literal,omitempty"`

	program *expr.Program
	err     error
}

func (v *Value) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a value is text, a whole number, true or false, or an =expression", n.Line)
	}
	switch n.Tag {
	case "!!int":
		i, err := strconv.ParseInt(n.Value, 0, 64)
		if err != nil {
			return fmt.Errorf("line %d: %q is not a whole number", n.Line, n.Value)
		}
		v.Literal = i
	case "!!bool":
		v.Literal = n.Value == "true"
	case "!!str":
		if rest, ok := strings.CutPrefix(n.Value, "="); ok {
			v.Expr = strings.TrimSpace(rest)
		} else {
			v.Literal = n.Value
		}
	default:
		return fmt.Errorf("line %d: a value is text, a whole number, true or false, or an =expression", n.Line)
	}
	return nil
}

// UnmarshalJSON reads a value back from the stored definition. Plain decoding would turn the literal 1 into the
// float 1.0, which is not a whole number as far as validation and the engine are concerned.
func (v *Value) UnmarshalJSON(data []byte) error {
	var raw struct {
		Expr    string `json:"expr"`
		Literal any    `json:"literal"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	v.Expr, v.Literal = raw.Expr, raw.Literal
	if n, ok := raw.Literal.(json.Number); ok {
		i, err := n.Int64()
		if err != nil {
			return fmt.Errorf("%s is not a whole number", n)
		}
		v.Literal = i
	}
	return nil
}

// Action is one step of a rule.
type Action struct {
	Action   string `json:"action"`
	When     string `json:"when,omitempty"`    // this action runs only if this is true
	Field    string `json:"field,omitempty"`   // set: which field
	Value    *Value `json:"value,omitempty"`   // set: the new value
	Message  *Value `json:"message,omitempty"` // abort: what the person sees
	Body     *Value `json:"body,omitempty"`    // comment.add: the text
	Internal bool   `json:"internal,omitempty"`
	Workflow string `json:"workflow,omitempty"` // workflow.start: the name of a workflow in the same app
	Template string `json:"template,omitempty"` // notify: the name of a notification template in the same app
	// To is who notify tells: assignee, requester, assignment_group (its active members), or an =expression giving
	// a list of user ids. The engine still drops the person who made the change, anyone deactivated, and anyone
	// who cannot see the record.
	To []string `json:"to,omitempty"`

	when    *expr.Program
	whenErr error
	to      []*expr.Program // per To entry: the compiled =expression, nil for a named recipient
	toErr   []error
}

// Rule is "on this event, if this is true, do these things", at a point in the save.
type Rule struct {
	Name       string   `json:"name"`
	RecordType string   `json:"record_type"`
	Timing     string   `json:"timing"`
	On         []string `json:"on"`
	Order      int      `json:"order"`
	When       string   `json:"when,omitempty"`
	OnError    string   `json:"on_error"`
	// RunAs "app" makes an after rule act as its app's principal (with the permissions its manifest declares) instead
	// of as the person saving. Async rules always run as the app; before rules always as the person.
	RunAs string `json:"run_as,omitempty"`
	// Lookups are counted before the rule's expressions run, which read them as lookups.name.
	Lookups Lookups  `json:"lookups,omitempty"`
	Run     []Action `json:"run"`
	// Fields are the extra fields of the app's form the rule reads or sets, with their types when it was installed.
	Fields expr.FieldTypes `json:"fields,omitempty"`

	// App and Version say where the rule came from. They are set at runtime and are not part of the definition.
	App     string `json:"-"`
	Version string `json:"-"`

	when    *expr.Program
	whenErr error
}

// ---- YAML ----

type ruleFile struct {
	Kind       string       `yaml:"kind"`
	Name       string       `yaml:"name"`
	RecordType string       `yaml:"record_type"`
	Timing     string       `yaml:"timing"`
	On         []string     `yaml:"on"`
	Order      *int         `yaml:"order"`
	When       string       `yaml:"when"`
	OnError    string       `yaml:"on_error"`
	RunAs      string       `yaml:"run_as"`
	Lookups    Lookups      `yaml:"lookups"`
	Run        []actionFile `yaml:"run"`
}

type actionFile struct {
	Action   string   `yaml:"action"`
	When     string   `yaml:"when"`
	Field    string   `yaml:"field"`
	Value    *Value   `yaml:"value"`
	Message  *Value   `yaml:"message"`
	Body     *Value   `yaml:"body"`
	Internal bool     `yaml:"internal"`
	Workflow string   `yaml:"workflow"`
	Template string   `yaml:"template"`
	To       []string `yaml:"to"`
}

func parseRule(file string, data []byte, forms formSet, p *Problems) *Rule {
	var f ruleFile
	if err := decodeStrict(data, &f); err != nil {
		p.add(file, "%v", err)
		return nil
	}
	if f.Kind != "rule" {
		p.add(file, "kind must be rule in this folder, got %q", f.Kind)
		return nil
	}
	r := &Rule{Name: f.Name, RecordType: f.RecordType, Timing: f.Timing, On: f.On, Order: defaultOrder, When: f.When, OnError: f.OnError, RunAs: f.RunAs, Lookups: f.Lookups,
		Fields: forms.fieldsUsed(f.RecordType, data)}
	if f.Order != nil {
		r.Order = *f.Order
	}
	if r.OnError == "" {
		r.OnError = OnErrorBlock
	}
	for _, a := range f.Run {
		r.Run = append(r.Run, Action{Action: a.Action, When: a.When, Field: a.Field, Value: a.Value, Message: a.Message, Body: a.Body, Internal: a.Internal, Workflow: a.Workflow, Template: a.Template, To: a.To})
	}
	before := len(*p)
	r.validate(file, p)
	if len(*p) > before {
		return nil
	}
	return r
}

// ---- validation ----

// validate checks the rule's structure and compiles every expression in it, reporting anything wrong. It is what an
// install runs.
func (r *Rule) validate(file string, p *Problems) {
	r.validateStructure(file, p)
	r.compile()
	env, envErr := expr.RuleFor(r.Fields)
	for _, msg := range r.Lookups.validate(env, envErr) {
		p.add(file, "%s", msg)
	}
	// Every expression may read only the lookups the rule declares.
	declared := func(at string, prog *expr.Program) {
		if names := r.Lookups.undeclared(prog); len(names) > 0 {
			p.add(file, "%s: %s", at, undeclaredProblem(names))
		}
	}
	if r.whenErr != nil {
		p.add(file, "when: %v", r.whenErr)
	}
	declared("when", r.when)
	for i := range r.Run {
		a := &r.Run[i]
		if a.whenErr != nil {
			p.add(file, "run[%d]: when: %v", i, a.whenErr)
		}
		declared(fmt.Sprintf("run[%d]: when", i), a.when)
		for j, err := range a.toErr {
			if err != nil {
				p.add(file, "run[%d]: to[%d]: %v", i, j, err)
			}
			declared(fmt.Sprintf("run[%d]: to[%d]", i, j), a.to[j])
		}
		for _, v := range []struct {
			name string
			v    *Value
		}{{"value", a.Value}, {"message", a.Message}, {"body", a.Body}} {
			if v.v != nil && v.v.err != nil {
				p.add(file, "run[%d]: %s: %v", i, v.name, v.v.err)
			}
			if v.v != nil {
				declared(fmt.Sprintf("run[%d]: %s", i, v.name), v.v.program)
			}
		}
	}
}

func (r *Rule) validateStructure(file string, p *Problems) {
	if !ruleNameRE.MatchString(r.Name) {
		p.add(file, "name %q must be 2-63 characters: lowercase letters, digits and hyphens, starting with a letter", r.Name)
	}
	if !validTypeName(r.RecordType) {
		p.add(file, "record_type %q is not a record type's name", r.RecordType)
	}
	switch r.Timing {
	case TimingBefore, TimingAfter, TimingAsync:
	default:
		p.add(file, "timing must be before, after or async, got %q", r.Timing)
	}
	if len(r.On) == 0 {
		p.add(file, "on is required: list the events this rule runs for (create, update)")
	}
	seen := map[string]bool{}
	for _, e := range r.On {
		switch {
		case e == "delete":
			p.add(file, "on: delete is not supported yet")
		case e != EventCreate && e != EventUpdate:
			p.add(file, "on: %q is not an event (create or update)", e)
		case seen[e]:
			p.add(file, "on: %q is listed twice", e)
		}
		seen[e] = true
	}
	if r.Order < 0 || r.Order > maxOrder {
		p.add(file, "order must be from 0 to %d", maxOrder)
	}
	if r.OnError != OnErrorBlock && r.OnError != OnErrorSkip {
		p.add(file, "on_error must be block or skip, got %q", r.OnError)
	}
	switch {
	case r.RunAs != "" && r.RunAs != RunAsApp:
		p.add(file, "run_as must be app, got %q", r.RunAs)
	case r.RunAs == RunAsApp && r.Timing == TimingBefore:
		p.add(file, "run_as: app is not supported for a before rule yet: it changes the record the person is saving")
	case r.RunAs == RunAsApp && r.Timing == TimingAsync:
		p.add(file, "run_as: app is how every async rule runs already, so leave it out")
	}
	if r.Timing == TimingAsync && r.OnError == OnErrorSkip {
		p.add(file, "on_error: skip does not apply to an async rule: nothing waits for it, so a failing run is retried and then dead-lettered")
	}
	limit := maxRuleActions
	if r.Timing == TimingAsync {
		limit = maxAsyncRuleActions
	}
	switch n := len(r.Run); {
	case n == 0:
		p.add(file, "a rule needs at least one action in run")
	case n > limit:
		p.add(file, "a %s rule may have at most %d actions, this has %d", r.Timing, limit, n)
	}
	for i := range r.Run {
		for _, msg := range r.Run[i].structureProblems(r, i) {
			p.add(file, "run[%d]: %s", i, msg)
		}
	}
}

// structureProblems lists what is wrong with an action's shape: its fields, and whether it is allowed at the rule's
// timing.
func (a *Action) structureProblems(r *Rule, _ int) (out []string) {
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	extra := func(name string, present bool) {
		if present {
			add("%s does not take %s", a.Action, name)
		}
	}
	switch a.Action {
	case "abort":
		if r.Timing == TimingAsync {
			add("abort is not allowed in an async rule: the save has already happened, so there is nothing to stop")
		}
		if a.Message == nil {
			add("abort needs a message: what the person sees")
		}
		extra("field", a.Field != "")
		extra("value", a.Value != nil)
		extra("body", a.Body != nil)
	case "set", "record.update":
		switch {
		case a.Action == "set" && r.Timing != TimingBefore:
			add("set changes the record being saved, so it is only allowed in a before rule (an async rule uses record.update)")
		case a.Action == "record.update" && r.Timing != TimingAsync:
			add("record.update saves a change of its own, so it is only allowed in an async rule (a before rule uses set)")
		}
		extra("message", a.Message != nil)
		extra("body", a.Body != nil)
		kind, ok := SettableFields[a.Field]
		extra, isExtra := strings.CutPrefix(a.Field, "fields.")
		switch {
		case a.Field == "":
			add("%s needs a field", a.Action)
		case isExtra:
			kind, ok = r.Fields[extra], r.Fields[extra] != ""
			if !ok {
				add("%s: this app's form for %s records has no field %q", a.Action, r.RecordType, extra)
			}
		case !ok:
			add("%s cannot change %q (it can change: %s)", a.Action, a.Field, settableList())
		case ownerProblem(a.Field, r.RecordType, "a rule") != "":
			add("%s", ownerProblem(a.Field, r.RecordType, "a rule"))
		}
		if a.Value == nil {
			add("%s needs a value", a.Action)
		} else if ok && a.Value.Expr == "" {
			out = append(out, literalProblems(a.Field, kind, a.Value.Literal)...)
		}
	case "comment.add":
		if r.Timing == TimingBefore {
			add("comment.add is only allowed in an after or async rule, because the ticket must exist first")
		}
		if a.Body == nil {
			add("comment.add needs a body")
		}
		extra("field", a.Field != "")
		extra("value", a.Value != nil)
		extra("message", a.Message != nil)
	case "workflow.start":
		if r.Timing == TimingBefore {
			add("workflow.start is only allowed in an after or async rule, because the ticket must exist first")
		}
		switch {
		case a.Workflow == "":
			add("workflow.start needs workflow: the name of a workflow in this app")
		case !ruleNameRE.MatchString(a.Workflow):
			add("workflow %q is not a workflow name", a.Workflow)
		}
		extra("field", a.Field != "")
		extra("value", a.Value != nil)
		extra("message", a.Message != nil)
		extra("body", a.Body != nil)
	case "notify":
		if r.Timing == TimingBefore {
			add("notify is only allowed in an after or async rule, because the ticket must be saved first")
		}
		switch {
		case a.Template == "":
			add("notify needs template: the name of a notification template in this app")
		case !ruleNameRE.MatchString(a.Template):
			add("template %q is not a template name", a.Template)
		}
		if len(a.To) == 0 {
			add("notify needs to: who to tell (%s, or an =expression giving user ids)", strings.Join(Recipients, ", "))
		}
		for j, to := range a.To {
			if !strings.HasPrefix(to, "=") && !slices.Contains(Recipients, to) {
				add("to[%d]: %q is not a recipient (%s, or an =expression giving user ids)", j, to, strings.Join(Recipients, ", "))
			}
		}
		extra("field", a.Field != "")
		extra("value", a.Value != nil)
		extra("message", a.Message != nil)
		extra("body", a.Body != nil)
		extra("workflow", a.Workflow != "")
	case "":
		add("an action needs a name: abort, set, comment.add, record.update, workflow.start or notify")
	default:
		add("unknown action %q (available: abort, set, comment.add, record.update, workflow.start, notify)", a.Action)
	}
	if a.Action != "notify" {
		extra("template", a.Template != "")
		extra("to", len(a.To) > 0)
	}
	return out
}

func quoteAll(values []string) string {
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = strconv.Quote(v)
	}
	return strings.Join(q, ", ")
}

func settableList() string {
	names := make([]string, 0, len(SettableFields))
	for n := range SettableFields {
		names = append(names, n)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

func literalProblems(field, kind string, lit any) (out []string) {
	switch kind {
	case expr.FieldBool:
		if _, ok := lit.(bool); !ok {
			out = append(out, fmt.Sprintf("%s takes true or false", field))
		}
	case "number":
		n, ok := lit.(int64)
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s takes a whole number", field))
		case field == "priority" && (n < 1 || n > 4):
			out = append(out, "priority must be 1 (critical) to 4 (low)")
		}
	default:
		s, ok := lit.(string)
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s takes text", field))
		case field == "title" && (strings.TrimSpace(s) == "" || len(s) > 200):
			out = append(out, "title must be 1 to 200 characters")
		case FieldValues[field] != nil && !slices.Contains(FieldValues[field], s):
			out = append(out, fmt.Sprintf("%s must be one of %s", field, quoteAll(FieldValues[field])))
		case len(s) > 10000:
			out = append(out, fmt.Sprintf("%s is too long", field))
		}
	}
	return out
}

// compile compiles every expression in the rule. Anything that fails is remembered, so at run time the rule is
// treated as broken (blocked or skipped, per on_error) instead of being silently ignored.
func (r *Rule) compile() {
	env, err := expr.RuleFor(r.Fields)
	r.Lookups.compile(env, err)
	r.when, r.whenErr = nil, err
	if err == nil && r.When != "" {
		r.when, r.whenErr = env.Compile(r.When)
	}
	for i := range r.Run {
		a := &r.Run[i]
		a.when, a.whenErr = nil, err
		if err == nil && a.When != "" {
			a.when, a.whenErr = env.Compile(a.When)
		}
		want := expr.String
		if SettableFields[a.Field] == "number" {
			want = expr.Int
		}
		if name, ok := strings.CutPrefix(a.Field, "fields."); ok {
			want = expr.KindType(r.Fields[name])
		}
		a.Value.compile(env, err, want)
		a.Message.compile(env, err, expr.String)
		a.Body.compile(env, err, expr.String)
		a.to, a.toErr = make([]*expr.Program, len(a.To)), make([]error, len(a.To))
		for j, to := range a.To {
			if src, ok := strings.CutPrefix(to, "="); ok {
				a.toErr[j] = err
				if err == nil {
					a.to[j], a.toErr[j] = env.CompileAs(strings.TrimSpace(src), expr.StringList)
				}
			}
		}
	}
}

// locatedRule is a rule with where it sits in its file: "" for a rule definition, "on_breach: " for an SLA's breach.
type locatedRule struct {
	at   string
	rule *Rule
}

// actionRules lists the rules in a definition whose actions may name a template or a workflow of the app.
func (d Definition) actionRules() []locatedRule {
	switch {
	case d.Rule != nil:
		return []locatedRule{{"", d.Rule}}
	case d.SLA != nil:
		return d.SLA.actionRules()
	}
	return nil
}

// Recipients are the people a notify action can name without an expression.
var Recipients = []string{"assignee", "requester", "assignment_group"}

// Recipient returns the To entry's compiled expression (nil for a named recipient) and any reason it cannot be
// evaluated.
func (a *Action) Recipient(i int) (*expr.Program, error) { return a.to[i], a.toErr[i] }

func (v *Value) compile(env *expr.Env, envErr error, want *cel.Type) {
	if v == nil {
		return
	}
	v.program, v.err = nil, envErr
	if envErr == nil && v.Expr != "" {
		v.program, v.err = env.CompileAs(v.Expr, want)
	}
}

// Applies reports whether the rule is for this timing and event.
func (r *Rule) Applies(timing, event string) bool {
	return r.Timing == timing && slices.Contains(r.On, event)
}

// ---- what the engine needs to run a rule ----

// Condition returns the rule's compiled filter (nil when it has none) and any reason it cannot be evaluated.
func (r *Rule) Condition() (*expr.Program, error) { return r.when, r.whenErr }

// Condition returns the action's compiled filter (nil when it has none) and any reason it cannot be evaluated.
func (a *Action) Condition() (*expr.Program, error) { return a.when, a.whenErr }

// Compiled returns the value's expression program (nil for a literal) and any reason it cannot be evaluated.
func (v *Value) Compiled() (*expr.Program, error) {
	if v == nil {
		return nil, nil
	}
	return v.program, v.err
}
