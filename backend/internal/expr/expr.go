// Package expr evaluates the expressions apps use for conditions (docs/apps-platform.md, §5). They are CEL:
// pure, always terminating, and type-checked when an app is installed, so a typo such as record.prioity is
// rejected at install time and not discovered in production.
//
// What an expression can see is fixed by the environment it is compiled in. Optional values are tested with
// has(record.assignee_id); comparing to null is rejected, because for some types it silently never matches.
package expr

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/ext"
)

const (
	// MaxSourceLength keeps an expression readable; anything longer belongs in a workflow.
	MaxSourceLength = 1000
	// costLimit aborts a pathological expression (a huge list, deep nesting) instead of letting it run.
	costLimit = 100_000
)

// Record is what an expression sees as `record` (and `old`): a ticket as flat, typed fields.
// Optional fields are pointers, so has(record.assignee_id) is false when unset.
type Record struct {
	ID            string  `cel:"id"`
	Type          string  `cel:"type"`
	Number        int64   `cel:"number"`
	Reference     string  `cel:"reference"`
	Title         string  `cel:"title"`
	Description   string  `cel:"description"`
	Status        string  `cel:"status"`
	StateCategory string  `cel:"state_category"`
	Priority      int64   `cel:"priority"`
	RequesterID   string  `cel:"requester_id"`
	RequesterName string  `cel:"requester_name"`
	AssigneeID    *string `cel:"assignee_id"`
	AssigneeName  *string `cel:"assignee_name"`
	// AssignmentGroup is the name of the team the ticket is routed to; rules and workflows set it by name.
	AssignmentGroupID *string   `cel:"assignment_group_id"`
	AssignmentGroup   *string   `cel:"assignment_group"`
	CreatedAt         time.Time `cel:"created_at"`
	UpdatedAt         time.Time `cel:"updated_at"`
	// Resolution is an incident's own column. It reads as empty for types that have none, so a guard written for
	// `task` never fails to evaluate: the record is flat, whichever table a field lives in.
	Resolution string `cel:"resolution"`
	// A change's own columns, empty for other types. change_type is standard, normal or emergency; risk and impact
	// are low, medium, high or "" (not assessed). The planned window is a zero time until it is set, so test it with
	// has(record.planned_start).
	ChangeType         string    `cel:"change_type"`
	Risk               string    `cel:"risk"`
	Impact             string    `cel:"impact"`
	PlannedStart       time.Time `cel:"planned_start"`
	PlannedEnd         time.Time `cel:"planned_end"`
	ImplementationPlan string    `cel:"implementation_plan"`
	BackoutPlan        string    `cel:"backout_plan"`
	TestPlan           string    `cel:"test_plan"`
}

// User is what an expression sees as `user`: the person acting.
type User struct {
	ID    string `cel:"id"`
	Name  string `cel:"name"`
	Email string `cel:"email"`
	Role  string `cel:"role"`
}

// Input is the data an expression is evaluated against. Now is an input, not a function, so a result depends only
// on what was passed in.
type Input struct {
	Record *Record
	Old    *Record
	User   *User
	Now    time.Time
	// Event is what is happening to the record (create or update). Only rules see it; guards have no event.
	Event string
	// Vars are a workflow's variables. Only workflows see them.
	Vars map[string]any
	// Groups looks up the ids of a group's members by the group's name, for group('name'). Only workflows can use
	// group(); an expression that does is refused if this is nil. A missing group is an error, never an empty list.
	Groups func(ctx context.Context, name string) ([]string, error)
	// Roles looks up the ids of everyone who holds a role, for role('name'), the same way.
	Roles func(ctx context.Context, name string) ([]string, error)
}

// A lookup is a function of a name written in quotes, such as group('CAB') or role('cab-member'), whose result is
// people, who change: so it is data, not something an app can know when it is installed. The function is a macro
// for an index into a hidden map variable, and only the names written in the expression are looked up, just before
// it runs.
type lookup struct {
	fn, variable, example string
}

