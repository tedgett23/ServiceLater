// Package expr evaluates the expressions apps use for conditions (docs/apps-platform.md, §5). They are CEL:
// pure, always terminating, and type-checked when an app is installed, so a typo such as record.prioity is
// rejected at install time and not discovered in production.
//
// What an expression can see is fixed by the environment it is compiled in. Optional values are tested with
// has(record.assignee_id); comparing to null is rejected, because for some types it silently never matches.
package expr

import (
	"context"
	"encoding/json"
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
	"google.golang.org/protobuf/types/known/structpb"
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
	// Fields are the record's extra fields, the ones a form declares (docs/apps-platform.md §8), read as
	// record.fields.name. Values are as stored: text, a whole number (int64 or json.Number), true or false, and times
	// as RFC 3339 or YYYY-MM-DD text. What type each has in an expression is the FieldTypes it was compiled with.
	Fields map[string]any `cel:"fields"`
	// CIs are the configuration items linked to the record, by name, as the person saving may see them (none without
	// cmdb:read). Rules read them; linking or unlinking one is an update.
	CIs []CI `cel:"cis"`
	// ParentID is the record this one is part of (a task's request); unset for a record that has none.
	ParentID *string `cel:"parent_id"`
	// OpenTasks is how many tasks under the record are still open or waiting: a workflow waits until it is 0, and a
	// lifecycle can keep a request from being fulfilled before then.
	OpenTasks int64 `cel:"open_tasks"`
	// CatalogItem is the catalog item a request was ordered from (its name), empty otherwise.
	CatalogItem string `cel:"catalog_item"`
	// RequesterManagerID is the requester's manager (unset when they have none): the usual first approver.
	RequesterManagerID *string `cel:"requester_manager_id"`
	// ProblemID is the problem an incident is linked to, or a change fixes (unset when it has none, and for other types).
	ProblemID *string `cel:"problem_id"`
	// OpenIncidents is how many incidents linked to a problem are still open or waiting.
	OpenIncidents int64 `cel:"open_incidents"`
}

// CI is a configuration item linked to a record. The groups are unset when it has none, so test them with
// has(c.support_group).
type CI struct {
	ID     string `cel:"id"`
	Name   string `cel:"name"`
	Class  string `cel:"class"`
	Status string `cel:"status"`
	// SupportGroup is the name of the group that fixes it, which a rule can route the record to.
	SupportGroupID *string `cel:"support_group_id"`
	SupportGroup   *string `cel:"support_group"`
	OwnerGroupID   *string `cel:"owner_group_id"`
	OwnerGroup     *string `cel:"owner_group"`
}

// SetField sets an extra field on a working copy of a record, as a rule or a workflow step does: empty text clears
// it. The map is copied, so the record the copy was made from keeps its own.
func (r *Record) SetField(name string, v any) {
	next := make(map[string]any, len(r.Fields)+1)
	for k, x := range r.Fields {
		next[k] = x
	}
	if s, ok := v.(string); ok && s == "" {
		delete(next, name)
	} else {
		next[name] = v
	}
	r.Fields = next
}

// Kinds of extra field as expressions see them.
const (
	FieldText   = "text"   // a CEL string: text, long text and choices
	FieldNumber = "number" // a whole number
	FieldBool   = "bool"
	FieldTime   = "time" // a timestamp: dates are midnight UTC
)

// FieldTypes are the extra fields an expression may read as record.fields.name, and the kind of each. An expression
// compiled with none can read no extra field: record.fields.x is then "undefined field".
type FieldTypes map[string]string

// signature is a stable text form of the types, for caching an environment per set.
func (f FieldTypes) signature() string {
	names := make([]string, 0, len(f))
	for n := range f {
		names = append(names, n)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n + ":" + f[n] + ";")
	}
	return b.String()
}

func (f FieldTypes) celType(name string) (*types.Type, bool) {
	switch f[name] {
	case FieldText:
		return types.StringType, true
	case FieldNumber:
		return types.IntType, true
	case FieldBool:
		return types.BoolType, true
	case FieldTime:
		return types.TimestampType, true
	}
	return nil, false
}

