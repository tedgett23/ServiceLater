// Package script runs Starlark scripts: a script step in a workflow and a script action in a rule
// (docs/apps-platform.md §6.7). A script computes; it has no database, network, files or clock of its own. What it may
// read comes from the caller (the record, the workflow's vars, the person, a Reader for other records, lookups), and
// what it asks for (changes to records, comments) comes back as a Result for the caller to apply through the one save
// path, as whoever the script runs as. So a script can never do more than the rule or step around it could.
package script

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.starlark.net/lib/json"
	"go.starlark.net/lib/math"
	sltime "go.starlark.net/lib/time"
	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

// MaxSource is the longest script, in bytes.
const MaxSource = 20000

// Limits bound one run: how many Starlark steps it may take and for how long.
type Limits struct {
	Steps uint64
	Time  time.Duration
}

var (
	// BeforeLimits are for a before rule, which runs inside the save someone is waiting for.
	BeforeLimits = Limits{Steps: 200_000, Time: 100 * time.Millisecond}
	// RunLimits are for an async rule or a workflow step, which run in the background.
	RunLimits = Limits{Steps: 2_000_000, Time: 5 * time.Second}
)

// maxEffects is the most changes and comments one run may ask for.
const maxEffects = 1000

var fileOptions = &syntax.FileOptions{Set: true, While: true}

// entry is the function an inline script's body becomes, so it can return and loop like a function body.
const entry = "__script__"

// Names a script can use without defining them.
var predeclared = map[string]bool{
	"record": true, "vars": true, "user": true, "now": true,
	"children": true, "get": true, "count": true, "update": true, "comment": true, "abort": true,
	"json": true, "math": true, "time": true,
}

// Program is a compiled script, safe to run many times at once, with the script files it loads.
type Program struct {
	prog *starlark.Program
	// lines maps a line of the compiled source to the author's (0: a line they did not write).
	lines   []int
	modules map[string]*starlark.Program
}

// Error is a script's mistake, with where it is: Line in the inline script (0 when it is not about one line), and
// File and FileLine when it is in a script file the script loaded.
type Error struct {
	Line, Col int
	File      string
	FileLine  int
	Message   string
}

func (e *Error) Error() string {
	var at []string
	if e.Line > 0 {
		at = append(at, fmt.Sprintf("line %d", e.Line))
	}
	if e.File != "" {
		at = append(at, fmt.Sprintf("%s line %d", e.File, e.FileLine))
	}
	if len(at) == 0 {
		return e.Message
	}
	return strings.Join(at, ", in ") + ": " + e.Message
}

// Abort is a script's abort("message"): a before rule refuses the save with it, a workflow step fails with it.
type Abort struct{ Message string }

func (a *Abort) Error() string { return a.Message }

// fileRE is what a script file may be called: scripts/<name>.star, loaded as load("<name>.star", ...).
var fileRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}\.star$`)

// ValidFileName reports whether name may name a script file (requests.star).
func ValidFileName(name string) bool { return fileRE.MatchString(name) }

func isPredeclared(name string) bool { return predeclared[name] }

// module is a script file as compiled: its program, the files it loads with the names it takes from each, and the
// names it defines (what others may load from it).
type module struct {
	prog    *starlark.Program
	loads   []loadOf
	globals map[string]bool
}

type loadOf struct {
	file  string
	names []string
	line  int
}

// scan finds a parsed file's loads and top-level names.
func scan(f *syntax.File) (loads []loadOf, globals map[string]bool) {
	globals = map[string]bool{}
	for _, st := range f.Stmts {
		switch x := st.(type) {
		case *syntax.LoadStmt:
			l := loadOf{file: x.ModuleName(), line: int(x.Load.Line)}
			for _, from := range x.From {
				l.names = append(l.names, from.Name)
			}
			loads = append(loads, l)
		case *syntax.DefStmt:
			globals[x.Name.Name] = true
		case *syntax.AssignStmt:
			if id, ok := x.LHS.(*syntax.Ident); ok {
				globals[id.Name] = true
			}
		}
	}
	return loads, globals
}

const loadHelp = `only this app's script files can be loaded, by name (such as "helpers.star")`