var lookups = []lookup{
	{fn: "group", variable: "_groups", example: "group('CAB')"},
	{fn: "role", variable: "_roles", example: "role('cab-member')"},
}

// macro expands fn('name'). The name must be written in quotes, so which names an expression needs is known when it
// is compiled.
func (l lookup) macro() cel.Macro {
	return cel.GlobalMacro(l.fn, 1, func(eh cel.MacroExprFactory, _ ast.Expr, args []ast.Expr) (ast.Expr, *common.Error) {
		arg := args[0]
		if arg.Kind() != ast.LiteralKind || arg.AsLiteral().Type() != types.StringType {
			return nil, eh.NewError(arg.ID(), l.fn+"() takes a name in quotes, such as "+l.example)
		}
		return eh.NewCall(operators.Index, eh.NewIdent(l.variable), eh.Copy(arg)), nil
	})
}

// names lists the names a checked expression passes to l's function. Any other use of the hidden variable is refused,
// so the list is complete.
func (l lookup) names(a *cel.Ast) ([]string, error) {
	var names []string
	var bad bool
	root := a.NativeRep()
	seen := map[int64]bool{}
	ast.PreOrderVisit(root.Expr(), ast.NewExprVisitor(func(e ast.Expr) {
		switch e.Kind() {
		case ast.CallKind:
			c := e.AsCall()
			if c.FunctionName() == operators.Index && len(c.Args()) == 2 && c.Args()[0].Kind() == ast.IdentKind && c.Args()[0].AsIdent() == l.variable {
				seen[c.Args()[0].ID()] = true
				s, ok := "", false
				if lit := c.Args()[1]; lit.Kind() == ast.LiteralKind {
					s, ok = lit.AsLiteral().Value().(string)
				}
				if !ok {
					bad = true
				} else if !slices.Contains(names, s) {
					names = append(names, s)
				}
			}
		case ast.IdentKind:
			if e.AsIdent() == l.variable && !seen[e.ID()] {
				bad = true
			}
		}
	}))
	if bad {
		return nil, errors.New("use " + l.fn + "('name') to name a " + l.fn)
	}
	return names, nil
}

// Env compiles expressions for one context. It is safe for concurrent use.
type Env struct {
	env   *cel.Env
	cache sync.Map // source -> *Program, so an expression is compiled once
}

var (
	guardOnce sync.Once
	guardEnv  *Env
	guardErr  error
)

// LifecycleGuard is the environment for a lifecycle transition's `when`: record (as it would be after the move),
// old (as it is now), user and now.
func LifecycleGuard() (*Env, error) {
	guardOnce.Do(func() {
		var e *cel.Env
		e, guardErr = cel.NewEnv(
			ext.NativeTypes(reflect.TypeOf(&Record{}), reflect.TypeOf(&User{}), ext.ParseStructTags(true)),
			cel.Variable("record", cel.ObjectType("expr.Record")),
			cel.Variable("old", cel.ObjectType("expr.Record")),
			cel.Variable("user", cel.ObjectType("expr.User")),
			cel.Variable("now", cel.TimestampType),
		)
		guardEnv = &Env{env: e}
	})
	return guardEnv, guardErr
}

var (
	ruleOnce sync.Once
	ruleEnv  *Env
	ruleErr  error
)

// Rule is the environment for a rule's `when`, its per-action `when`, and the values its actions compute. It is the
// guard environment plus `event`. On create, `old` is an empty record, so use event == "create" to tell.
func Rule() (*Env, error) {
	ruleOnce.Do(func() {
		var e *cel.Env
		e, ruleErr = cel.NewEnv(
			ext.NativeTypes(reflect.TypeOf(&Record{}), reflect.TypeOf(&User{}), ext.ParseStructTags(true)),
			cel.Variable("record", cel.ObjectType("expr.Record")),
			cel.Variable("old", cel.ObjectType("expr.Record")),
			cel.Variable("user", cel.ObjectType("expr.User")),
			cel.Variable("now", cel.TimestampType),
			cel.Variable("event", cel.StringType),
		)
		ruleEnv = &Env{env: e}
	})
	return ruleEnv, ruleErr
}

