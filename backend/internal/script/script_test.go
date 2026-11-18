package script_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/script"
)

type fakeReader map[string][]*expr.Record

func (f fakeReader) Children(_ context.Context, parent string, open bool) ([]*expr.Record, error) {
	var out []*expr.Record
	for _, r := range f[parent] {
		if !open || r.StateCategory == "open" {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f fakeReader) Get(_ context.Context, ref string) (*expr.Record, error) {
	for _, list := range f {
		for _, r := range list {
			if r.Reference == ref || r.ID == ref {
				return r, nil
			}
		}
	}
	return nil, nil
}

func run(t *testing.T, src string, env script.Env) (*script.Result, error) {
	t.Helper()
	p, err := script.Compile(src, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return p.Run(context.Background(), env)
}

var request = &expr.Record{ID: "r1", Reference: "REQ1", Title: "New laptop", Status: "open", Fields: map[string]any{"model": "x1"}}

// A script loops over records and asks for changes; nothing is changed by the script itself.
func TestAScriptAsksForChanges(t *testing.T) {
	reader := fakeReader{"r1": {
		{ID: "t1", Reference: "TASK1", StateCategory: "open"},
		{ID: "t2", Reference: "TASK2", StateCategory: "done"},
		{ID: "t3", Reference: "TASK3", StateCategory: "open"},
	}}
	res, err := run(t, `
closed = []
for task in children(record, open=True):
    update(task, {"status": "cancelled", "priority": 2}, comment="The request was withdrawn.")
    closed.append(task.reference)
print("closed", len(closed), "for", record.fields["model"])
if not closed:
    return {"next": "nothing_open"}
return {"vars": {"closed": closed}, "next": "done"}
`, script.Env{Record: request, Reader: reader, Allow: script.Allow{Others: true, Comments: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updates) != 2 || res.Updates[0].Record != "t1" || res.Updates[0].Status != "cancelled" ||
		res.Updates[0].Fields["priority"] != int64(2) || res.Updates[1].Comment != "The request was withdrawn." {
		t.Errorf("updates: %+v", res.Updates)
	}
	if res.Next != "done" || len(res.Vars["closed"].([]any)) != 2 || res.Printed[0] != "closed 2 for x1" {
		t.Errorf("result: %+v", res)
	}
}

// Mistakes are found when the script is compiled, with the script's own line numbers.
func TestMistakesAreFoundWhenCompiling(t *testing.T) {
	for src, want := range map[string]string{
		"x = 1\nupate(record, {})":      "line 2: undefined: upate",
		"for x in [1]\n    pass":        "want ':'",
		"load('a.star', 'b')":           "this app has no script file a.star",
		"":                              "the script is empty",
		strings.Repeat("x = 1\n", 5000): "longer than",
	} {
		_, err := script.Compile(src, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.30q: want %q, got %v", src, want, err)
		}
	}
}

// A run is bounded: endless loops and slow scripts stop with a reason; errors say on which line.
func TestRunsAreBoundedAndErrorsHaveLines(t *testing.T) {
	_, err := run(t, "while True:\n    pass", script.Env{Limits: script.Limits{Steps: 10_000, Time: time.Second}})
	if err == nil || !strings.Contains(err.Error(), "more than 10000 steps") {
		t.Errorf("an endless loop: %v", err)
	}
	_, err = run(t, "x = 1\ny = x + 'a'", script.Env{})
	var se *script.Error
	if !errors.As(err, &se) || se.Line != 2 {
		t.Errorf("a runtime error: %#v", err)
	}
	_, err = run(t, "abort('Not on a Friday')", script.Env{Allow: script.Allow{Abort: true}})
	var ab *script.Abort
	if !errors.As(err, &ab) || ab.Message != "Not on a Friday" {
		t.Errorf("abort: %#v", err)
	}
}

// What a script may ask for depends on where it runs: a before rule changes only its own record.
func TestWhatAScriptMayDoDependsOnWhereItRuns(t *testing.T) {
	before := script.Env{Record: request, Reader: fakeReader{}, Allow: script.Allow{Own: true, Abort: true}}
	res, err := run(t, `update(record, {"title": record.title.upper()})`, before)
	if err != nil || res.Updates[0].Fields["title"] != "NEW LAPTOP" {
		t.Errorf("own record: %v %+v", err, res)
	}
	for src, want := range map[string]string{
		`comment(record, "hi")`:                          "comment() is not available here",
		`update("TASK9", {"title": "x"})`:                "there is no record TASK9",
		`abort("x") if False else update({"id": 1}, {})`: "give a record",
	} {
		if _, err := run(t, src, before); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", src, want, err)
		}
	}
	if _, err := run(t, `update("t1", {"title": "x"})`, script.Env{Record: request, Reader: fakeReader{"r1": {{ID: "t1", Reference: "TASK1"}}}, Allow: script.Allow{Own: true}}); err == nil ||
		!strings.Contains(err.Error(), "only change the record this runs for") {
		t.Errorf("another record from a before rule: %v", err)
	}
	if _, err := run(t, `return {"vars": 1}`, script.Env{}); err == nil || !strings.Contains(err.Error(), "vars must be a dict") {
		t.Errorf("a bad result: %v", err)
	}
}

// count() runs a lookup through the caller, as rules' lookups do.
func TestCount(t *testing.T) {
	var got []expr.Filter
	env := script.Env{Count: func(_ context.Context, target string, f []expr.Filter) (int64, error) {
		got = f
		return 3, nil
	}}
	res, err := run(t, `return {"vars": {"n": count("tasks", {"status": "open", "priority": {"lte": 2}})}}`, env)
	if err != nil || res.Vars["n"] != int64(3) || len(got) != 2 {
		t.Errorf("count: %v %+v %+v", err, res, got)
	}
}

var appFiles = map[string]string{
	"requests.star": `load("text.star", "sentence")

def cancel_open(record, reason):
    """Cancel the open records under one, each with a note."""
    done = []
    for task in children(record, open=True):
        update(task, {"status": "cancelled"}, comment=sentence(reason))
        done.append(task.reference)
    return done

def _private():
    pass

def broken(record):
    return record.title + 1
`,
	"text.star": `def sentence(s):
    return s[0].upper() + s[1:] + "."
`,
	"loop_a.star": `load("loop_b.star", "b")
def a():
    pass
`,
	"loop_b.star": `load("loop_a.star", "a")
def b():
    pass
`,
}

// Functions in the app's script files are loaded by an inline script and called from it; their changes join the run's.
func TestScriptFilesAreLoadedAndCalled(t *testing.T) {
	p, err := script.Compile("load(\"requests.star\", \"cancel_open\")\n\ndone = cancel_open(record, \"the request was withdrawn\")\nreturn {\"vars\": {\"done\": done}}", appFiles)
	if err != nil {
		t.Fatal(err)
	}
	reader := fakeReader{"r1": {{ID: "t1", Reference: "TASK1", StateCategory: "open"}, {ID: "t2", Reference: "TASK2", StateCategory: "done"}}}
	res, err := p.Run(context.Background(), script.Env{Record: request, Reader: reader, Allow: script.Allow{Others: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updates) != 1 || res.Updates[0].Comment != "The request was withdrawn." || len(res.Vars["done"].([]any)) != 1 {
		t.Errorf("result: %+v", res)
	}
	if got := script.Files("load(\"requests.star\", \"cancel_open\")\nx = 1", appFiles); strings.Join(got, ",") != "requests.star,text.star" {
		t.Errorf("files it needs: %v", got)
	}

	// A mistake inside a file says both where the script called it and where in the file it went wrong.
	p, err = script.Compile("load(\"requests.star\", \"broken\")\nbroken(record)", appFiles)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Run(context.Background(), script.Env{Record: request})
	var se *script.Error
	if !errors.As(err, &se) || se.Line != 2 || se.File != "requests.star" || se.FileLine != 15 {
		t.Errorf("an error in a file: %#v", err)
	}
}

// Loads are checked when the script compiles: only this app's files, only names they define, and no loops.
func TestLoadsAreChecked(t *testing.T) {
	for src, want := range map[string]string{
		`load("nope.star", "x")`:                       "this app has no script file nope.star",
		`load("requests.star", "cancel_all")`:          "requests.star defines no cancel_all",
		`load("@other//requests.star", "x")`:           "only this app's script files can be loaded",
		`load("loop_a.star", "a")`:                     "load each other in a loop",
		"x = 1\nload(\"requests.star\", \"_private\")": "load",
	} {
		_, err := script.Compile(src, appFiles)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", src, want, err)
		}
	}
	if err := script.CheckFile("Bad.star", "x = 1"); err == nil || !strings.Contains(err.Error(), "not a script file name") {
		t.Errorf("a bad file name: %v", err)
	}
	if err := script.CheckFile("ok.star", "def f(:\n    pass"); err == nil || !strings.Contains(err.Error(), "ok.star line 1") {
		t.Errorf("a file's syntax error: %v", err)
	}
}