// fieldsType is the object type record.fields has: one field per extra field.
const fieldsType = "expr.Fields"

// fieldsProvider declares record.fields as an object whose fields are the FieldTypes, so an expression is checked
// against them like any other field, and reads them from the record's map, converting stored values to their kind.
type fieldsProvider struct {
	types.Provider
	fields FieldTypes
}

func (p fieldsProvider) FindStructType(name string) (*types.Type, bool) {
	if name == fieldsType {
		return types.NewTypeTypeWithParam(types.NewObjectType(fieldsType)), true
	}
	return p.Provider.FindStructType(name)
}

func (p fieldsProvider) FindStructFieldNames(name string) ([]string, bool) {
	if name == fieldsType {
		names := make([]string, 0, len(p.fields))
		for n := range p.fields {
			names = append(names, n)
		}
		slices.Sort(names)
		return names, true
	}
	return p.Provider.FindStructFieldNames(name)
}

func (p fieldsProvider) FindStructFieldType(name, field string) (*types.FieldType, bool) {
	switch {
	case name == "expr.Record" && field == "fields":
		return &types.FieldType{
			Type:  types.NewObjectType(fieldsType),
			IsSet: func(any) bool { return true },
			GetFrom: func(obj any) (any, error) {
				if r, ok := obj.(*Record); ok {
					return fieldMap(r.Fields), nil
				}
				return nil, fmt.Errorf("fields of %T", obj)
			},
		}, true
	case name == fieldsType:
		t, ok := p.fields.celType(field)
		if !ok {
			return nil, false
		}
		kind := p.fields[field]
		return &types.FieldType{
			Type: t,
			IsSet: func(obj any) bool {
				v, ok := obj.(fieldMap)[field]
				return ok && v != nil
			},
			GetFrom: func(obj any) (any, error) {
				v, ok := obj.(fieldMap)[field]
				if !ok || v == nil {
					return nil, fmt.Errorf("record.fields.%s is not set: test it with has(record.fields.%s) first", field, field)
				}
				return FieldValue(kind, v)
			},
		}, true
	}
	return p.Provider.FindStructFieldType(name, field)
}

// fieldMap is a record's extra fields as the provider passes them between the steps of a select.
type fieldMap map[string]any

// FieldValue converts a stored extra field to the Go value of its kind: a string, an int64, a bool or a time.Time.
func FieldValue(kind string, v any) (any, error) {
	bad := func() (any, error) { return nil, fmt.Errorf("holds %v, which is not %s", v, kindName(kind)) }
	switch kind {
	case FieldText:
		if s, ok := v.(string); ok {
			return s, nil
		}
	case FieldNumber:
		switch n := v.(type) {
		case int64:
			return n, nil
		case int:
			return int64(n), nil
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return i, nil
			}
		case float64:
			if n == float64(int64(n)) {
				return int64(n), nil
			}
		}
	case FieldBool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
	case FieldTime:
		switch t := v.(type) {
		case time.Time:
			return t, nil
		case string:
			if at, err := time.Parse(time.RFC3339, t); err == nil {
				return at, nil
			}
			if at, err := time.Parse(time.DateOnly, t); err == nil {
				return at, nil
			}
		}
	}
	return bad()
}

func kindName(kind string) string {
	switch kind {
	case FieldNumber:
		return "a whole number"
	case FieldBool:
		return "true or false"
	case FieldTime:
		return "a time"
	}
	return "text"
}

