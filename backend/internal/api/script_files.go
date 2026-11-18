package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/script"
)

// An app's script files (scripts/<name>.star, docs/apps-platform.md §6.7) are edited like its workflows: an edit is
// kept in the app's studio layer, whole (code is replaced, never merged), and the app is reinstalled with it at once,
// so everything that loads the file is checked against the change. Runs in flight keep the code they started with.

// scriptFileDTO is a script file in an app.
type scriptFileDTO struct {
	App    string `json:"app"`
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
	// Edited says it was changed here; Shipped that its app (or a local file) has it.
	Edited   bool       `json:"edited"`
	Shipped  bool       `json:"shipped"`
	EditedAt *time.Time `json:"edited_at,omitempty"`
	EditedBy *string    `json:"edited_by,omitempty"`
	// UsedBy are the app's workflows and rules that load it (workflow:name, rule:name).
	UsedBy []string `json:"used_by"`
}

func scriptFilesOf(c *apps.Compiled) []scriptFileDTO {
	usedBy := map[string][]string{}
	for _, d := range c.Definitions {
		var mods map[string]string
		switch {
		case d.Workflow != nil:
			mods = d.Workflow.Modules
		case d.Rule != nil:
			mods = d.Rule.Modules
		}
		for name := range mods {
			usedBy[name] = append(usedBy[name], d.Kind+":"+d.Name)
		}
	}
	out := []scriptFileDTO{}
	for _, d := range c.Definitions {
		if d.ScriptFile == nil {
			continue
		}
		used := usedBy[d.ScriptFile.Name]
		sort.Strings(used)
		if used == nil {
			used = []string{}
		}
		out = append(out, scriptFileDTO{App: c.Manifest.Name, Name: d.ScriptFile.Name, Source: d.ScriptFile.Source, UsedBy: used,
			Edited: slices.Contains(d.Layers, "studio"), Shipped: slices.ContainsFunc(d.Layers, func(l string) bool { return l != "studio" })})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GET /scripts (workflows:read_all or workflows:edit): every installed app's script files, without their source.
func (h *handlers) listScriptFiles(w http.ResponseWriter, r *http.Request) {
	out := []scriptFileDTO{}
	for _, a := range h.apps.Apps() {
		s, err := h.loadStudioApp(r.Context(), a.Name)
		if err != nil || s.compiled == nil {
			continue
		}
		for _, f := range scriptFilesOf(s.compiled) {
			f.Source = ""
			out = append(out, f)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// scriptFileIn finds a script file of an app, with who last changed it here.
func (h *handlers) scriptFileIn(ctx context.Context, c *apps.Compiled, name string) (scriptFileDTO, bool, error) {
	for _, f := range scriptFilesOf(c) {
		if f.Name != name {
			continue
		}
		if !f.Edited {
			return f, true, nil
		}
		err := db.WithSystem(ctx, h.pool, func(tx pgx.Tx) error {
			o, err := dbgen.New(tx).GetAppOverride(ctx, dbgen.GetAppOverrideParams{AppName: c.Manifest.Name, Path: "scripts/" + name})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			f.EditedAt, f.EditedBy = &o.UpdatedAt.Time, textPtr(o.UpdatedByName)
			return err
		})
		return f, true, err
	}
	return scriptFileDTO{}, false, nil
}

// GET /scripts/{app}/{file} (workflows:read_all or workflows:edit): a script file, its source, and what loads it.
func (h *handlers) getScriptFile(w http.ResponseWriter, r *http.Request) {
	s, err := h.loadStudioApp(r.Context(), chi.URLParam(r, "app"))
	if err != nil {
		writeStudioError(w, err)
		return
	}
	f, ok, err := h.scriptFileIn(r.Context(), s.compiled, chi.URLParam(r, "file"))
	switch {
	case err != nil:
		writeDBError(w, err)
	case !ok:
		writeError(w, http.StatusNotFound, "not found")
	default:
		writeJSON(w, http.StatusOK, f)
	}
}

// scriptFileRequest is a script file's new source.
type scriptFileRequest struct {
	Source string `json:"source"`
}

// PUT /scripts/{app}/{file} (scripts:edit) with {"source"}: save a script file into the app's studio layer (adding it
// if the app has none of that name) and reinstall the app with it at once. It is checked as an install checks it,
// with everything that loads it; every problem is listed (422 with problems).
func (h *handlers) putScriptFile(w http.ResponseWriter, r *http.Request) {
	app, name := chi.URLParam(r, "app"), chi.URLParam(r, "file")
	var req scriptFileRequest
	if !decode(w, r, &req) {
		return
	}
	if err := script.CheckFile(name, req.Source); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the script file cannot be saved", "problems": []string{err.Error()}})
		return
	}
	if _, err := h.loadStudioApp(r.Context(), app); err != nil {
		writeStudioError(w, err)
		return
	}
	c, err := h.installStudio(r, app, "scripts/"+name, []byte(req.Source))
	if err != nil {
		var pe *problemsError
		if errors.As(err, &pe) {
			pe.msg = "the script file cannot be saved"
		}
		writeStudioError(w, err)
		return
	}
	f, _, err := h.scriptFileIn(r.Context(), c, name)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// DELETE /scripts/{app}/{file}/edits (scripts:edit): undo the changes made here: the file goes back to the app's
// version, or, if it was added here, goes (refused, 422, while something still loads it).
func (h *handlers) resetScriptFile(w http.ResponseWriter, r *http.Request) {
	app, name := chi.URLParam(r, "app"), chi.URLParam(r, "file")
	s, err := h.loadStudioApp(r.Context(), app)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	f, ok, err := h.scriptFileIn(r.Context(), s.compiled, name)
	switch {
	case err != nil:
		writeDBError(w, err)
		return
	case !ok || !f.Edited:
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s has no changes made here", name))
		return
	}
	c, err := h.installStudio(r, app, "scripts/"+name, nil)
	if err != nil {
		var pe *problemsError
		if errors.As(err, &pe) {
			pe.msg = "the change cannot be undone"
		}
		writeStudioError(w, err)
		return
	}
	after, ok, err := h.scriptFileIn(r.Context(), c, name)
	switch {
	case err != nil:
		writeDBError(w, err)
	case !ok:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusOK, after)
	}
}
