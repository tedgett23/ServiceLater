package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// The catalog editor (Admin > Catalog) changes catalog items the way the workflow builder changes workflows: an edit
// is kept in the app's studio layer as only what it changes, and the app is reinstalled with it at once
// (docs/apps-platform.md §4.5, §8.4).

// catalogEntryDTO is an item in the editor's list: every item, whoever it is for.
type catalogEntryDTO struct {
	catalogItemDTO
	App string `json:"app"`
	// AvailableTo is who may order it; absent, everyone.
	AvailableTo *apps.Audience `json:"available_to,omitempty"`
	// Edited says it was changed here; Shipped that its app (or a local file) defines it.
	Edited  bool `json:"edited"`
	Shipped bool `json:"shipped"`
}

// catalogAppDTO is an app new items can be added to, with the workflows an item of it may name.
type catalogAppDTO struct {
	Name string `json:"name"`
	// Workflows are its workflows for requests.
	Workflows []string `json:"workflows"`
}

// catalogItemDefDTO is an item as the editor changes it: the same definition its file holds.
type catalogItemDefDTO struct {
	Item     *apps.CatalogItem `json:"item"`
	App      string            `json:"app"`
	Edited   bool              `json:"edited"`
	Shipped  bool              `json:"shipped"`
	EditedAt *time.Time        `json:"edited_at,omitempty"`
	EditedBy *string           `json:"edited_by,omitempty"`
	Layers   []string          `json:"layers"`
	// YAML is the item as a file; Override is the edit as it is kept (what `servicelater app export-local` writes).
	YAML     string `json:"yaml"`
	Override string `json:"override,omitempty"`
	Path     string `json:"path"`
	// Workflows are its app's workflows for requests, any of which may fulfil it.
	Workflows []string `json:"workflows"`
}

// definitionOf finds a definition among the compiled app's.
func (s *studioApp) definitionOf(kind, name string) (apps.Definition, bool) {
	if s.compiled == nil {
		return apps.Definition{}, false
	}
	for _, d := range s.compiled.Definitions {
		if d.Kind == kind && d.Name == name {
			return d, true
		}
	}
	return apps.Definition{}, false
}

func requestWorkflows(c *apps.Compiled) []string {
	out := []string{}
	for _, d := range c.Definitions {
		if d.Workflow != nil && (d.Workflow.RecordType == "request" || d.Workflow.RecordType == "task") {
			out = append(out, d.Name)
		}
	}
	slices.Sort(out)
	return out
}

