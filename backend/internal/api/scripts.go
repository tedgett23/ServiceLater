package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/script"
)

// Where a script runs decides what it may do (docs/apps-platform.md §6.7).
const (
	scriptInWorkflow = "workflow"
	scriptInBefore   = "before"
	scriptInAsync    = "async"
)

// scriptProblemDTO is a mistake in a script, at a line (0: not about one line).
type scriptProblemDTO struct {
	Line int `json:"line"`
	Col  int `json:"col"`
	// File and FileLine are where in a script file the script loaded it went wrong.
	File     string `json:"file,omitempty"`
	FileLine int    `json:"file_line,omitempty"`
	Message  string `json:"message"`
}

func toScriptProblem(err error) scriptProblemDTO {
	var se *script.Error
	if errors.As(err, &se) {
		return scriptProblemDTO{Line: se.Line, Col: se.Col, File: se.File, FileLine: se.FileLine, Message: se.Message}
	}
	return scriptProblemDTO{Message: err.Error()}
}

// scriptCheckRequest is a script to check.
type scriptCheckRequest struct {
	Script string `json:"script"`
	// App is the app the script belongs to: its script files are what the script may load.
	App string `json:"app"`
	// File, when set, says this is that script file of the app rather than an inline script.
	File string `json:"file"`
}

// appScripts are an installed app's script files (name to source), as installed, with its edits.
func (h *handlers) appScripts(ctx context.Context, app string) (map[string]string, error) {
	files := map[string]string{}
	if app == "" {
		return files, nil
	}
	s, err := h.loadStudioApp(ctx, app)
	if err != nil {
		return nil, err
	}
	for _, d := range s.compiled.Definitions {
		if d.ScriptFile != nil {
			files[d.ScriptFile.Name] = d.ScriptFile.Source
		}
	}
	return files, nil
}

// POST /scripts/check (scripts:edit) with {"script", "app", "file"}: the mistakes the compiler finds (syntax, undefined
// names, loads of files or names the app does not have), each with its line, without running it: {"problems": [...]},
// empty when there are none. With file, the script is that script file of the app. The editor asks as it is typed.
func (h *handlers) checkScript(w http.ResponseWriter, r *http.Request) {
	var req scriptCheckRequest
	if !decode(w, r, &req) {
		return
	}
	files, err := h.appScripts(r.Context(), req.App)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	out := []scriptProblemDTO{}
	if req.File != "" {
		if err := script.CheckFile(req.File, req.Script); err != nil {
			p := toScriptProblem(err)
			if p.Line == 0 {
				p.Line = p.FileLine // a file's own lines
			}
			out = append(out, p)
		}
	} else if _, err := script.Compile(req.Script, files); err != nil {
		out = append(out, toScriptProblem(err))
	}
	writeJSON(w, http.StatusOK, map[string]any{"problems": out})
}

// scriptTestRequest is a script to try against a record.
type scriptTestRequest struct {
	Script string `json:"script"`
	// Record is the reference (INC42) or id of the record to run it for; leave it out for a workflow with none.
	Record string `json:"record"`
	// Vars are a workflow's variables as the script would see them.
	Vars map[string]any `json:"vars"`
	// Where is workflow (the default), before or async: what the script may do, and its limits, follow from it.
	Where string `json:"where"`
	// App is the app the script belongs to: its script files are what the script may load.
	App string `json:"app"`
}

type scriptChangeDTO struct {
	Reference string         `json:"reference"`
	Title     string         `json:"title"`
	Fields    map[string]any `json:"fields,omitempty"`
	Status    string         `json:"status,omitempty"`
	Comment   string         `json:"comment,omitempty"`
	Internal  bool           `json:"internal,omitempty"`
}

type scriptTestDTO struct {
	// OK is false when the script failed (Problem) or stopped itself with abort (Aborted).
	OK      bool              `json:"ok"`
	Problem *scriptProblemDTO `json:"problem,omitempty"`
	Aborted string            `json:"aborted,omitempty"`
	// Changes and Comments are what it asked for, in order: nothing was changed.
	Changes  []scriptChangeDTO `json:"changes"`
	Comments []scriptChangeDTO `json:"comments"`
	Vars     map[string]any    `json:"vars,omitempty"`
	Next     string            `json:"next,omitempty"`
	Printed  []string          `json:"printed"`
	// Millis is how long it ran.
	Millis int64 `json:"millis"`
}

// POST /scripts/test (scripts:edit) with {"script", "record", "vars", "where"}: run a script as it would run there, for
// a record, as you (reading only what you may read), and say what it would change. Nothing is changed.
func (h *handlers) testScript(w http.ResponseWriter, r *http.Request) {
	var req scriptTestRequest
	if !decode(w, r, &req) {
		return
	}
	out := scriptTestDTO{Changes: []scriptChangeDTO{}, Comments: []scriptChangeDTO{}, Printed: []string{}}
	files, err := h.appScripts(r.Context(), req.App)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	prog, err := script.Compile(req.Script, files)
	if err != nil {
		p := toScriptProblem(err)
		out.Problem = &p
		writeJSON(w, http.StatusOK, out)
		return
	}
	env := script.Env{Vars: req.Vars, Now: time.Now()}
	switch req.Where {
	case scriptInBefore:
		env.Limits, env.Allow = script.BeforeLimits, script.Allow{Own: true, Abort: true}
	case scriptInAsync:
		env.Limits, env.Allow = script.RunLimits, script.Allow{Own: true, Others: true, Comments: true}
	case scriptInWorkflow, "":
		env.Limits, env.Allow = script.RunLimits, script.Allow{Own: true, Others: true, Comments: true, Abort: true}
	default:
		writeError(w, http.StatusUnprocessableEntity, "where must be workflow, before or async")
		return
	}
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		u, err := q.GetUser(r.Context(), p.UserID)
		if err != nil {
			return err
		}
		env.User = &expr.User{ID: u.ID.String(), Name: u.Name, Email: u.Email, Role: u.Role}
		env.Reader, env.Count = records.ScriptReader(q), records.Counter(q)
		if req.Record != "" {
			t, err := resolveTicket(r.Context(), q, req.Record)
			if err != nil {
				return err
			}
			full, err := records.Load(r.Context(), q, t.ID)
			if err != nil {
				return err
			}
			env.Record = full.ExprRecord()
		}
		start := time.Now()
		res, err := prog.Run(r.Context(), env)
		out.Millis = time.Since(start).Milliseconds()
		var ab *script.Abort
		var se *script.Error
		switch {
		case errors.As(err, &ab):
			out.Aborted = ab.Message
			return nil
		case errors.As(err, &se):
			p := toScriptProblem(err)
			out.Problem = &p
			return nil
		case err != nil:
			return err
		}
		out.OK, out.Vars, out.Next, out.Printed = true, res.Vars, res.Next, append(out.Printed, res.Printed...)
		describe := func(id string) scriptChangeDTO {
			if rid, err := uuid.Parse(id); err == nil {
				if t, err := records.Load(r.Context(), q, rid); err == nil {
					return scriptChangeDTO{Reference: t.Reference, Title: t.Title}
				}
			}
			return scriptChangeDTO{Reference: id}
		}
		for _, u := range res.Updates {
			c := describe(u.Record)
			c.Fields, c.Status, c.Comment, c.Internal = u.Fields, u.Status, u.Comment, u.Internal
			out.Changes = append(out.Changes, c)
		}
		for _, cm := range res.Comments {
			c := describe(cm.Record)
			c.Comment, c.Internal = cm.Body, cm.Internal
			out.Comments = append(out.Comments, c)
		}
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
