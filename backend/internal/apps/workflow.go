package apps

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/script"
)

// Step types of this version. More arrive with what they depend on (mail, lists).
const (
	StepSet          = "set"
	StepCondition    = "condition"
	StepWait         = "wait"
	StepRecordUpdate = "record.update"
	// StepRecordTransition moves the record to another status of its lifecycle, through the same checks as a
	// person's move (the move must exist, its guard must agree); it may also make the moves marked automatic.
	StepRecordTransition = "record.transition"
	StepCommentAdd       = "comment.add"
	StepApproval         = "approval"
	// StepTaskCreate files a task under the record for a team, and (unless wait: false) waits until it is done or
	// cancelled. The result is vars.<id>_outcome: done, cancelled or timeout.
	StepTaskCreate = "task.create"
	// StepScript runs a Starlark script (docs/apps-platform.md §6.7): it reads the record and other records, asks for
	// changes and comments, and may set vars and choose one of its branches.
	StepScript = "script"
	// StepSubflow runs another workflow of the same app and waits for it; its variables come back.
	StepSubflow = "subflow"
	// StepForeach runs another workflow of the same app once per item of a list (or per record), some at a time.
	StepForeach = "foreach"
	// StepHTTP calls an outside API, at a host the app's manifest allows, with a secret the app declares.
	StepHTTP = "http.request"
	// StepQuery finds records of a type matching filters, as the run's actor: their ids for later steps.
	StepQuery = "query"
	// StepRecordCreate files a record of any type, through the save path.
	StepRecordCreate = "record.create"
	// StepNotify sends one of the app's notification templates, like a rule's notify action.
	StepNotify = "notify"
	StepEnd    = "end"

	maxWorkflowSteps = 50
	maxStepAssigns   = 20
	minWait          = time.Second
	maxWait          = 30 * 24 * time.Hour
)

// Approval rules: how many of the votes must agree. Each person in approvers has a vote, and so does each group in
// groups (cast by whichever member answers first).
const (
	RuleAny      = "any"
	RuleAll      = "all"
	RuleMajority = "majority"
)

var approvalRules = []string{RuleAny, RuleAll, RuleMajority}

var stepTypes = []string{StepSet, StepCondition, StepWait, StepRecordUpdate, StepRecordTransition, StepCommentAdd, StepApproval, StepTaskCreate, StepScript, StepSubflow, StepForeach, StepHTTP, StepQuery, StepRecordCreate, StepNotify, StepEnd}

var stepIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// NamedValue is one assignment: a variable of a `set` step or a field of a `record.update`. They are a list, not a
// map, so the author's order is kept and a later one can use an earlier one.
type NamedValue struct {
	Name  string `json:"name"`
	Value *Value `json:"value"`
}