// Comment is what a rule sees as `comment` when its event is comment: the comment just added. For other events it is
// empty (event == "comment" tells).
type Comment struct {
	ID       string `cel:"id"`
	Body     string `cel:"body"`
	Internal bool   `cel:"internal"`
	AuthorID string `cel:"author_id"`
	// Mentions are the ids of the people it @mentions, each of whom may read it.
	Mentions []string `cel:"mentions"`
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
	// Event is what is happening to the record (create, update or comment). Only rules see it; guards have no event.
	Event string
	// Comment is the comment a comment event is about. Only rules see it.
	Comment *Comment
	// Vars are a workflow's variables. Only workflows see them.
	Vars map[string]any
	// Groups looks up the ids of a group's members by the group's name, for group('name'). Only workflows can use
	// group(); an expression that does is refused if this is nil. A missing group is an error, never an empty list.
	Groups func(ctx context.Context, name string) ([]string, error)
	// Roles looks up the ids of everyone who holds a role, for role('name'), the same way.
	Roles func(ctx context.Context, name string) ([]string, error)
	// Lookups are the counts a rule's or a guard's declared lookups found, read as lookups.name. Only rules and guards
	// see them. A name an expression uses that is missing here makes the evaluation fail, so it can never pass.
	Lookups map[string]int64
	// Count runs one lookup: how many records of a kind match every filter, under the acting person's RLS. The caller
	// supplies it, because expressions do no database work; where it is nil, lookups are not available.
	Count func(ctx context.Context, target string, filters []Filter) (int64, error)
	// Records reads other records for a script action, under the acting person's RLS; nil where scripts cannot.
	Records RecordReader
}

// RecordReader reads records other than the one a rule or step runs for, as whoever it runs as (scripts use it).
type RecordReader interface {
	// Children are the records under one (a request's tasks), open ones only when asked.
	Children(ctx context.Context, parentID string, open bool) ([]*Record, error)
	// Get finds a record by reference (INC42) or id; nil when there is none the reader may see.
	Get(ctx context.Context, ref string) (*Record, error)
}

// Filter is one condition of a lookup: field op value, where value is text, a time, or (for in) a list of text.
type Filter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

// lookupsVar is the variable a rule or guard reads its lookups' counts from: lookups.name.
const lookupsVar = "lookups"

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

// The environments an expression can be compiled in.
const (
	kindGuard    = "guard"
	kindRule     = "rule"
	kindWorkflow = "workflow"
)

type envEntry struct {
	once sync.Once
	env  *Env
	err  error
}

// envs holds one environment per kind and set of extra fields, built when first asked for.
var envs sync.Map // kind|signature -> *envEntry

func envFor(kind string, fields FieldTypes) (*Env, error) {
	v, _ := envs.LoadOrStore(kind+"|"+fields.signature(), &envEntry{})
	e := v.(*envEntry)
	e.once.Do(func() { e.env, e.err = buildEnv(kind, fields) })
	return e.env, e.err
}

func buildEnv(kind string, fields FieldTypes) (*Env, error) {
	base, err := cel.NewEnv(ext.NativeTypes(reflect.TypeOf(&Record{}), reflect.TypeOf(&CI{}), reflect.TypeOf(&User{}), reflect.TypeOf(&Comment{}), ext.ParseStructTags(true)))
	if err != nil {
		return nil, err
	}
	opts := []cel.EnvOption{
		cel.CustomTypeProvider(fieldsProvider{Provider: base.CELTypeProvider(), fields: fields}),
		cel.Variable("record", cel.ObjectType("expr.Record")),
		cel.Variable("user", cel.ObjectType("expr.User")),
		cel.Variable("now", cel.TimestampType),
	}
	switch kind {
	case kindGuard:
		opts = append(opts,
			cel.Variable("old", cel.ObjectType("expr.Record")),
			cel.Variable(lookupsVar, cel.MapType(cel.StringType, cel.IntType)))
	case kindRule:
		opts = append(opts,
			cel.Variable("old", cel.ObjectType("expr.Record")),
			cel.Variable("event", cel.StringType),
			cel.Variable("comment", cel.ObjectType("expr.Comment")),
			cel.Variable(lookupsVar, cel.MapType(cel.StringType, cel.IntType)))
	case kindWorkflow:
		opts = append(opts,
			cel.Variable("vars", cel.MapType(cel.StringType, cel.DynType)),
			cel.Variable(lookups[0].variable, cel.MapType(cel.StringType, cel.ListType(cel.StringType))),
			cel.Variable(lookups[1].variable, cel.MapType(cel.StringType, cel.ListType(cel.StringType))),
			cel.Macros(lookups[0].macro(), lookups[1].macro()))
	}
	e, err := base.Extend(opts...)
	if err != nil {
		return nil, err
	}
	return &Env{env: e}, nil
}