// CheckFile checks a script file on its own: its name, syntax and names, and that what it loads are other files of
// the same app (never another app's). Whether those files exist and define what it loads is checked by Compile.
func CheckFile(name, src string) error {
	_, err := compileFile(name, src)
	return err
}

func compileFile(name, src string) (*module, error) {
	switch {
	case !fileRE.MatchString(name):
		return nil, &Error{Message: fmt.Sprintf("%q is not a script file name (lowercase letters, digits and underscores, then .star)", name)}
	case len(src) > MaxSource*5:
		return nil, &Error{File: name, Message: fmt.Sprintf("the file is longer than %d characters", MaxSource*5)}
	}
	f, err := fileOptions.Parse(name, src, 0)
	if err != nil {
		return nil, translateWith(err, nil)
	}
	loads, globals := scan(f)
	for _, l := range loads {
		if !fileRE.MatchString(l.file) {
			return nil, &Error{File: name, FileLine: l.line, Message: fmt.Sprintf("load(%q): %s", l.file, loadHelp)}
		}
	}
	prog, err := starlark.FileProgram(f, isPredeclared)
	if err != nil {
		return nil, translateWith(err, nil)
	}
	return &module{prog: prog, loads: loads, globals: globals}, nil
}

// Compile checks a script written inline (in a step or an action): its body is a function's, so it may loop and
// return. Leading load("file.star", "name") lines take functions from the app's script files (files: name to
// source). Every mistake the compiler can find is reported, by line: syntax, undefined names, and loads of files or
// names that are not there.
func Compile(src string, files map[string]string) (*Program, error) {
	if len(src) > MaxSource {
		return nil, &Error{Message: fmt.Sprintf("the script is longer than %d characters", MaxSource)}
	}
	if strings.TrimSpace(src) == "" {
		return nil, &Error{Message: "the script is empty"}
	}
	// Loads go before the function (Starlark has them at the top of a file); everything else is its body. lines
	// maps each compiled line back to the author's.
	var head, body []string
	var headLines, bodyLines []int
	for i, line := range strings.Split(strings.ReplaceAll(src, "\t", "    "), "\n") {
		if strings.HasPrefix(line, "load(") {
			head, headLines = append(head, line), append(headLines, i+1)
			body, bodyLines = append(body, "    pass"), append(bodyLines, i+1)
			continue
		}
		body, bodyLines = append(body, "    "+line), append(bodyLines, i+1)
	}
	lines := []int{0}
	lines = append(lines, headLines...)
	lines = append(lines, 0) // the def line
	lines = append(lines, bodyLines...)
	text := strings.Join(head, "\n")
	if len(head) > 0 {
		text += "\n"
	}
	text += "def " + entry + "():\n" + strings.Join(body, "\n") + "\n"

	f, err := fileOptions.Parse("script", text, 0)
	if err != nil {
		return nil, translateWith(err, lines)
	}
	loads, _ := scan(f)
	modules := map[string]*starlark.Program{}
	if err := resolveLoads("", loads, files, modules, map[string]bool{}, lines); err != nil {
		return nil, err
	}
	prog, err := starlark.FileProgram(f, isPredeclared)
	if err != nil {
		return nil, translateWith(err, lines)
	}
	return &Program{prog: prog, lines: lines, modules: modules}, nil
}

// resolveLoads compiles what a script or file loads, and what those load, checking each file is there and defines
// the names taken from it, and that no file loads itself through others.
func resolveLoads(from string, loads []loadOf, files map[string]string, done map[string]*starlark.Program, stack map[string]bool, lines []int) error {
	at := func(l loadOf, msg string) error {
		if from == "" {
			return &Error{Line: lineOf(lines, l.line), Message: msg}
		}
		return &Error{File: from, FileLine: l.line, Message: msg}
	}
	for _, l := range loads {
		if !fileRE.MatchString(l.file) {
			return at(l, fmt.Sprintf("load(%q): %s", l.file, loadHelp))
		}
		src, ok := files[l.file]
		if !ok {
			return at(l, fmt.Sprintf("load(%q): this app has no script file %s", l.file, l.file))
		}
		if stack[l.file] {
			return at(l, fmt.Sprintf("load(%q): the files load each other in a loop", l.file))
		}
		m, err := compileFile(l.file, src)
		if err != nil {
			return err
		}
		for _, name := range l.names {
			if !m.globals[name] {
				return at(l, fmt.Sprintf("load(%q): %s defines no %s", l.file, l.file, name))
			}
		}
		if _, seen := done[l.file]; seen {
			continue
		}
		done[l.file] = m.prog
		stack[l.file] = true
		if err := resolveLoads(l.file, m.loads, files, done, stack, lines); err != nil {
			return err
		}
		delete(stack, l.file)
	}
	return nil
}

