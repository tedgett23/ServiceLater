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
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
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
	ID            string    `cel:"id"`
	Type          string    `cel:"type"`
	Number        int64     `cel:"number"`
	Reference     string    `cel:"reference"`
	Title         string    `cel:"title"`
	Description   string    `cel:"description"`
	Status        string    `cel:"status"`
	StateCategory string    `cel:"state_category"`
	Priority      int64     `cel:"priority"`
	RequesterID   string    `cel:"requester_id"`
	RequesterName string    `cel:"requester_name"`
	AssigneeID    *string   `cel:"assignee_id"`
	AssigneeName  *string   `cel:"assignee_name"`
	CreatedAt     time.Time `cel:"created_at"`
	UpdatedAt     time.Time `cel:"updated_at"`
	// Resolution is an incident's own column. It reads as empty for types that have none, so a guard written for
	// `task` never fails to evaluate: the record is flat, whichever table a field lives in.
	Resolution string `cel:"resolution"`
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

// Program is a compiled, checked expression.
type Program struct {
	prg    cel.Program
	Source string
}

// Compile parses and type-checks a boolean expression. The error is one readable sentence with a position.
func (e *Env) Compile(source string) (*Program, error) { return e.CompileAs(source, cel.BoolType) }

// Types an expression may be required to produce.
var (
	Bool   = cel.BoolType
	Int    = cel.IntType
	String = cel.StringType
)

func describe(t *cel.Type) string {
	switch {
	case t.IsExactType(cel.BoolType):
		return "true or false"
	case t.IsExactType(cel.IntType):
		return "a whole number"
	case t.IsExactType(cel.StringType):
		return "text"
	}
	return t.String()
}

// CompileAs is Compile for an expression that must produce a particular type: a rule's `set` value is a number or
// text, not a condition.
func (e *Env) CompileAs(source string, want *cel.Type) (*Program, error) {
	key := want.String() + "|" + source
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
	if !a.OutputType().IsExactType(want) {
		return nil, fmt.Errorf("the expression must produce %s, not %s", describe(want), a.OutputType())
	}
	prg, err := e.env.Program(a, cel.EvalOptions(cel.OptTrackCost), cel.CostLimit(costLimit))
	if err != nil {
		return nil, err
	}
	p := &Program{prg: prg, Source: src}
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

// EvalValue runs an expression compiled with CompileAs and returns its value: a bool, an int64 or a string.
func (p *Program) EvalValue(ctx context.Context, in Input) (any, error) {
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
	out, _, err := p.prg.ContextEval(ctx, map[string]any{"record": rec, "old": old, "user": user, "now": in.Now, "event": in.Event})
	if err != nil {
		return nil, err
	}
	return out.Value(), nil
}
