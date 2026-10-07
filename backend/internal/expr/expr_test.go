package expr

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"cel.dev/cel-go/cel"
)

func env(t testing.TB) *Env {
	t.Helper()
	e, err := LifecycleGuard()
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func str(s string) *string { return &s }

func sample() Input {
	return Input{
		Record: &Record{ID: "r1", Number: 42, Title: "VPN down", Status: "in_progress", StateCategory: "open", Priority: 2,
			RequesterID: "u-lena", RequesterName: "Lena", AssigneeID: str("u-priya"), AssigneeName: str("Priya"),
			CreatedAt: time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)},
		Old:  &Record{Status: "new", Priority: 2},
		User: &User{ID: "u-priya", Name: "Priya", Role: "agent"},
		Now:  time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
}

func eval(t *testing.T, source string, in Input) bool {
	t.Helper()
	p, err := env(t).Compile(source)
	if err != nil {
		t.Fatalf("%q: %v", source, err)
	}
	got, err := p.Eval(context.Background(), in)
	if err != nil {
		t.Fatalf("%q: %v", source, err)
	}
	return got
}

func TestEvaluates(t *testing.T) {
	in := sample()
	for _, c := range []struct {
		source string
		want   bool
	}{
		{`record.priority <= 2`, true},
		{`record.priority > 2`, false},
		{`record.status == "in_progress" && old.status == "new"`, true},
		{`record.title.size() > 0 && record.title.contains("VPN")`, true},
		{`user.role == "agent" && user.id == record.assignee_id`, true},
		{`user.role in ["admin", "agent"]`, true},
		{`record.state_category in ["open", "waiting"]`, true},
		{`now - record.created_at > duration("3h")`, true},
		{`now - record.created_at > duration("5h")`, false},
		{`record.number % 2 == 0`, true},
		{`has(record.assignee_id)`, true},
		{`has(record.assignee_id) && record.assignee_name == "Priya"`, true},
		{`record.requester_name.startsWith("Le")`, true},
		{`!(record.priority == 1) ? true : false`, true},
	} {
		if got := eval(t, c.source, in); got != c.want {
			t.Errorf("%s = %v, want %v", c.source, got, c.want)
		}
	}
}

// Optional values are tested with has(); an unset one is absent, and reading it is safe.
func TestOptionalFields(t *testing.T) {
	in := sample()
	in.Record.AssigneeID, in.Record.AssigneeName = nil, nil
	if eval(t, `has(record.assignee_id)`, in) {
		t.Error("an unset field must not be present")
	}
	if !eval(t, `!has(record.assignee_id)`, in) {
		t.Error("!has() on an unset field")
	}
	if eval(t, `has(record.assignee_id) && record.assignee_id == "u-priya"`, in) {
		t.Error("the guard pattern must be false for an unset field")
	}
	if !eval(t, `has(record.assignee_id) ? record.assignee_id == "x" : true`, in) {
		t.Error("ternary on has()")
	}
}

// Everything an author can get wrong must be caught when the expression is compiled, and say where.
func TestCompileRejectsMistakes(t *testing.T) {
	long := `record.title == "` + strings.Repeat("a", MaxSourceLength) + `"`
	for _, c := range []struct{ name, source, want string }{
		{"a typo in a field", `record.prioity <= 2`, "undefined field 'prioity'"},
		{"a typo inside has()", `has(record.assignee)`, "undefined field 'assignee'"},
		{"comparing a number with text", `record.priority == "high"`, "no matching overload"},
		{"an unknown variable", `ticket.priority <= 2`, "undeclared reference to 'ticket'"},
		{"an unknown function", `nope(record.priority)`, "undeclared reference to 'nope'"},
		{"not a boolean", `record.priority + 1`, "must produce true or false, not int"},
		{"text is not a boolean", `record.title`, "must produce true or false, not string"},
		{"broken syntax", `record.priority <=`, "Syntax error"},
		{"empty", "   ", "empty"},
		{"too long", long, "the limit is"},
		{"null comparison", `record.priority == null`, "do not compare with null, use has(record.field)"},
		{"null on an optional", `record.assignee_id != null`, "do not compare with null"},
		{"null on a timestamp", `record.created_at == null`, "do not compare with null"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := env(t).Compile(c.source)
			if err == nil {
				t.Fatalf("%q was accepted", c.source)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("errors must be one line so they read well in a report: %q", err)
			}
		})
	}
}