// GET /catalog-items (catalog:manage): every catalog item, whoever may order it, with where it comes from; and the
// apps new items can go in.
func (h *handlers) listCatalogItems(w http.ResponseWriter, r *http.Request) {
	out := []catalogEntryDTO{}
	edited, shipped := map[string]bool{}, map[string]bool{}
	err := db.WithSystem(r.Context(), h.pool, func(tx pgx.Tx) error {
		defs, err := dbgen.New(tx).ListCurrentDefinitions(r.Context())
		for _, d := range defs {
			if d.Kind == "catalog_item" {
				layers := strings.Split(d.Source, "+")
				edited[d.Name] = slices.Contains(layers, "studio")
				shipped[d.Name] = slices.ContainsFunc(layers, func(l string) bool { return l != "studio" })
			}
		}
		return err
	})
	if err != nil {
		writeDBError(w, err)
		return
	}
	for _, it := range h.apps.CatalogItems() {
		out = append(out, catalogEntryDTO{catalogItemDTO: toCatalogItem(it), App: it.App, AvailableTo: it.AvailableTo,
			Edited: edited[it.Name], Shipped: shipped[it.Name]})
	}
	appsOut := []catalogAppDTO{}
	for _, a := range h.apps.Apps() {
		s, err := h.loadStudioApp(r.Context(), a.Name)
		if err != nil || s.compiled == nil {
			continue
		}
		appsOut = append(appsOut, catalogAppDTO{Name: a.Name, Workflows: requestWorkflows(s.compiled)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "apps": appsOut})
}

func (h *handlers) toCatalogItemDef(ctx context.Context, c *apps.Compiled, d apps.Definition) (catalogItemDefDTO, error) {
	app := c.Manifest.Name
	out := catalogItemDefDTO{Item: d.CatalogItem, App: app, Layers: d.Layers, Path: d.Path, Workflows: requestWorkflows(c),
		Edited: slices.Contains(d.Layers, "studio"), Shipped: slices.ContainsFunc(d.Layers, func(l string) bool { return l != "studio" })}
	data, err := apps.CatalogItemYAML(d.CatalogItem, false)
	if err != nil {
		return out, err
	}
	out.YAML = string(data)
	if out.Edited {
		err = db.WithSystem(ctx, h.pool, func(tx pgx.Tx) error {
			o, err := dbgen.New(tx).GetAppOverride(ctx, dbgen.GetAppOverrideParams{AppName: app, Path: d.Path})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			out.EditedAt, out.EditedBy, out.Override = &o.UpdatedAt.Time, textPtr(o.UpdatedByName), o.Content
			return err
		})
	}
	return out, err
}

// itemApp is the app that provides the {name} item, loaded for editing.
func (h *handlers) itemApp(ctx context.Context, name string) (*studioApp, apps.Definition, error) {
	it := h.apps.CatalogItem(name)
	if it == nil {
		return nil, apps.Definition{}, notFound()
	}
	s, err := h.loadStudioApp(ctx, it.App)
	if err != nil {
		return nil, apps.Definition{}, err
	}
	d, ok := s.definitionOf("catalog_item", name)
	if !ok {
		return nil, apps.Definition{}, notFound()
	}
	return s, d, nil
}

// GET /catalog-items/{name} (catalog:manage): an item's definition for the editor, with where it comes from, whether
// it was edited here, and its YAML.
func (h *handlers) getCatalogItemDef(w http.ResponseWriter, r *http.Request) {
	s, d, err := h.itemApp(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		writeStudioError(w, err)
		return
	}
	out, err := h.toCatalogItemDef(r.Context(), s.compiled, d)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// catalogItemSaveRequest is an item as the editor saves it. App names the app a new item goes in (an existing item
// stays in its own).
type catalogItemSaveRequest struct {
	App  string            `json:"app"`
	Item *apps.CatalogItem `json:"item"`
}

// PUT /catalog-items/{name} (catalog:manage) with {"app"?, "item"}: save an item into its app's studio layer, adding
// it to app when no app has it, and reinstall the app with it at once. It is checked as an install checks it; every
// problem is listed (422 with problems). Upgrades of the app keep the edit; requests already filed keep their answers.
func (h *handlers) putCatalogItemDef(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var req catalogItemSaveRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Item == nil || req.Item.Form == nil {
		writeError(w, http.StatusUnprocessableEntity, "item is required, with its form")
		return
	}
	req.Item.Name, req.Item.Form.RecordType, req.Item.Form.TitleGiven = name, "request", true
	// The editor sends the questions' order as create; any sections it read back are worked out from it again.
	req.Item.Form.CreateSections = nil
	app := req.App
	if it := h.apps.CatalogItem(name); it != nil {
		app = it.App
	}
	if app == "" {
		writeError(w, http.StatusUnprocessableEntity, "app is required for a new item: the app it goes in")
		return
	}
	s, err := h.loadStudioApp(r.Context(), app)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	path := "catalog/" + name + ".yaml"
	if d, ok := s.definitionOf("catalog_item", name); ok {
		path = d.Path
	}
	var content []byte
	base, below, err := s.bundle.Below(path)
	switch {
	case err != nil:
		writeDBError(w, err)
		return
	case below:
		content, err = apps.CatalogItemOverride(base, req.Item)
	default:
		content, err = apps.CatalogItemYAML(req.Item, false)
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	c, err := h.installStudio(r, app, path, content)
	if err != nil {
		var pe *problemsError
		if errors.As(err, &pe) {
			pe.msg = "the item cannot be saved"
		}
		writeStudioError(w, err)
		return
	}
	saved := studioApp{bundle: s.bundle, compiled: c}
	d, _ := saved.definitionOf("catalog_item", name)
	out, err := h.toCatalogItemDef(r.Context(), c, d)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /catalog-items/{name}/edits (catalog:manage): undo the edits made here: an item its app ships goes back to the
// app's version; one added here is removed from the catalog. Requests already filed from it are kept.
func (h *handlers) resetCatalogItemDef(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	s, d, err := h.itemApp(r.Context(), name)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	if !slices.Contains(d.Layers, "studio") {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s has no edits made here", name))
		return
	}
	c, err := h.installStudio(r, s.compiled.Manifest.Name, d.Path, nil)
	if err != nil {
		writeStudioError(w, err)
		return
	}
	reset := studioApp{bundle: s.bundle, compiled: c}
	shipped, ok := reset.definitionOf("catalog_item", name)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	out, err := h.toCatalogItemDef(r.Context(), c, shipped)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