// Step is one node of a workflow. Which fields apply depends on Type; validation refuses the rest.
type Step struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// Next is where to go after this step. Not used by condition (Then and Else) or end.
	Next string `json:"next,omitempty"`

	Vars []NamedValue `json:"vars,omitempty"` // set

	If   string `json:"if,omitempty"` // condition
	Then string `json:"then,omitempty"`
	Else string `json:"else,omitempty"`

	For       string `json:"for,omitempty"` // wait: a duration, or until an expression is true, with a limit
	Until     string `json:"until,omitempty"`
	Timeout   string `json:"timeout,omitempty"`
	OnTimeout string `json:"on_timeout,omitempty"`

	Set []NamedValue `json:"set,omitempty"` // record.update: field and value

	To string `json:"to,omitempty"` // record.transition: the status to move to

	Body     *Value `json:"body,omitempty"` // comment.add
	Internal bool   `json:"internal,omitempty"`

	Approvers string `json:"approvers,omitempty"` // approval: an expression producing a list of user ids
	// Groups is an approval's other source of approvers: an expression producing a list of group names, each of which
	// has one vote that any of its members may cast.
	Groups string `json:"groups,omitempty"`
	Rule   string `json:"rule,omitempty"` // approval: any, all or majority of the votes (default any)

	// task.create: the task's title and description, and the team (group name) it goes to. Wait false files it and
	// carries on (a later wait until record.open_tasks == 0 can collect several).
	Title       *Value `json:"title,omitempty"`
	Description *Value `json:"description,omitempty"`
	Group       *Value `json:"group,omitempty"`
	Wait        *bool  `json:"wait,omitempty"`

	// script: the Starlark to run, and the exits it may take besides next (by name, to a step).
	Script   string            `json:"script,omitempty"`
	Branches map[string]string `json:"branches,omitempty"`

	// subflow and foreach: the workflow (of this app) to run, and values for its variables. Record is the record a
	// subflow's run is for (an id; this run's when left out). Items is a foreach's list (an expression); Each says
	// whether they are records to run on ("record") or values (the default), each run getting vars.item and
	// vars.index; Parallel how many run at once (1 to 10); KeepGoing carries on after one fails.
	Workflow  string       `json:"workflow,omitempty"`
	Inputs    []NamedValue `json:"inputs,omitempty"`
	Record    *Value       `json:"record,omitempty"`
	Items     string       `json:"items,omitempty"`
	Each      string       `json:"each,omitempty"`
	Parallel  int          `json:"parallel,omitempty"`
	KeepGoing bool         `json:"keep_going,omitempty"`
	// OnError is where subflow, foreach and http.request go when they fail; without it the run fails.
	OnError string `json:"on_error,omitempty"`

	// http.request: the method, URL, headers and body (Body), with a secret the app declares sent as Auth (bearer,
	// basic or header, AuthHeader naming the header), and how many times to try again on a network fault or a 5xx.
	Method     string       `json:"method,omitempty"`
	URL        *Value       `json:"url,omitempty"`
	Headers    []NamedValue `json:"headers,omitempty"`
	Secret     string       `json:"secret,omitempty"`
	Auth       string       `json:"auth,omitempty"`
	AuthHeader string       `json:"auth_header,omitempty"`
	Retries    int          `json:"retries,omitempty"`

	// query: what to find.
	Find *RecordQuery `json:"find,omitempty"`

	// record.create: the type, its extra fields and the record it goes under (Title, Description and Group as for
	// task.create).
	RecordType string       `json:"record_type,omitempty"`
	Fields     []NamedValue `json:"fields,omitempty"`
	Parent     *Value       `json:"parent,omitempty"`

	// notify: one of the app's notification templates, to people named as a rule's notify names them (assignee,
	// requester, assignment_group, or =an expression giving user ids).
	Template   string   `json:"template,omitempty"`
	Recipients []string `json:"recipients,omitempty"`
	recipients []*expr.Program
	items      *Value

	program   *script.Program
	cond      *expr.Program
	until     *expr.Program
	approvers *expr.Program
	groups    *expr.Program
	compileEr error
	forDur    time.Duration
	timeout   time.Duration
}

// Workflow is a graph of steps run by the durable engine, interpreted from this definition.
type Workflow struct {
	Name       string `json:"name"`
	RecordType string `json:"record_type"`
	Start      string `json:"start"`
	Steps      []Step `json:"steps"`
	// Fields are the extra fields of the app's form the workflow reads or sets, with their types when it was installed.
	Fields expr.FieldTypes `json:"fields,omitempty"`
	// Schedule starts the workflow on its own, by the clock (as the app): once, or once per record a query finds.
	Schedule *Schedule `json:"schedule,omitempty"`
	// Layout is where each step sits in the builder's diagram. The engine ignores it; without it the diagram is laid
	// out from the graph.
	Layout map[string]Point `json:"layout,omitempty"`
	// Modules are the app's script files its script steps load (name to source), as they were at install: a run in
	// flight keeps the code it started with.
	Modules map[string]string `json:"modules,omitempty"`

	// App and Version say where the workflow came from. They are set at runtime and are not part of the definition.
	App     string `json:"-"`
	Version string `json:"-"`
}

// ---- YAML ----

type workflowFile struct {
	Kind       string           `yaml:"kind"`
	Name       string           `yaml:"name"`
	RecordType string           `yaml:"record_type"`
	Start      string           `yaml:"start"`
	Steps      orderedSteps     `yaml:"steps"`
	Layout     map[string]Point `yaml:"layout"`
	Schedule   *Schedule        `yaml:"schedule"`
}

// Point is a position in the builder's diagram.
type Point struct {
	X int `json:"x" yaml:"x"`
	Y int `json:"y" yaml:"y"`
}