var loadLineRE = regexp.MustCompile(`(?m)^\s*load\(\s*["']([^"']+)["']`)

// Files are the script files a script loads, directly or through others: what a definition keeps with it, so it
// compiles and runs the same way later (a run in flight keeps the code it started with).
func Files(src string, files map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(text string)
	walk = func(text string) {
		for _, m := range loadLineRE.FindAllStringSubmatch(text, -1) {
			name := m[1]
			if seen[name] {
				continue
			}
			seen[name] = true
			if body, ok := files[name]; ok {
				out = append(out, name)
				walk(body)
			}
		}
	}
	walk(src)
	sort.Strings(out)
	return out
}

func lineOf(lines []int, generated int) int {
	if lines == nil {
		return generated
	}
	if generated > 0 && generated < len(lines) {
		return lines[generated]
	}
	return 0
}

// translateWith turns Starlark's errors into Errors at the author's lines: the inline script's (through lines) or a
// script file's.
func translateWith(err error, lines []int) error {
	place := func(pos syntax.Position) (line int, file string, fileLine int) {
		if pos.Filename() == "script" {
			return lineOf(lines, int(pos.Line)), "", 0
		}
		return 0, pos.Filename(), int(pos.Line)
	}
	var se syntax.Error
	if errors.As(err, &se) {
		line, file, fl := place(se.Pos)
		return &Error{Line: line, Col: int(se.Pos.Col), File: file, FileLine: fl, Message: se.Msg}
	}
	var rl resolve.ErrorList
	if errors.As(err, &rl) && len(rl) > 0 {
		line, file, fl := place(rl[0].Pos)
		msgs := []string{rl[0].Msg}
		for _, e := range rl[1:] {
			l, f, fl2 := place(e.Pos)
			if f != "" {
				msgs = append(msgs, fmt.Sprintf("%s line %d: %s", f, fl2, e.Msg))
			} else {
				msgs = append(msgs, fmt.Sprintf("line %d: %s", l, e.Msg))
			}
		}
		return &Error{Line: line, Col: int(rl[0].Pos.Col), File: file, FileLine: fl, Message: strings.Join(msgs, "; ")}
	}
	var ee *starlark.EvalError
	if errors.As(err, &ee) {
		var a *Abort
		if errors.As(ee.Unwrap(), &a) {
			return a
		}
		out := &Error{Message: ee.Msg}
		for _, fr := range ee.CallStack {
			switch name := fr.Pos.Filename(); name {
			case "script":
				out.Line = lineOf(lines, int(fr.Pos.Line))
			case "", "<builtin>":
			default:
				out.File, out.FileLine = name, int(fr.Pos.Line)
			}
		}
		return out
	}
	return &Error{Message: err.Error()}
}

// Reader reads records other than the one the script runs for, as whoever the script runs as (under their RLS).
type Reader = expr.RecordReader

// Allow says what a script may ask for where it runs.
type Allow struct {
	// Own: changes to the record it runs for. Others: changes and comments on other records. Comments: comments on
	// its own record. Abort: abort() (refuse the save, or fail the step).
	Own, Others, Comments, Abort bool
}

// Env is what one run of a script sees and may do.
type Env struct {
	Record *expr.Record
	User   *expr.User
	Vars   map[string]any
	Now    time.Time
	Reader Reader
	// Count runs a lookup (count("tasks", {"status": "open"})); nil where lookups are not available.
	Count  func(ctx context.Context, target string, filters []expr.Filter) (int64, error)
	Limits Limits
	Allow  Allow
}