var (
	wfOnce sync.Once
	wfEnv  *Env
	wfErr  error
)

// Workflow is the environment for a workflow step's expressions: the record as it is now, the person the workflow
// runs as, the time, and the workflow's own variables (`vars.name`, of no fixed type: a variable is whatever a `set`
// step stored in it). group('name') is the ids of a group's members, so `approvers: group('CAB')` asks a team, and
// role('name') the ids of everyone holding a role.
func Workflow() (*Env, error) {
	wfOnce.Do(func() {
		var e *cel.Env
		e, wfErr = cel.NewEnv(
			ext.NativeTypes(reflect.TypeOf(&Record{}), reflect.TypeOf(&User{}), ext.ParseStructTags(true)),
			cel.Variable("record", cel.ObjectType("expr.Record")),
			cel.Variable("user", cel.ObjectType("expr.User")),
			cel.Variable("now", cel.TimestampType),
			cel.Variable("vars", cel.MapType(cel.StringType, cel.DynType)),
			cel.Variable(lookups[0].variable, cel.MapType(cel.StringType, cel.ListType(cel.StringType))),
			cel.Variable(lookups[1].variable, cel.MapType(cel.StringType, cel.ListType(cel.StringType))),
			cel.Macros(lookups[0].macro(), lookups[1].macro()),
		)
		wfEnv = &Env{env: e}
	})
	return wfEnv, wfErr
}

// Program is a compiled, checked expression.
type Program struct {
	prg    cel.Program
	Source string
	names  map[string][]string // per lookup variable, the names written in the expression, looked up before each run
}

// Compile parses and type-checks a boolean expression. The error is one readable sentence with a position.
func (e *Env) Compile(source string) (*Program, error) { return e.CompileAs(source, cel.BoolType) }

// Types an expression may be required to produce.
var (
	Bool       = cel.BoolType
	Int        = cel.IntType
	String     = cel.StringType
	StringList = cel.ListType(cel.StringType)
)

func describe(t *cel.Type) string {
	switch {
	case t.IsExactType(cel.BoolType):
		return "true or false"
	case t.IsExactType(cel.IntType):
		return "a whole number"
	case t.IsExactType(cel.StringType):
		return "text"
	case t.IsExactType(StringList):
		return "a list of text"
	}
	return t.String()
}

// CompileAs is Compile for an expression that must produce a particular type: a rule's `set` value is a number or
// text, not a condition.
func (e *Env) CompileAs(source string, want *cel.Type) (*Program, error) {
	return e.compile(source, want)
}

// CompileAny is CompileAs for an expression whose result may be of any type: a workflow variable holds whatever it
// is given.
func (e *Env) CompileAny(source string) (*Program, error) { return e.compile(source, nil) }

func (e *Env) compile(source string, want *cel.Type) (*Program, error) {
	key := "any|" + source
	if want != nil {
		key = want.String() + "|" + source
	}
	if p, ok := e.cache.Load(key); ok {
		return p.(*Program), nil
	}
	src := strings.TrimSpace(source)
	switch {
	case src == "":
		return nil, errors.New("the expression is empty")
	case len(src) > MaxSourceLength:
		return nil, fmt.Errorf("the expression is %d characters; the limit is %d", len(src), MaxSourceLength)
	}
	parsed, iss := e.env.Parse(src)
	if err := issues(iss); err != nil {
		return nil, err
	}
	// Look for null before type-checking: the checker would reject `record.priority == null` with an obscure
	// overload error, and for some types it would accept it and the comparison would never match.
	if col, found := nullLiteral(parsed); found {
		return nil, fmt.Errorf("column %d: do not compare with null, use has(record.field) to test whether a field is set", col)
	}
	a, iss := e.env.Check(parsed)
	if err := issues(iss); err != nil {
		return nil, err
	}
	if want != nil && !a.OutputType().IsExactType(want) {
		return nil, fmt.Errorf("the expression must produce %s, not %s", describe(want), a.OutputType())
	}
	names := map[string][]string{}
	for _, l := range lookups {
		n, err := l.names(a)
		if err != nil {
			return nil, err
		}
		if len(n) > 0 {
			names[l.variable] = n
		}
	}
	prg, err := e.env.Program(a, cel.EvalOptions(cel.OptTrackCost), cel.CostLimit(costLimit))
	if err != nil {
		return nil, err
	}
	p := &Program{prg: prg, Source: src, names: names}
	e.cache.Store(key, p)
	return p, nil
}