type stepFile struct {
	Type       string            `yaml:"type"`
	Next       string            `yaml:"next"`
	Vars       orderedValues     `yaml:"vars"`
	If         string            `yaml:"if"`
	Then       string            `yaml:"then"`
	Else       string            `yaml:"else"`
	For        string            `yaml:"for"`
	Until      string            `yaml:"until"`
	Timeout    string            `yaml:"timeout"`
	OnTimeout  string            `yaml:"on_timeout"`
	Set        orderedValues     `yaml:"set"`
	To         string            `yaml:"to"`
	Body       *Value            `yaml:"body"`
	Internal   bool              `yaml:"internal"`
	Approvers  string            `yaml:"approvers"`
	Groups     string            `yaml:"groups"`
	Rule       string            `yaml:"rule"`
	Title      *Value            `yaml:"title"`
	Desc       *Value            `yaml:"description"`
	Group      *Value            `yaml:"group"`
	Wait       *bool             `yaml:"wait"`
	Script     string            `yaml:"script"`
	Branches   map[string]string `yaml:"branches"`
	Workflow   string            `yaml:"workflow"`
	Inputs     orderedValues     `yaml:"inputs"`
	Record     *Value            `yaml:"record"`
	Items      string            `yaml:"items"`
	Each       string            `yaml:"each"`
	Parallel   int               `yaml:"parallel"`
	KeepGoing  bool              `yaml:"keep_going"`
	OnError    string            `yaml:"on_error"`
	Method     string            `yaml:"method"`
	URL        *Value            `yaml:"url"`
	Headers    orderedValues     `yaml:"headers"`
	Secret     string            `yaml:"secret"`
	Auth       string            `yaml:"auth"`
	AuthHeader string            `yaml:"auth_header"`
	Retries    int               `yaml:"retries"`
	Find       *RecordQuery      `yaml:"find"`
	RecordType string            `yaml:"record_type"`
	Fields     orderedValues     `yaml:"fields"`
	Parent     *Value            `yaml:"parent"`
	Template   string            `yaml:"template"`
	Recipients []string          `yaml:"recipients"`
}

// orderedSteps reads the steps map keeping the author's order, which is the order the viewer lists them in.
type orderedSteps []Step

func (o *orderedSteps) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: steps must be a map of step id to its definition", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		var f stepFile
		// Strictly: an unknown field is an author's typo, not something to ignore.
		if err := decodeNodeStrict(val, &f); err != nil {
			return fmt.Errorf("step %q: %w", key.Value, err)
		}
		*o = append(*o, Step{ID: key.Value, Type: f.Type, Next: f.Next, Vars: f.Vars, If: f.If, Then: f.Then, Else: f.Else,
			For: f.For, Until: f.Until, Timeout: f.Timeout, OnTimeout: f.OnTimeout, Set: f.Set, To: f.To, Body: f.Body, Internal: f.Internal,
			Approvers: f.Approvers, Groups: f.Groups, Rule: f.Rule, Title: f.Title, Description: f.Desc, Group: f.Group, Wait: f.Wait,
			Script: strings.TrimRight(f.Script, "\n"), Branches: f.Branches,
			Workflow: f.Workflow, Inputs: f.Inputs, Record: f.Record, Items: f.Items, Each: f.Each, Parallel: f.Parallel, KeepGoing: f.KeepGoing,
			OnError: f.OnError, Method: f.Method, URL: f.URL, Headers: f.Headers, Secret: f.Secret, Auth: f.Auth, AuthHeader: f.AuthHeader,
			Retries: f.Retries, Find: f.Find, RecordType: f.RecordType, Fields: f.Fields, Parent: f.Parent, Template: f.Template, Recipients: f.Recipients})
	}
	return nil
}

func decodeNodeStrict(n *yaml.Node, into any) error {
	raw, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	return decodeStrict(raw, into)
}

// orderedValues reads a map of name to value, keeping the order.
type orderedValues []NamedValue

func (o *orderedValues) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected a map of name to value", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		var v Value
		if err := n.Content[i+1].Decode(&v); err != nil {
			return fmt.Errorf("%q: %w", n.Content[i].Value, err)
		}
		*o = append(*o, NamedValue{Name: n.Content[i].Value, Value: &v})
	}
	return nil
}

