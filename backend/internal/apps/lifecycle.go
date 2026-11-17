package apps

import (
	"context"
	"fmt"
	"regexp"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

// Category is what core code understands about a state. Names and labels belong to the app; categories do not,
// so an app can rename or add states without core changing (docs/apps-platform.md, D8).
type Category string

const (
	CategoryOpen    Category = "open"
	CategoryWaiting Category = "waiting"
	CategoryDone    Category = "done"
	CategoryClosed  Category = "closed"
)

func (c Category) valid() bool {
	return c == CategoryOpen || c == CategoryWaiting || c == CategoryDone || c == CategoryClosed
}

type State struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Category    Category `json:"category"`
	RenamedFrom string   `json:"renamed_from,omitempty"` // records in that old state move to this one on update
}

type Transition struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Verb    string `json:"verb"`
	Primary bool   `json:"primary,omitempty"` // the suggested next step from From
	// Automatic moves are made by the process (a workflow's record.transition step), never by hand: they are not
	// offered to people, and a person asking for one is refused. An approval moving a change on is one.
	Automatic bool `json:"automatic,omitempty"`

	// When is a guard: an expression that must be true for the move to be allowed. Error is what the person sees
	// when it is false. They are set together or not at all.
	When  string `json:"when,omitempty"`
	Error string `json:"error,omitempty"`
	// Guards are more guards, each with its own message, for a move that can be blocked for more than one reason.
	// They are checked in order after When, and the first that fails gives the message.
	Guards []Guard `json:"guards,omitempty"`
	// Lookups are counted before the guards run, which read them as lookups.name.
	Lookups Lookups `json:"lookups,omitempty"`

	checks []*Guard // When (as a guard) and Guards, compiled
}

// Guard is a condition a move needs, and what the person sees when it is not met.
type Guard struct {
	When  string `json:"when"`
	Error string `json:"error"`

	program *expr.Program
	err     error // why When could not be compiled (only seen when a stored guard no longer compiles)
}

// Guarded reports whether the move has any guard.
func (t *Transition) Guarded() bool { return t.When != "" || len(t.Guards) > 0 }

// Lifecycle is the status flow of one record type.
type Lifecycle struct {
	RecordType  string       `json:"record_type"`
	Initial     string       `json:"initial"`
	States      []State      `json:"states"`
	Transitions []Transition `json:"transitions"`
	// Fields are the extra fields of the app's form the guards read, with their types when it was installed.
	Fields expr.FieldTypes `json:"fields,omitempty"`

	// Source says where the lifecycle came from. It is set at runtime and is not part of the stored definition.
	App     string `json:"-"`
	Version string `json:"-"`
}

var stateIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ---- YAML ----

type lifecycleFile struct {
	Kind        string           `yaml:"kind"`
	RecordType  string           `yaml:"record_type"`
	Initial     string           `yaml:"initial"`
	States      orderedStates    `yaml:"states"`
	Transitions []transitionFile `yaml:"transitions"`
}

type transitionFile struct {
	From      string      `yaml:"from"`
	To        string      `yaml:"to"`
	Verb      string      `yaml:"verb"`
	Primary   bool        `yaml:"primary"`
	Automatic bool        `yaml:"automatic"` // made only by a workflow, never offered to people
	When      string      `yaml:"when"`      // an expression that must be true for the move to be allowed
	Error     string      `yaml:"error"`     // the message shown when it is false
	Guards    []guardFile `yaml:"guards"`    // more guards, each with its own message
	Lookups   Lookups     `yaml:"lookups"`   // counts the guards read as lookups.name
}

type guardFile struct {
	When  string `yaml:"when"`
	Error string `yaml:"error"`
}

// orderedStates reads a YAML map while keeping the author's order, which is the order states are shown in.
type orderedStates []State