// issues turns CEL's multi-line diagnostics into one line: the first problem and where it is.
func issues(iss *cel.Issues) error {
	if iss == nil || iss.Err() == nil {
		return nil
	}
	errs := iss.Errors()
	if len(errs) == 0 {
		return iss.Err()
	}
	first := errs[0]
	msg := strings.TrimSpace(strings.SplitN(first.Message, "\n", 2)[0])
	return fmt.Errorf("column %d: %s", first.Location.Column()+1, msg)
}

// nullLiteral finds a `null` written in the expression.
func nullLiteral(a *cel.Ast) (column int, found bool) {
	root := a.NativeRep()
	ast.PreOrderVisit(root.Expr(), ast.NewExprVisitor(func(e ast.Expr) {
		if !found && e.Kind() == ast.LiteralKind && e.AsLiteral() == types.NullValue {
			found = true
			column = root.SourceInfo().GetStartLocation(e.ID()).Column() + 1
		}
	}))
	return column, found
}

// Eval runs a compiled expression. An error means it could not be decided (for example the cost limit was hit or
// a value was missing); callers must treat that as "no", never as "yes".
func (p *Program) Eval(ctx context.Context, in Input) (bool, error) {
	v, err := p.EvalValue(ctx, in)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("expression produced %T, not a boolean", v)
	}
	return b, nil
}

func (p *Program) eval(ctx context.Context, in Input) (ref.Val, error) {
	rec, old, user := in.Record, in.Old, in.User
	if rec == nil {
		rec = &Record{}
	}
	if old == nil {
		old = &Record{}
	}
	if user == nil {
		user = &User{}
	}
	vars := in.Vars
	if vars == nil {
		vars = map[string]any{}
	}
	vals := map[string]any{"record": rec, "old": old, "user": user, "now": in.Now, "event": in.Event, "vars": vars}
	resolvers := map[string]func(context.Context, string) ([]string, error){lookups[0].variable: in.Groups, lookups[1].variable: in.Roles}
	for _, l := range lookups {
		found := map[string][]string{}
		if names := p.names[l.variable]; len(names) > 0 {
			resolve := resolvers[l.variable]
			if resolve == nil {
				return nil, errors.New(l.fn + "() is not available here")
			}
			for _, name := range names {
				ids, err := resolve(ctx, name)
				if err != nil {
					return nil, err
				}
				found[name] = ids
			}
		}
		vals[l.variable] = found
	}
	out, _, err := p.prg.ContextEval(ctx, vals)
	return out, err
}

// EvalValue runs an expression compiled with CompileAs and returns its value: a bool, an int64 or a string.
func (p *Program) EvalValue(ctx context.Context, in Input) (any, error) {
	out, err := p.eval(ctx, in)
	if err != nil {
		return nil, err
	}
	return out.Value(), nil
}

// EvalStringList runs an expression compiled with CompileAs(..., StringList) and returns its value as a []string.
func (p *Program) EvalStringList(ctx context.Context, in Input) ([]string, error) {
	out, err := p.eval(ctx, in)
	if err != nil {
		return nil, err
	}
	native, err := out.ConvertToNative(reflect.TypeOf([]string{}))
	if err != nil {
		return nil, err
	}
	list, ok := native.([]string)
	if !ok {
		return nil, fmt.Errorf("expression produced %T, not a list of text", native)
	}
	return list, nil
}
