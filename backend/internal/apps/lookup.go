package apps

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"cel.dev/cel-go/cel"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

// A lookup is a bounded, read-only count a rule or a guard declares (docs/apps-platform.md §5): how many records of
// one kind match every filter. It runs before the definition's expressions, under the acting person's RLS, and they
// read it as lookups.name. The menu is deliberately one shape: count, AND-only filters, on indexed fields.
//
//	lookups:
//	  freezes:
//	    count: freeze_window
//	    where:
//	      starts_at: { lt: =record.planned_end }
//	      ends_at:   { gt: =record.planned_start }
//	      applies_to: { in: =record.cis.map(c, c.id) }   # freezes for these items, or for everything
type Lookup struct {
	Count string `json:"count" yaml:"count"`
	// Where is field to operator to value. A field may take several operators, which is how a range is written.
	Where map[string]map[string]*Value `json:"where" yaml:"where"`

	filters []lookupFilter // Where in a fixed order, with each value compiled
	err     error          // why the lookup cannot run (only seen when a stored definition no longer validates)
}

type lookupFilter struct {
	field, op string
	value     *Value
}

// Lookups are a definition's lookups by name.
type Lookups map[string]*Lookup

const (
	maxLookups       = 5
	maxLookupFilters = 8
	// MaxLookupCount is where counting stops, so a lookup never scans more than this many rows; a count of this
	// many means "at least this many".
	MaxLookupCount = 1000
)

// Kinds of field a lookup can filter on, and so the value each filter takes.
const (
	lookupText = "text"
	lookupID   = "id"
	lookupTime = "time"
	// lookupScope is a freeze's items: applies_to in [item ids] keeps the freezes that list one of them or none.
	lookupScope = "scope"
)

// taskLookupFields are the fields every record type can be counted by: each has an index on tasks.
var taskLookupFields = map[string]string{
	"id": lookupID, "status": lookupText, "state_category": lookupText,
	"requester_id": lookupID, "assignee_id": lookupID, "assignment_group_id": lookupID,
}

// LookupTargets are what a lookup can count, and the fields each can be filtered by. The SQL function
// app_lookup_count (migration 00024) maps the same names to columns; TestLookupTargetsMatchTheDatabase keeps them
// in step.
var LookupTargets = func() map[string]map[string]string {
	out := map[string]map[string]string{
		"freeze_window": {"starts_at": lookupTime, "ends_at": lookupTime, "applies_to": lookupScope},
	}
	for _, t := range KnownRecordTypes {
		if t == "task" {
			continue
		}
		fields := map[string]string{}
		for f, k := range taskLookupFields {
			fields[f] = k
		}
		out[t] = fields
	}
	for f, k := range map[string]string{"change_type": lookupText, "planned_start": lookupTime, "planned_end": lookupTime} {
		out["change"][f] = k
	}
	return out
}()

// LookupOps are the operators a filter may use. in takes an =expression giving a list of text.
var LookupOps = []string{"eq", "ne", "lt", "lte", "gt", "gte", "in"}

var lookupNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validate checks the lookups' shape, compiles their values in env, and returns what is wrong, prefixed by where.
func (ls Lookups) validate(env *expr.Env, envErr error) (out []string) {
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if len(ls) > maxLookups {
		add("lookups: at most %d per definition, this has %d", maxLookups, len(ls))
	}
	for _, name := range sortedKeys(ls) {
		l := ls[name]
		at := "lookups." + name
		if !lookupNameRE.MatchString(name) {
			add("%s: a lookup's name must be lowercase letters, digits and underscores, starting with a letter", at)
		}
		if l == nil {
			add("%s: needs count and where", at)
			continue
		}
		before := len(out)
		l.filters, l.err = nil, nil
		fields, ok := LookupTargets[l.Count]
		if !ok {
			add("%s: count %q is not something a lookup can count (available: %s)", at, l.Count, strings.Join(sortedKeys(LookupTargets), ", "))
		}
		if len(l.Where) == 0 {
			add("%s: needs where: at least one filter, so it counts a few records and not all of them", at)
		}
		for _, field := range sortedKeys(l.Where) {
			kind, known := fields[field]
			if ok && !known {
				add("%s: %s cannot be filtered by %q (it can be filtered by: %s)", at, l.Count, field, strings.Join(sortedKeys(fields), ", "))
			}
			for _, op := range sortedKeys(l.Where[field]) {
				v := l.Where[field][op]
				where := fmt.Sprintf("%s: where.%s.%s", at, field, op)
				if !slices.Contains(LookupOps, op) {
					add("%s: %q is not an operator (available: %s)", where, op, strings.Join(LookupOps, ", "))
					continue
				}
				if v == nil {
					add("%s: needs a value", where)
					continue
				}
				l.filters = append(l.filters, lookupFilter{field: field, op: op, value: v})
				if known {
					if msg := v.compileFilter(env, envErr, kind, op); msg != "" {
						add("%s: %s", where, msg)
					}
				}
			}
		}
		if len(l.filters) > maxLookupFilters {
			add("%s: at most %d filters, this has %d", at, maxLookupFilters, len(l.filters))
		}
		// A lookup with anything wrong never runs: with a filter missing it would count more than it was written to.
		if len(out) > before {
			l.err = errors.New(out[before])
		}
	}
	return out
}