// LifecycleGuard is the environment for a lifecycle transition's `when`: record (as it would be after the move),
// old (as it is now), user, now, and the counts of the transition's lookups (lookups.name).
func LifecycleGuard() (*Env, error) { return LifecycleGuardFor(nil) }

// LifecycleGuardFor is LifecycleGuard where record.fields has these extra fields.
func LifecycleGuardFor(fields FieldTypes) (*Env, error) { return envFor(kindGuard, fields) }

// Rule is the environment for a rule's `when`, its per-action `when`, and the values its actions compute. It is the
// guard environment plus `event`; lookups are the rule's own. On create, `old` is an empty record, so use event == "create" to tell.
// Form conditions are compiled here too.
func Rule() (*Env, error) { return RuleFor(nil) }

// RuleFor is Rule where record.fields has these extra fields.
func RuleFor(fields FieldTypes) (*Env, error) { return envFor(kindRule, fields) }

// Workflow is the environment for a workflow step's expressions: the record as it is now, the person the workflow
// runs as, the time, and the workflow's own variables (`vars.name`, of no fixed type: a variable is whatever a `set`
// step stored in it). group('name') is the ids of a group's members, so `approvers: group('CAB')` asks a team, and
// role('name') the ids of everyone holding a role.
func Workflow() (*Env, error) { return WorkflowFor(nil) }

// WorkflowFor is Workflow where record.fields has these extra fields.
func WorkflowFor(fields FieldTypes) (*Env, error) { return envFor(kindWorkflow, fields) }

// Program is a compiled, checked expression.
type Program struct {
	prg    cel.Program
	Source string
	names  map[string][]string // per lookup variable, the names written in the expression, looked up before each run
	counts []string            // the lookups (lookups.name) the expression reads
	reads  []string            // the record's fields it reads: record.title is "title", record.fields.x is "fields.x"
}

// Reads lists the fields of `record` the expression reads, extra fields as "fields.name". A form asks again for its
// conditions only when one of these changes.
func (p *Program) Reads() []string { return p.reads }

// Lookups lists the lookups the expression reads, so a definition can check that it declares each one.
func (p *Program) Lookups() []string { return p.counts }

// Compile parses and type-checks a boolean expression. The error is one readable sentence with a position.
func (e *Env) Compile(source string) (*Program, error) { return e.CompileAs(source, cel.BoolType) }

// Types an expression may be required to produce.
var (
	Bool       = cel.BoolType
	Int        = cel.IntType
	String     = cel.StringType
	StringList = cel.ListType(cel.StringType)
	Duration   = cel.DurationType
	Timestamp  = cel.TimestampType
)

// KindType is the type an expression setting an extra field of this kind must produce.
func KindType(kind string) *cel.Type {
	switch kind {
	case FieldNumber:
		return Int
	case FieldBool:
		return Bool
	case FieldTime:
		return Timestamp
	}
	return String
}

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
	case t.IsExactType(cel.DurationType):
		return `a duration, such as duration("4h")`
	case t.IsExactType(cel.TimestampType):
		return `a time, such as now + duration("24h")`
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
		if strings.Contains(err.Error(), "undefined field") && strings.Contains(src, "fields.") {
			return nil, fmt.Errorf("%w (record.fields has only the fields this app's form for the record type declares)", err)
		}
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
	counts, err := countNames(a)
	if err != nil {
		return nil, err
	}
	prg, err := e.env.Program(a, cel.EvalOptions(cel.OptTrackCost), cel.CostLimit(costLimit))
	if err != nil {
		return nil, err
	}
	p := &Program{prg: prg, Source: src, names: names, counts: counts, reads: recordReads(a)}
	e.cache.Store(key, p)
	return p, nil
}