func parseWorkflow(file string, data []byte, forms formSet, scripts map[string]string, p *Problems) *Workflow {
	var f workflowFile
	if err := decodeStrict(data, &f); err != nil {
		p.add(file, "%v", err)
		return nil
	}
	if f.Kind != "workflow" {
		p.add(file, "kind must be workflow in this folder, got %q", f.Kind)
		return nil
	}
	w := &Workflow{Name: f.Name, RecordType: f.RecordType, Start: f.Start, Steps: f.Steps, Schedule: f.Schedule, Fields: forms.fieldsUsed(f.RecordType, data)}
	var sources []string
	for _, st := range w.Steps {
		sources = append(sources, st.Script)
	}
	w.Modules = modulesFor(scripts, sources...)
	// Positions of steps that no longer exist are dropped rather than refused: they only place boxes on a screen.
	for id := range f.Layout {
		if slices.ContainsFunc(f.Steps, func(s Step) bool { return s.ID == id }) {
			if w.Layout == nil {
				w.Layout = map[string]Point{}
			}
			w.Layout[id] = f.Layout[id]
		}
	}
	before := len(*p)
	w.validate(file, p)
	if len(*p) > before {
		return nil
	}
	return w
}

// ---- validation ----

func (w *Workflow) validate(file string, p *Problems) {
	w.validateStructure(file, p)
	w.compile()
	for i := range w.Steps {
		if err := w.Steps[i].compileEr; err != nil {
			p.add(file, "step %s: %v", w.Steps[i].ID, err)
		}
	}
	for _, msg := range w.scheduleProblems() {
		p.add(file, "schedule: %s", msg)
	}
}

// scheduleProblems compiles the workflow's schedule, if it has one.
func (w *Workflow) scheduleProblems() []string {
	if w.Schedule == nil {
		return nil
	}
	env, envErr := expr.WorkflowFor(w.Fields)
	return w.Schedule.compile(w, env, envErr)
}

// ParseDuration reads a duration such as 30s, 15m, 2h or 7d (days are the one unit Go does not have).
func ParseDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 1 {
			return 0, fmt.Errorf("%q is not a duration such as 30s, 15m, 2h or 7d", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 30s, 15m, 2h or 7d", s)
	}
	return d, nil
}

func (w *Workflow) validateStructure(file string, p *Problems) {
	if !ruleNameRE.MatchString(w.Name) {
		p.add(file, "name %q must be 2-63 characters: lowercase letters, digits and hyphens, starting with a letter", w.Name)
	}
	if !validTypeName(w.RecordType) {
		p.add(file, "record_type %q is not a record type's name", w.RecordType)
	}
	switch n := len(w.Steps); {
	case n == 0:
		p.add(file, "a workflow needs steps")
		return
	case n > maxWorkflowSteps:
		p.add(file, "a workflow may have at most %d steps, this has %d", maxWorkflowSteps, n)
	}
	byID := map[string]*Step{}
	for i := range w.Steps {
		s := &w.Steps[i]
		if !stepIDRE.MatchString(s.ID) {
			p.add(file, "step id %q must be lowercase letters, digits and underscores, starting with a letter", s.ID)
		}
		byID[s.ID] = s
	}
	if w.Start == "" {
		p.add(file, "start is required: the id of the first step")
	} else if byID[w.Start] == nil {
		p.add(file, "start: there is no step %q", w.Start)
	}
	for i := range w.Steps {
		s := &w.Steps[i]
		for _, msg := range s.structureProblems(w, byID) {
			p.add(file, "step %s: %s", s.ID, msg)
		}
	}
	if byID[w.Start] != nil {
		w.graphProblems(file, byID, p)
	}
}

// edges are the steps this step can go to next.
func (s *Step) edges() []string {
	var out []string
	for _, t := range []string{s.Next, s.Then, s.Else, s.OnTimeout, s.OnError} {
		if t != "" {
			out = append(out, t)
		}
	}
	for _, name := range s.BranchNames() {
		out = append(out, s.Branches[name])
	}
	return out
}