// compileFilter compiles a filter's value for a field of this kind and returns what is wrong with it, or "".
func (v *Value) compileFilter(env *expr.Env, envErr error, kind, op string) string {
	if kind == lookupScope && op != "in" {
		return "takes in, with an =expression giving item ids, such as =record.cis.map(c, c.id)"
	}
	want := expr.String
	switch {
	case op == "in":
		want = expr.StringList
	case kind == lookupTime:
		want = cel.TimestampType
	}
	if v.Expr == "" {
		switch {
		case op == "in":
			return "in takes an =expression giving a list of text, such as =['normal', 'emergency']"
		case kind == lookupTime:
			return "a time is written as an =expression, such as =now or =record.planned_start"
		}
		if _, ok := v.Literal.(string); !ok {
			return "takes text"
		}
		v.program, v.err = nil, nil
		return ""
	}
	v.compile(env, envErr, want)
	if v.err != nil {
		return v.err.Error()
	}
	if len(v.program.Lookups()) > 0 {
		v.program, v.err = nil, errors.New("a lookup's filter cannot read lookups: they are all counted before any expression runs")
		return v.err.Error()
	}
	return ""
}

// compile compiles every filter value again, for a definition read back from the database.
func (ls Lookups) compile(env *expr.Env, envErr error) {
	_ = ls.validate(env, envErr)
}

// undeclared lists the lookups p reads that ls does not declare.
func (ls Lookups) undeclared(p *expr.Program) []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, n := range p.Lookups() {
		if ls[n] == nil {
			out = append(out, n)
		}
	}
	return out
}

func undeclaredProblem(names []string) string {
	return fmt.Sprintf("reads %s, which is not declared under lookups", "lookups."+strings.Join(names, ", lookups."))
}

// Eval counts each lookup for in, evaluating the filters' values against it. Nil when there are none. Any error
// (a value that cannot be evaluated, no way to count here, the database) is returned, and the caller fails closed.
func (ls Lookups) Eval(ctx context.Context, in expr.Input) (map[string]int64, error) {
	if len(ls) == 0 {
		return nil, nil
	}
	if in.Count == nil {
		return nil, errors.New("lookups are not available here")
	}
	out := make(map[string]int64, len(ls))
	for _, name := range sortedKeys(ls) {
		l := ls[name]
		switch {
		case l == nil:
			return nil, fmt.Errorf("lookups.%s is empty", name)
		case l.err != nil:
			return nil, l.err
		}
		filters := make([]expr.Filter, 0, len(l.filters))
		for _, f := range l.filters {
			v, err := filterValue(ctx, f.value, f.op, in)
			if err != nil {
				return nil, fmt.Errorf("lookups.%s: where.%s.%s: %w", name, f.field, f.op, err)
			}
			filters = append(filters, expr.Filter{Field: f.field, Op: f.op, Value: v})
		}
		n, err := in.Count(ctx, l.Count, filters)
		if err != nil {
			return nil, fmt.Errorf("lookups.%s: %w", name, err)
		}
		out[name] = n
	}
	return out, nil
}

func filterValue(ctx context.Context, v *Value, op string, in expr.Input) (any, error) {
	p, err := v.Compiled()
	switch {
	case err != nil:
		return nil, err
	case p == nil:
		return v.Literal, nil
	case op == "in":
		return p.EvalStringList(ctx, in)
	}
	x, err := p.EvalValue(ctx, in)
	if err != nil {
		return nil, err
	}
	switch x.(type) {
	case time.Time, string:
		return x, nil
	}
	return nil, fmt.Errorf("produced %T, not text or a time", x)
}