// countNames lists the lookups an expression reads. They must be written lookups.name (or lookups['name']), so the
// list is complete and a definition can check each is declared.
func countNames(a *cel.Ast) ([]string, error) {
	var names []string
	var bad bool
	seen := map[int64]bool{}
	add := func(n string) {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	ast.PreOrderVisit(a.NativeRep().Expr(), ast.NewExprVisitor(func(e ast.Expr) {
		switch e.Kind() {
		case ast.SelectKind:
			if op := e.AsSelect().Operand(); op.Kind() == ast.IdentKind && op.AsIdent() == lookupsVar {
				seen[op.ID()] = true
				add(e.AsSelect().FieldName())
			}
		case ast.CallKind:
			c := e.AsCall()
			if c.FunctionName() == operators.Index && len(c.Args()) == 2 && c.Args()[0].Kind() == ast.IdentKind && c.Args()[0].AsIdent() == lookupsVar {
				seen[c.Args()[0].ID()] = true
				if lit := c.Args()[1]; lit.Kind() == ast.LiteralKind {
					if s, ok := lit.AsLiteral().Value().(string); ok {
						add(s)
						return
					}
				}
				bad = true
			}
		case ast.IdentKind:
			if e.AsIdent() == lookupsVar && !seen[e.ID()] {
				bad = true
			}
		}
	}))
	if bad {
		return nil, errors.New("read a lookup as lookups.name")
	}
	return names, nil
}

// recordReads lists the fields of record an expression selects: record.x as "x", record.fields.x as "fields.x".
func recordReads(a *cel.Ast) []string {
	var out []string
	add := func(n string) {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	ast.PreOrderVisit(a.NativeRep().Expr(), ast.NewExprVisitor(func(e ast.Expr) {
		if e.Kind() != ast.SelectKind {
			return
		}
		sel := e.AsSelect()
		op := sel.Operand()
		switch {
		case op.Kind() == ast.IdentKind && op.AsIdent() == "record" && sel.FieldName() != "fields":
			add(sel.FieldName())
		case op.Kind() == ast.SelectKind && op.AsSelect().FieldName() == "fields" &&
			op.AsSelect().Operand().Kind() == ast.IdentKind && op.AsSelect().Operand().AsIdent() == "record":
			add("fields." + sel.FieldName())
		}
	}))
	slices.Sort(out)
	return out
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
	counts := in.Lookups
	if counts == nil {
		counts = map[string]int64{}
	}
	comment := in.Comment
	if comment == nil {
		comment = &Comment{Mentions: []string{}}
	}
	vals := map[string]any{"record": rec, "old": old, "user": user, "now": in.Now, "event": in.Event, "comment": comment, "vars": vars, lookupsVar: counts}
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

// EvalValue runs an expression compiled with CompileAs and returns its value: a bool, an int64, a string or a
// time.Duration.
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

// EvalJSON runs an expression and returns its value as JSON holds it: text, numbers (whole numbers as int64), true or
// false, null, lists and maps of them; a time is RFC 3339 text. For values that become variables or request bodies.
func (p *Program) EvalJSON(ctx context.Context, in Input) (any, error) {
	out, err := p.eval(ctx, in)
	if err != nil {
		return nil, err
	}
	switch v := out.Value().(type) {
	case time.Time:
		return v.UTC().Format(time.RFC3339), nil
	case string, bool, int64, float64, nil:
		return v, nil
	case uint64:
		return int64(v), nil //nolint:gosec // an expression's whole number
	}
	native, err := out.ConvertToNative(reflect.TypeOf(&structpb.Value{}))
	if err != nil {
		return nil, fmt.Errorf("the value cannot be kept: %w", err)
	}
	return wholeNumbers(native.(*structpb.Value).AsInterface()), nil
}

// wholeNumbers turns JSON's floats that are whole back into int64, as definitions keep integers.
func wholeNumbers(x any) any {
	switch v := x.(type) {
	case float64:
		if v == float64(int64(v)) {
			return int64(v)
		}
	case []any:
		for i := range v {
			v[i] = wholeNumbers(v[i])
		}
	case map[string]any:
		for k := range v {
			v[k] = wholeNumbers(v[k])
		}
	}
	return x
}