func TestErrorsSayWhere(t *testing.T) {
	_, err := env(t).Compile(`record.priority <= 2 && record.prioity > 1`)
	if err == nil || !strings.Contains(err.Error(), "column 31") {
		t.Errorf("want the column of the typo, got %v", err)
	}
	_, err = env(t).Compile(`has(record.assignee_id) || record.title == null`)
	if err == nil || !strings.Contains(err.Error(), "column 44") {
		t.Errorf("want the column of the null, got %v", err)
	}
}

func TestCompiledProgramsAreCached(t *testing.T) {
	a, _ := env(t).Compile(`record.priority <= 3`)
	b, _ := env(t).Compile(`record.priority <= 3`)
	if a != b {
		t.Error("the same expression was compiled twice")
	}
}

// The same compiled program is used by every request at once.
func TestConcurrentEvaluation(t *testing.T) {
	p, err := env(t).Compile(`record.priority <= 2 && has(record.assignee_id)`)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := sample()
			in.Record.Priority = int64(1 + i%4)
			got, err := p.Eval(context.Background(), in)
			if err != nil || got != (i%4 < 2) {
				t.Errorf("goroutine %d: got %v, %v", i, got, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestMissingInputsDoNotPanic(t *testing.T) {
	p, _ := env(t).Compile(`record.priority == 0 && user.role == "" && old.status == ""`)
	got, err := p.Eval(context.Background(), Input{})
	if err != nil || !got {
		t.Errorf("empty input: %v %v", got, err)
	}
}

// A guard runs on every transition, so a regression here is a regression in every save. This is a generous
// ceiling meant to catch something like compiling on each evaluation, not to measure.
func TestGuardEvaluationStaysCheap(t *testing.T) {
	p, err := env(t).Compile(`record.priority <= 3 && has(record.assignee_id) && user.role in ["admin", "agent"] && now - record.created_at > duration("1m")`)
	if err != nil {
		t.Fatal(err)
	}
	in := sample()
	const n = 5000
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, err := p.Eval(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	if per := time.Since(start) / n; per > 250*time.Microsecond {
		t.Errorf("one guard evaluation takes %v, want well under 250µs", per)
	}
}

func BenchmarkGuardEvaluation(b *testing.B) {
	p, err := env(b).Compile(`record.priority <= 3 && has(record.assignee_id) && user.role in ["admin", "agent"] && now - record.created_at > duration("1m")`)
	if err != nil {
		b.Fatal(err)
	}
	in := sample()
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := p.Eval(ctx, in); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompile(b *testing.B) {
	e := env(b)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		// A different literal every time, so the cache can never answer.
		if _, err := e.Compile(fmt.Sprintf("record.priority <= 3 && has(record.assignee_id) && record.number > %d", i)); err != nil {
			b.Fatal(err)
		}
	}
}

func rulesEnv(t testing.TB) *Env {
	t.Helper()
	e, err := Rule()
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRulesSeeTheEventAndGuardsDoNot(t *testing.T) {
	p, err := rulesEnv(t).Compile(`event == "create" || record.priority != old.priority`)
	if err != nil {
		t.Fatal(err)
	}
	in := sample()
	in.Event = "create"
	if got, err := p.Eval(context.Background(), in); err != nil || !got {
		t.Errorf("create: %v %v", got, err)
	}
	in.Event = "update"
	in.Old = &Record{Priority: 2}
	if got, _ := p.Eval(context.Background(), in); got {
		t.Error("an update that changes nothing")
	}
	if _, err := env(t).Compile(`event == "create"`); err == nil {
		t.Error("guards have no event: this should not compile there")
	}
}

func TestTypedExpressionsProduceTheRightThing(t *testing.T) {
	e := rulesEnv(t)
	in := sample()
	in.Event = "update"

	num, err := e.CompileAs(`record.priority > 2 ? 1 : 3`, Int)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := num.EvalValue(context.Background(), in); err != nil || v != int64(3) {
		t.Errorf("number: %v %v", v, err)
	}
	txt, err := e.CompileAs(`"[" + record.reference + "] " + record.title`, String)
	if err != nil {
		t.Fatal(err)
	}
	in.Record.Reference = "INC42"
	if v, _ := txt.EvalValue(context.Background(), in); v != "[INC42] VPN down" {
		t.Errorf("text: %v", v)
	}
	for _, c := range []struct {
		source string
		want   *cel.Type
		msg    string
	}{
		{`record.title`, Int, "a whole number, not string"},
		{`record.priority`, String, "text, not int"},
		{`record.priority`, Bool, "true or false, not int"},
		{`record.priority + 0.5`, Int, "no matching overload"},
	} {
		if _, err := e.CompileAs(c.source, c.want); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%q as %v: want %q, got %v", c.source, c.want, c.msg, err)
		}
	}
	// The same source compiled for two types is two programs: the cache must not confuse them.
	if _, err := e.Compile(`record.priority > 0`); err != nil {
		t.Error(err)
	}
	if _, err := e.CompileAs(`record.priority > 0`, Int); err == nil {
		t.Error("a boolean expression was accepted where a number is required")
	}
}

func TestWorkflowExpressionsSeeVarsAndTheRecord(t *testing.T) {
	env, err := Workflow()
	if err != nil {
		t.Fatal(err)
	}
	in := Input{Record: &Record{Priority: 2, Title: "Printer"}, User: &User{Name: "Ada"}, Vars: map[string]any{"level": int64(3), "tag": "urgent"}, Now: time.Now()}
	p, err := env.CompileAny(`vars.level + record.priority`)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := p.EvalValue(context.Background(), in); err != nil || v != int64(5) {
		t.Errorf("got %v, %v", v, err)
	}
	b, err := env.Compile(`vars.tag == "urgent" && record.title.startsWith("P")`)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := b.Eval(context.Background(), in); err != nil || !ok {
		t.Errorf("got %v, %v", ok, err)
	}
	// A variable that was never set is an evaluation error, not a silent false: the step fails, it does not guess.
	miss, _ := env.Compile(`vars.nothing == "x"`)
	if _, err := miss.Eval(context.Background(), in); err == nil {
		t.Error("a missing variable should be an error")
	}
	// Typos in the record are still caught at install time, even though vars have no fixed type.
	if _, err := env.Compile(`record.prioity == 1`); err == nil {
		t.Error("record typo accepted")
	}
	if _, err := env.CompileAny(`event`); err == nil {
		t.Error("`event` belongs to rules, not workflows")
	}
}

// group('name') resolves only the groups the expression names, just before it runs, and only in workflows.
func TestGroupResolvesNamedGroupsInWorkflows(t *testing.T) {
	wf, err := Workflow()
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	in := sample()
	in.Groups = func(_ context.Context, name string) ([]string, error) {
		asked = append(asked, name)
		switch name {
		case "CAB":
			return []string{"u-ann", "u-bob"}, nil
		case "Empty":
			return []string{}, nil
		}
		return nil, fmt.Errorf("there is no group named %q", name)
	}

	p, err := wf.CompileAs(`group('CAB') + group("Empty") + [record.requester_id]`, StringList)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.EvalStringList(context.Background(), in)
	if err != nil || strings.Join(got, ",") != "u-ann,u-bob,u-lena" {
		t.Errorf("got %v, %v", got, err)
	}
	if strings.Join(asked, ",") != "CAB,Empty" {
		t.Errorf("looked up %v", asked)
	}
	if ok, err := mustCompile(t, wf, `user.id in group('CAB')`).Eval(context.Background(), in); err != nil || ok {
		t.Errorf("priya is not in CAB: %v %v", ok, err)
	}

	// A group that does not exist is an error, never an empty list that asks nobody.
	if _, err := mustCompile(t, wf, `size(group('Ghosts')) == 0`).Eval(context.Background(), in); err == nil || !strings.Contains(err.Error(), "Ghosts") {
		t.Errorf("missing group: %v", err)
	}
	// Without a resolver (anything but a workflow step), it cannot run.
	in.Groups = nil
	if _, err := mustCompile(t, wf, `size(group('CAB')) > 0`).Eval(context.Background(), in); err == nil {
		t.Error("evaluated group() without a resolver")
	}

	for _, src := range []string{
		`size(group(record.title)) > 0`, // the name must be written in quotes
		`size(_groups) > 0`,             // the hidden variable is not a way around that
		`size(_groups[record.title]) > 0`,
	} {
		if _, err := wf.Compile(src); err == nil {
			t.Errorf("%q compiled", src)
		}
	}
	// Rules and guards have no group().
	if _, err := env(t).Compile(`size(group('CAB')) > 0`); err == nil {
		t.Error("a guard compiled group()")
	}
}

func mustCompile(t *testing.T, e *Env, src string) *Program {
	t.Helper()
	p, err := e.Compile(src)
	if err != nil {
		t.Fatalf("%q: %v", src, err)
	}
	return p
}