func (o *orderedStates) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: states must be a map of state id to {label, category}", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		if val.Kind != yaml.MappingNode {
			return fmt.Errorf("line %d: state %q must be a map with label and category", val.Line, key.Value)
		}
		st := State{ID: key.Value}
		for j := 0; j+1 < len(val.Content); j += 2 {
			k, v := val.Content[j].Value, val.Content[j+1].Value
			switch k {
			case "label":
				st.Label = v
			case "category":
				st.Category = Category(v)
			case "renamed_from":
				st.RenamedFrom = v
			default:
				return fmt.Errorf("line %d: state %q has unknown field %q", val.Content[j].Line, key.Value, k)
			}
		}
		*o = append(*o, st)
	}
	return nil
}

func parseLifecycle(file string, data []byte, forms formSet, p *Problems) *Lifecycle {
	var f lifecycleFile
	if err := decodeStrict(data, &f); err != nil {
		p.add(file, "%v", err)
		return nil
	}
	if f.Kind != "lifecycle" {
		p.add(file, "kind must be lifecycle in this folder, got %q", f.Kind)
		return nil
	}
	lc := &Lifecycle{RecordType: f.RecordType, Initial: f.Initial, States: f.States, Fields: forms.fieldsUsed(f.RecordType, data)}
	before := len(*p)
	for _, t := range f.Transitions {
		tr := Transition{From: t.From, To: t.To, Verb: t.Verb, Primary: t.Primary, Automatic: t.Automatic, When: t.When, Error: t.Error, Lookups: t.Lookups}
		for _, g := range t.Guards {
			tr.Guards = append(tr.Guards, Guard{When: g.When, Error: g.Error})
		}
		lc.Transitions = append(lc.Transitions, tr)
	}
	lc.validate(file, p)
	if len(*p) > before {
		return nil
	}
	return lc
}

// ---- validation ----