// BranchNames are a script step's exits besides next, in order.
func (s *Step) BranchNames() []string {
	names := make([]string, 0, len(s.Branches))
	for name := range s.Branches {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (s *Step) structureProblems(w *Workflow, byID map[string]*Step) (out []string) {
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	forbid := func(name string, present bool) {
		if present {
			add("%s does not take %s", s.Type, name)
		}
	}
	target := func(field, id string, required bool) {
		switch {
		case id == "" && required:
			add("%s needs %s: the step to go to", s.Type, field)
		case id != "" && byID[id] == nil:
			add("%s: there is no step %q", field, id)
		case id == s.ID:
			add("%s: a step cannot go to itself", field)
		}
	}
	// Every field this type does not use is refused, so a typo or a misplaced field is not silently ignored.
	unused := func(used ...string) {
		all := map[string]bool{"next": s.Next != "", "vars": len(s.Vars) > 0, "if": s.If != "", "then": s.Then != "", "else": s.Else != "",
			"for": s.For != "", "until": s.Until != "", "timeout": s.Timeout != "", "on_timeout": s.OnTimeout != "",
			"set": len(s.Set) > 0, "to": s.To != "", "body": s.Body != nil, "internal": s.Internal, "approvers": s.Approvers != "", "groups": s.Groups != "", "rule": s.Rule != "",
			"title": s.Title != nil, "description": s.Description != nil, "group": s.Group != nil, "wait": s.Wait != nil,
			"script": s.Script != "", "branches": len(s.Branches) > 0,
			"workflow": s.Workflow != "", "inputs": len(s.Inputs) > 0, "record": s.Record != nil, "items": s.Items != "", "each": s.Each != "",
			"parallel": s.Parallel != 0, "keep_going": s.KeepGoing, "on_error": s.OnError != "", "method": s.Method != "", "url": s.URL != nil,
			"headers": len(s.Headers) > 0, "secret": s.Secret != "", "auth": s.Auth != "", "auth_header": s.AuthHeader != "", "retries": s.Retries != 0,
			"find": s.Find != nil, "record_type": s.RecordType != "", "fields": len(s.Fields) > 0, "parent": s.Parent != nil,
			"template": s.Template != "", "recipients": len(s.Recipients) > 0}
		for _, u := range used {
			delete(all, u)
		}
		for _, name := range []string{"next", "vars", "if", "then", "else", "for", "until", "timeout", "on_timeout", "set", "to", "body", "internal", "approvers", "groups", "rule", "title", "description", "group", "wait", "script", "branches",
			"workflow", "inputs", "record", "items", "each", "parallel", "keep_going", "on_error", "method", "url", "headers", "secret", "auth",
			"auth_header", "retries", "find", "record_type", "fields", "parent", "template", "recipients"} {
			forbid(name, all[name])
		}
	}
	switch s.Type {
	case StepSet:
		unused("next", "vars")
		target("next", s.Next, true)
		if len(s.Vars) == 0 {
			add("set needs vars: the variables to set")
		}
		out = append(out, assignProblems("vars", s.Vars, func(name string) []string {
			if !varNameRE.MatchString(name) {
				return []string{fmt.Sprintf("%q is not a variable name (lowercase letters, digits and underscores)", name)}
			}
			return nil
		})...)
	case StepCondition:
		unused("if", "then", "else")
		if s.If == "" {
			add("condition needs if: the expression to test")
		}
		target("then", s.Then, true)
		target("else", s.Else, true)
		if s.Then != "" && s.Then == s.Else {
			add("then and else go to the same step, so the condition decides nothing")
		}
	case StepWait:
		unused("next", "for", "until", "timeout", "on_timeout")
		target("next", s.Next, true)
		switch {
		case s.For != "" && s.Until != "":
			add("wait takes either for or until, not both")
		case s.For == "" && s.Until == "":
			add("wait needs for (a duration) or until (an expression)")
		}
		if s.For != "" {
			forbid("timeout", s.Timeout != "")
			forbid("on_timeout", s.OnTimeout != "")
			if d, err := ParseDuration(s.For); err != nil {
				add("for: %v", err)
			} else if d < minWait || d > maxWait {
				add("for must be between %s and 30d", minWait)
			}
		}
		if s.Until != "" {
			if s.Timeout == "" {
				add("wait until needs a timeout, so no workflow waits forever")
			} else if d, err := ParseDuration(s.Timeout); err != nil {
				add("timeout: %v", err)
			} else if d < minWait || d > maxWait {
				add("timeout must be between %s and 30d", minWait)
			}
			target("on_timeout", s.OnTimeout, false)
		}
	case StepRecordUpdate:
		unused("next", "set")
		target("next", s.Next, true)
		if len(s.Set) == 0 {
			add("record.update needs set: the fields to change")
		}
		out = append(out, assignProblems("set", s.Set, func(name string) []string {
			_, ok := SettableFields[name]
			if extra, isExtra := strings.CutPrefix(name, "fields."); isExtra {
				if w.Fields[extra] == "" {
					return []string{fmt.Sprintf("record.update: this app's form for %s records has no field %q", w.RecordType, extra)}
				}
				return nil
			}
			switch {
			case !ok:
				return []string{fmt.Sprintf("record.update cannot change %q (it can change: %s)", name, settableList())}
			case ownerProblem(name, w.RecordType, "a workflow") != "":
				return []string{ownerProblem(name, w.RecordType, "a workflow")}
			}
			return nil
		})...)
		for _, nv := range s.Set {
			kind, ok := SettableFields[nv.Name]
			if extra, isExtra := strings.CutPrefix(nv.Name, "fields."); isExtra {
				kind, ok = w.Fields[extra], w.Fields[extra] != ""
			}
			if ok && nv.Value != nil && nv.Value.Expr == "" {
				out = append(out, literalProblems(nv.Name, kind, nv.Value.Literal)...)
			}
		}
	case StepRecordTransition:
		unused("next", "to")
		target("next", s.Next, true)
		switch {
		case s.To == "":
			add("record.transition needs to: the status to move to")
		case !stateIDRE.MatchString(s.To):
			add("to: %q is not a status id", s.To)
		}
	case StepCommentAdd:
		unused("next", "body", "internal")
		target("next", s.Next, true)
		if s.Body == nil {
			add("comment.add needs a body")
		}
	case StepApproval:
		unused("next", "approvers", "groups", "rule", "timeout", "on_timeout")
		target("next", s.Next, true)
		target("on_timeout", s.OnTimeout, false)
		if s.Approvers == "" && s.Groups == "" {
			add("approval needs approvers (an expression producing a list of user ids) or groups (one producing a list of group names), or both")
		}
		if s.Rule != "" && !slices.Contains(approvalRules, s.Rule) {
			add("rule must be one of %s, got %q", strings.Join(approvalRules, ", "), s.Rule)
		}
		if s.Timeout != "" {
			if d, err := ParseDuration(s.Timeout); err != nil {
				add("timeout: %v", err)
			} else if d < minWait || d > maxWait {
				add("timeout must be between %s and 30d", minWait)
			}
		}
	case StepScript:
		unused("next", "script", "branches")
		target("next", s.Next, true)
		if strings.TrimSpace(s.Script) == "" {
			add("script needs script: the Starlark to run")
		}
		for _, name := range s.BranchNames() {
			if !varNameRE.MatchString(name) {
				add("branches: %q is not a name (lowercase letters, digits and underscores)", name)
			}
			target("branches."+name, s.Branches[name], true)
		}
	case StepSubflow, StepForeach, StepHTTP, StepQuery, StepRecordCreate, StepNotify:
		s.moreProblems(w, unused, target, add, forbid)
	case StepTaskCreate:
		unused("next", "title", "description", "group", "wait", "timeout", "on_timeout")
		target("next", s.Next, true)
		target("on_timeout", s.OnTimeout, false)
		if s.Title == nil {
			add("task.create needs a title")
		} else if s.Title.Expr == "" {
			if t, ok := s.Title.Literal.(string); !ok || strings.TrimSpace(t) == "" {
				add("title must be text, or =an expression")
			}
		}
		if s.Group != nil && s.Group.Expr == "" {
			if g, ok := s.Group.Literal.(string); !ok || strings.TrimSpace(g) == "" {
				add("group must be a group's name, or =an expression giving one")
			}
		}
		if !s.Waits() {
			forbid("timeout", s.Timeout != "")
			forbid("on_timeout", s.OnTimeout != "")
		}
		if s.Timeout != "" {
			if d, err := ParseDuration(s.Timeout); err != nil {
				add("timeout: %v", err)
			} else if d < minWait || d > maxWait {
				add("timeout must be between %s and 30d", minWait)
			}
		}
	case StepEnd:
		unused()
	case "":
		add("a step needs a type (%s)", strings.Join(stepTypes, ", "))
	default:
		add("unknown step type %q (available: %s)", s.Type, strings.Join(stepTypes, ", "))
	}
	return out
}

var varNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

func assignProblems(what string, list []NamedValue, name func(string) []string) (out []string) {
	if len(list) > maxStepAssigns {
		out = append(out, fmt.Sprintf("%s has %d entries; the limit is %d", what, len(list), maxStepAssigns))
	}
	seen := map[string]bool{}
	for _, nv := range list {
		out = append(out, name(nv.Name)...)
		if seen[nv.Name] {
			out = append(out, fmt.Sprintf("%s: %q is set twice", what, nv.Name))
		}
		seen[nv.Name] = true
	}
	return out
}

// graphProblems checks the shape: no cycles, and nothing that can never run. A workflow is a DAG, so every execution
// is finite; repetition comes from waiting and from steps that loop over a list, never from a loop in the graph.
func (w *Workflow) graphProblems(file string, byID map[string]*Step, p *Problems) {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var path []string
	var walk func(id string) bool
	walk = func(id string) bool {
		switch state[id] {
		case visiting:
			from := slices.Index(path, id)
			p.add(file, "the steps loop: %s", strings.Join(append(append([]string(nil), path[from:]...), id), " -> "))
			return false
		case done:
			return true
		}
		state[id] = visiting
		path = append(path, id)
		ok := true
		for _, next := range byID[id].edges() {
			if byID[next] != nil && !walk(next) {
				ok = false
				break
			}
		}
		path = path[:len(path)-1]
		state[id] = done
		return ok
	}
	if !walk(w.Start) {
		return
	}
	for i := range w.Steps {
		if state[w.Steps[i].ID] == 0 {
			p.add(file, "step %s can never run: nothing leads to it", w.Steps[i].ID)
		}
	}
}

// ---- expressions ----

// compile compiles every expression and duration in the workflow. Anything that fails is remembered, so at run time
// the step fails (and the execution with it) instead of being silently skipped.
func (w *Workflow) compile() {
	env, envErr := expr.WorkflowFor(w.Fields)
	if w.Schedule != nil {
		_ = w.Schedule.compile(w, env, envErr) // its problems are reported by validate
	}
	for i := range w.Steps {
		s := &w.Steps[i]
		s.cond, s.until, s.approvers, s.groups, s.program, s.compileEr = nil, nil, nil, nil, nil, envErr
		if envErr != nil {
			continue
		}
		var problems []string
		fail := func(what string, err error) {
			switch {
			case err == nil:
			case what == "":
				problems = append(problems, err.Error())
			default:
				problems = append(problems, fmt.Sprintf("%s: %v", what, err))
			}
		}
		if s.If != "" {
			var err error
			s.cond, err = env.Compile(s.If)
			fail("if", err)
		}
		if s.Script != "" {
			var err error
			s.program, err = script.Compile(s.Script, w.Modules)
			fail("script", err)
		}
		if s.Until != "" {
			var err error
			s.until, err = env.Compile(s.Until)
			fail("until", err)
		}
		if s.Approvers != "" {
			var err error
			s.approvers, err = env.CompileAs(s.Approvers, expr.StringList)
			fail("approvers", err)
		}
		if s.Groups != "" {
			var err error
			s.groups, err = env.CompileAs(s.Groups, expr.StringList)
			fail("groups", err)
		}
		if s.For != "" {
			var err error
			s.forDur, err = ParseDuration(s.For)
			fail("for", err)
		}
		if s.Timeout != "" {
			var err error
			s.timeout, err = ParseDuration(s.Timeout)
			fail("timeout", err)
		}
		for _, nv := range s.Vars {
			nv.Value.compileAny(env, envErr)
			fail("vars."+nv.Name, nv.Value.err)
		}
		for _, nv := range s.Set {
			want := expr.String
			if SettableFields[nv.Name] == "number" {
				want = expr.Int
			}
			if extra, ok := strings.CutPrefix(nv.Name, "fields."); ok {
				want = expr.KindType(w.Fields[extra])
			}
			nv.Value.compile(env, envErr, want)
			fail("set."+nv.Name, nv.Value.err)
		}
		if s.Type == StepHTTP {
			s.Body.compileAny(env, envErr) // a request's body may be any JSON
		} else {
			s.Body.compile(env, envErr, expr.String)
		}
		s.compileMore(env, envErr, fail)
		if s.Body != nil {
			fail("body", s.Body.err)
		}
		for _, v := range []struct {
			name string
			val  *Value
		}{{"title", s.Title}, {"description", s.Description}, {"group", s.Group}} {
			v.val.compile(env, envErr, expr.String)
			if v.val != nil {
				fail(v.name, v.val.err)
			}
		}
		if len(problems) > 0 {
			s.compileEr = fmt.Errorf("%s", strings.Join(problems, "; "))
		}
	}
}

func (v *Value) compileAny(env *expr.Env, envErr error) {
	if v == nil {
		return
	}
	v.program, v.err = nil, envErr
	if envErr == nil && v.Expr != "" {
		v.program, v.err = env.CompileAny(v.Expr)
	}
}

// ---- what the engine needs to run a workflow ----

// Compile prepares a workflow read back from storage (a snapshot pinned by an execution, or a registry load): its
// expressions are compiled again, and a step that no longer compiles fails when it is reached.
func (w *Workflow) Compile() { w.compile() }

// Step returns the step with this id.
func (w *Workflow) Step(id string) *Step {
	for i := range w.Steps {
		if w.Steps[i].ID == id {
			return &w.Steps[i]
		}
	}
	return nil
}

// Condition returns a condition step's compiled test, or why it cannot run.
func (s *Step) Condition() (*expr.Program, error) { return s.cond, s.compileEr }

// Program is a script step's compiled script.
func (s *Step) Program() (*script.Program, error) { return s.program, s.compileEr }

// UntilCondition returns a wait step's compiled `until`, or why it cannot run.
func (s *Step) UntilCondition() (*expr.Program, error) { return s.until, s.compileEr }

// ApproversProgram returns an approval step's compiled `approvers`, or why it cannot run.
func (s *Step) ApproversProgram() (*expr.Program, error) { return s.approvers, s.compileEr }

// GroupsProgram returns an approval step's compiled `groups`: nil when it has none.
func (s *Step) GroupsProgram() (*expr.Program, error) { return s.groups, s.compileEr }

// ApprovalRule is an approval step's rule (any, all or majority), defaulting to any.
func (s *Step) ApprovalRule() string {
	if s.Rule == "" {
		return RuleAny
	}
	return s.Rule
}

// ForDuration and TimeoutDuration are a wait step's parsed durations.
func (s *Step) ForDuration() time.Duration     { return s.forDur }
func (s *Step) TimeoutDuration() time.Duration { return s.timeout }

// Waits says whether a task.create step waits for its task (the default) or carries on once it is filed.
func (s *Step) Waits() bool { return s.Wait == nil || *s.Wait }

// Err is why the step's expressions cannot run, or nil.
func (s *Step) Err() error { return s.compileEr }

// checkWorkflowStarts checks what only the whole app can tell: that every workflow a rule starts is defined in the same
// app, for the same kind of record. Definitions are checked one at a time, so this runs once they all are.
func checkWorkflowStarts(defs []Definition, p *Problems) {
	workflows := map[string]*Workflow{}
	for _, d := range defs {
		if d.Workflow != nil {
			workflows[d.Workflow.Name] = d.Workflow
		}
	}
	for _, d := range defs {
		for _, lr := range d.actionRules() {
			for i, a := range lr.rule.Run {
				if a.Action != "workflow.start" || a.Workflow == "" {
					continue
				}
				wf := workflows[a.Workflow]
				switch {
				case wf == nil:
					p.add(d.File, "%srun[%d]: there is no workflow %q in this app (workflows: %s)", lr.at, i, a.Workflow, workflowNames(workflows))
				case wf.RecordType != lr.rule.RecordType && wf.RecordType != "task":
					p.add(d.File, "%srun[%d]: workflow %q is for %s records, but this is for %s", lr.at, i, a.Workflow, wf.RecordType, lr.rule.RecordType)
				}
			}
		}
	}
}

func workflowNames(m map[string]*Workflow) string {
	if len(m) == 0 {
		return "none"
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// checkTransitionTargets checks what only the whole app can tell: a record.transition step in a workflow for a type
// whose lifecycle the same app defines must name one of its states. A lifecycle from another app is checked when the
// step runs, since it can change after this app is installed.
func checkTransitionTargets(defs []Definition, p *Problems) {
	lifecycles := map[string]*Lifecycle{}
	for _, d := range defs {
		if d.Lifecycle != nil {
			lifecycles[d.Lifecycle.RecordType] = d.Lifecycle
		}
	}
	for _, d := range defs {
		if d.Workflow == nil {
			continue
		}
		lc := lifecycles[d.Workflow.RecordType]
		if lc == nil {
			continue
		}
		for _, s := range d.Workflow.Steps {
			if s.Type == StepRecordTransition && s.To != "" && !lc.HasState(s.To) {
				p.add(d.File, "step %s: to: %q is not a status of this app's %s lifecycle", s.ID, s.To, lc.RecordType)
			}
		}
	}
}