// Update is a change a script asked for: fields to set (as record.update names them: title, priority,
// fields.name...) and a status to move to, on a record, with a comment.
type Update struct {
	Record   string
	Fields   map[string]any
	Status   string
	Comment  string
	Internal bool
}

// Comment is a comment a script asked for on a record.
type Comment struct {
	Record   string
	Body     string
	Internal bool
}

// Result is what a run asked for, in order, for the caller to apply.
type Result struct {
	Updates  []Update
	Comments []Comment
	// Vars are a workflow step's new variables; Next the exit it took (empty: the step's next).
	Vars    map[string]any
	Next    string
	Printed []string
}

// Run runs the script once. A mistake while running is an *Error with its line; abort() is an *Abort.
func (p *Program) Run(ctx context.Context, env Env) (*Result, error) {
	lim := env.Limits
	if lim.Steps == 0 {
		lim = RunLimits
	}
	ctx, cancel := context.WithTimeout(ctx, lim.Time)
	defer cancel()
	res := &Result{}
	thread := &starlark.Thread{Name: "script", Print: func(_ *starlark.Thread, msg string) {
		if len(res.Printed) < 200 {
			res.Printed = append(res.Printed, msg)
		}
	}}
	thread.SetMaxExecutionSteps(lim.Steps)
	stop := context.AfterFunc(ctx, func() { thread.Cancel("it ran out of time") })
	defer stop()

	h := &host{ctx: ctx, env: env, res: res, prog: p}
	builtins := h.predeclared()
	// Script files are run once per run, with the same built-ins, so their functions' changes join this run's.
	loaded := map[string]starlark.StringDict{}
	thread.Load = func(t *starlark.Thread, file string) (starlark.StringDict, error) {
		if g, ok := loaded[file]; ok {
			return g, nil
		}
		prog, ok := p.modules[file]
		if !ok {
			return nil, fmt.Errorf("no script file %s", file)
		}
		g, err := prog.Init(t, builtins)
		if err != nil {
			return nil, err
		}
		g.Freeze()
		loaded[file] = g
		return g, nil
	}
	globals, err := p.prog.Init(thread, builtins)
	if err != nil {
		return nil, h.failure(err, thread)
	}
	fn := globals[entry]
	ret, err := starlark.Call(thread, fn, nil, nil)
	if err != nil {
		return nil, h.failure(err, thread)
	}
	if err := h.takeReturn(ret); err != nil {
		return nil, err
	}
	return res, nil
}

// failure explains why a run stopped: its own mistake, abort(), or a limit.
func (h *host) failure(err error, thread *starlark.Thread) error {
	if h.err != nil {
		return h.err
	}
	if thread.ExecutionSteps() >= h.env.Limits.Steps && h.env.Limits.Steps > 0 {
		return &Error{Message: fmt.Sprintf("the script took more than %d steps: is a loop endless?", h.env.Limits.Steps)}
	}
	if h.ctx.Err() != nil {
		return &Error{Message: "the script ran out of time"}
	}
	return translateWith(err, h.prog.lines)
}

// takeReturn reads what the script returned: nothing, or {"vars": {...}, "next": "exit"}.
func (h *host) takeReturn(v starlark.Value) error {
	if v == starlark.None {
		return nil
	}
	d, ok := v.(*starlark.Dict)
	if !ok {
		return &Error{Message: fmt.Sprintf(`a script returns nothing, or a dict such as {"vars": {...}, "next": "done"}, not a %s`, v.Type())}
	}
	for _, item := range d.Items() {
		key, _ := starlark.AsString(item[0])
		switch key {
		case "vars":
			m, err := toGo(item[1])
			vars, ok := m.(map[string]any)
			if err != nil || !ok {
				return &Error{Message: "vars must be a dict of names to values"}
			}
			h.res.Vars = vars
		case "next":
			s, ok := starlark.AsString(item[1])
			if !ok {
				return &Error{Message: "next must be the name of an exit"}
			}
			h.res.Next = s
		default:
			return &Error{Message: fmt.Sprintf("a script's result may have vars and next, not %q", key)}
		}
	}
	return nil
}

// Modules a script can use, all without side effects but the clock in time.now().
var modules = starlark.StringDict{"json": json.Module, "math": math.Module, "time": sltime.Module}