// validate checks the lifecycle's structure and compiles every guard, reporting anything wrong. It is what an
// install runs.
func (l *Lifecycle) validate(file string, p *Problems) {
	l.validateStructure(file, p)
	l.compileGuards()
	env, envErr := expr.LifecycleGuardFor(l.Fields)
	for i := range l.Transitions {
		t := &l.Transitions[i]
		at := fmt.Sprintf("transitions[%d]: %s to %s", i, t.From, t.To)
		for _, msg := range t.Lookups.validate(env, envErr) {
			p.add(file, "%s: %s", at, msg)
		}
		if len(t.Lookups) > 0 && !t.Guarded() {
			p.add(file, "%s: has lookups but no guard to read them", at)
		}
		for j, g := range t.checks {
			where := "when"
			if t.When == "" || j > 0 {
				where = fmt.Sprintf("guards[%d]: when", j-boolInt(t.When != ""))
			}
			if g.err != nil {
				p.add(file, "%s: %s: %v", at, where, g.err)
			}
			if names := t.Lookups.undeclared(g.program); len(names) > 0 {
				p.add(file, "%s: %s: %s", at, where, undeclaredProblem(names))
			}
		}
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// compileGuards compiles each transition's guards and lookups. A guard that fails to compile is remembered, and the
// move it guards is blocked when attempted, so a broken rule can never let something through.
func (l *Lifecycle) compileGuards() {
	env, envErr := expr.LifecycleGuardFor(l.Fields)
	for i := range l.Transitions {
		t := &l.Transitions[i]
		t.Lookups.compile(env, envErr)
		t.checks = nil
		all := t.Guards
		if t.When != "" {
			all = append([]Guard{{When: t.When, Error: t.Error}}, t.Guards...)
		}
		for _, g := range all {
			g.program, g.err = nil, envErr
			if envErr == nil {
				g.program, g.err = env.Compile(g.When)
			}
			t.checks = append(t.checks, &g)
		}
	}
}

func (l *Lifecycle) validateStructure(file string, p *Problems) {
	if !validTypeName(l.RecordType) {
		p.add(file, "record_type %q is not a record type's name", l.RecordType)
	}
	if len(l.States) == 0 {
		p.add(file, "a lifecycle needs at least one state")
		return
	}
	ids := map[string]bool{}
	for _, s := range l.States {
		switch {
		case !stateIDRE.MatchString(s.ID):
			p.add(file, "state id %q must be lowercase letters, digits and underscores, starting with a letter", s.ID)
		case ids[s.ID]:
			p.add(file, "state %q is defined twice", s.ID)
		}
		ids[s.ID] = true
		if s.Label == "" {
			p.add(file, "state %q needs a label", s.ID)
		}
		if !s.Category.valid() {
			p.add(file, "state %q: category must be open, waiting, done or closed, got %q", s.ID, s.Category)
		}
	}
	for _, s := range l.States {
		if s.RenamedFrom != "" && ids[s.RenamedFrom] {
			p.add(file, "state %q: renamed_from %q is still a state in this lifecycle", s.ID, s.RenamedFrom)
		}
	}
	if !ids[l.Initial] {
		p.add(file, "initial state %q is not one of the states", l.Initial)
	}

	pairs := map[[2]string]bool{}
	primaries := map[string]int{}
	for i, t := range l.Transitions {
		switch {
		case !ids[t.From] || !ids[t.To]:
			p.add(file, "transitions[%d]: %q to %q refers to a state that does not exist", i, t.From, t.To)
			continue
		case t.From == t.To:
			p.add(file, "transitions[%d]: %q cannot move to itself", i, t.From)
		case t.Verb == "":
			p.add(file, "transitions[%d]: %s to %s needs a verb (the button label)", i, t.From, t.To)
		case pairs[[2]string{t.From, t.To}]:
			p.add(file, "transitions[%d]: %s to %s is listed twice", i, t.From, t.To)
		}
		pairs[[2]string{t.From, t.To}] = true
		if t.Primary && t.Automatic {
			p.add(file, "transitions[%d]: %s to %s is automatic, so it is never offered and cannot be the primary move", i, t.From, t.To)
		}
		if t.Primary {
			if primaries[t.From]++; primaries[t.From] > 1 {
				p.add(file, "transitions[%d]: %q has more than one primary transition", i, t.From)
			}
		}
		guardProblem := func(where, when, msg string) {
			switch {
			case when != "" && msg == "":
				p.add(file, "transitions[%d]: %s to %s has a guard (%swhen) but no error message to show when it fails", i, t.From, t.To, where)
			case when == "" && msg != "":
				p.add(file, "transitions[%d]: %s to %s has an error message but no guard (%swhen)", i, t.From, t.To, where)
			case len(msg) > 200:
				p.add(file, "transitions[%d]: the error message is longer than 200 characters", i)
			}
		}
		guardProblem("", t.When, t.Error)
		for j, g := range t.Guards {
			if g.When == "" && g.Error == "" {
				p.add(file, "transitions[%d]: %s to %s: guards[%d] needs when and error", i, t.From, t.To, j)
			}
			guardProblem(fmt.Sprintf("guards[%d]: ", j), g.When, g.Error)
		}
	}

	// Every state must be reachable, or it is dead: usually a typo in a transition.
	if ids[l.Initial] {
		reached := map[string]bool{l.Initial: true}
		for grew := true; grew; {
			grew = false
			for _, t := range l.Transitions {
				if reached[t.From] && !reached[t.To] && ids[t.To] {
					reached[t.To], grew = true, true
				}
			}
		}
		for _, s := range l.States {
			if !reached[s.ID] {
				p.add(file, "state %q can never be reached from %q", s.ID, l.Initial)
			}
		}
	}
}

// ---- runtime behaviour ----

func (l *Lifecycle) state(id string) (State, bool) {
	for _, s := range l.States {
		if s.ID == id {
			return s, true
		}
	}
	return State{}, false
}

func (l *Lifecycle) HasState(id string) bool { _, ok := l.state(id); return ok }

// Label falls back to the id for a state the lifecycle no longer knows, so old records still display.
func (l *Lifecycle) Label(id string) string {
	if s, ok := l.state(id); ok {
		return s.Label
	}
	return id
}

// Category reports a state's category. The second value is false for a state this lifecycle does not know.
func (l *Lifecycle) Category(id string) (Category, bool) {
	s, ok := l.state(id)
	return s.Category, ok
}

// CanTransition reports whether a record may move from one state to another.
// A record stranded in a state the lifecycle no longer has (an app was removed or replaced) may still be moved
// to any closed state, so it is never stuck.
func (l *Lifecycle) CanTransition(from, to string) bool {
	if !l.HasState(from) {
		return slices.ContainsFunc(l.StrandedTransitions(), func(t Transition) bool { return t.To == to })
	}
	return slices.ContainsFunc(l.Transitions, func(t Transition) bool { return t.From == from && t.To == to })
}

// StrandedTransitions are the moves offered for a record whose state this lifecycle does not have: it can be closed
// and nothing else. The API publishes them so clients offer exactly what the server will accept.
func (l *Lifecycle) StrandedTransitions() []Transition {
	var out []Transition
	for _, s := range l.States {
		if s.Category == CategoryClosed {
			out = append(out, Transition{To: s.ID, Verb: "Close", Primary: true})
		}
	}
	return out
}

// Automatic reports whether a move is made only by the process, never by hand.
func (l *Lifecycle) Automatic(from, to string) bool {
	return slices.ContainsFunc(l.Transitions, func(t Transition) bool { return t.From == from && t.To == to && t.Automatic })
}

// Guarded reports whether a move has a guard, so callers only load what a guard needs when there is one.
func (l *Lifecycle) Guarded(from, to string) bool {
	return slices.ContainsFunc(l.Transitions, func(t Transition) bool { return t.From == from && t.To == to && t.Guarded() })
}

// Check evaluates the guards on a move, in order, after counting its lookups. Only a definite yes from each lets
// it through: if a guard or a lookup cannot be evaluated (it no longer compiles, or evaluation fails) the error is
// returned and the caller must treat the move as not allowed. The message is the first failing guard's. A move
// without a guard, and a stranded record's move to a closed state, are always allowed here; whether the move exists
// at all is CanTransition's question.
func (l *Lifecycle) Check(ctx context.Context, from, to string, in expr.Input) (allowed bool, message string, err error) {
	for i := range l.Transitions {
		t := &l.Transitions[i]
		if t.From != from || t.To != to || !t.Guarded() {
			continue
		}
		if in.Lookups, err = t.Lookups.Eval(ctx, in); err != nil {
			return false, "", fmt.Errorf("the rule for %s to %s failed: %w", from, to, err)
		}
		for _, g := range t.checks {
			if g.err != nil {
				return false, "", fmt.Errorf("the rule for %s to %s cannot be checked: %w", from, to, g.err)
			}
			ok, err := g.program.Eval(ctx, in)
			if err != nil {
				return false, "", fmt.Errorf("the rule for %s to %s failed: %w", from, to, err)
			}
			if !ok {
				return false, g.Error, nil
			}
		}
		return true, "", nil
	}
	return true, "", nil
}

// TransitionsFrom lists the moves out of a state, the primary one first.
func (l *Lifecycle) TransitionsFrom(id string) []Transition {
	var out []Transition
	for _, t := range l.Transitions {
		if t.From == id {
			out = append(out, t)
		}
	}
	slices.SortStableFunc(out, func(a, b Transition) int {
		switch {
		case a.Primary && !b.Primary:
			return -1
		case !a.Primary && b.Primary:
			return 1
		}
		return 0
	})
	return out
}

// FallbackLifecycle is used for a record type when no installed app provides a lifecycle, so the product keeps
// working (and records stay movable) before any app is installed or after one is removed.
func FallbackLifecycle(recordType string) *Lifecycle {
	return &Lifecycle{
		RecordType: recordType,
		Initial:    "new",
		States: []State{
			{ID: "new", Label: "New", Category: CategoryOpen},
			{ID: "open", Label: "Open", Category: CategoryOpen},
			{ID: "closed", Label: "Closed", Category: CategoryClosed},
		},
		Transitions: []Transition{
			{From: "new", To: "open", Verb: "Open", Primary: true},
			{From: "new", To: "closed", Verb: "Close"},
			{From: "open", To: "closed", Verb: "Close", Primary: true},
			{From: "closed", To: "open", Verb: "Reopen", Primary: true},
		},
		App: "built-in",
	}
}
