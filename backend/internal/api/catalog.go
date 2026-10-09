package api

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
)

// catalogItemDTO is an item in the catalog, as the list shows it.
type catalogItemDTO struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Category string `json:"category"`
	Summary  string `json:"summary,omitempty"`
	// Icon is one of a fixed set of pictures (laptop, software, access...).
	Icon string `json:"icon,omitempty"`
}

// catalogItemDetailDTO is an item with what ordering it needs.
type catalogItemDetailDTO struct {
	catalogItemDTO
	// Description is the item's page, in Markdown.
	Description string `json:"description,omitempty"`
	// Form is its questions: fields, and which are asked when ordering (create) and shown on the request (detail).
	Form formDTO `json:"form"`
	// CanOrderForOthers says the caller may name someone else as who it is for (records:update:request).
	CanOrderForOthers bool   `json:"can_order_for_others"`
	App               string `json:"app"`
}

func toCatalogItem(it *apps.CatalogItem) catalogItemDTO {
	return catalogItemDTO{Name: it.Name, Label: it.Label, Category: it.Category, Summary: it.Summary, Icon: it.Icon}
}

// orderable is the {name} item, if the caller may see and order it. Anything else is not found: an item someone may
// not order is not shown to them at all.
func (h *handlers) orderable(r *http.Request, q *dbgen.Queries, p auth.Principal) (*apps.CatalogItem, error) {
	it := h.apps.CatalogItem(chi.URLParam(r, "name"))
	if it == nil {
		return nil, notFound()
	}
	ok, err := records.MayOrder(r.Context(), q, p.UserID, it)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound()
	}
	return it, nil
}

// GET /catalog: the items the caller may order, by category, then the order their apps give, then label.
func (h *handlers) listCatalog(w http.ResponseWriter, r *http.Request) {
	out := []catalogItemDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		for _, it := range h.apps.CatalogItems() {
			ok, err := records.MayOrder(r.Context(), q, p.UserID, it)
			if err != nil {
				return err
			}
			if ok {
				out = append(out, toCatalogItem(it))
			}
		}
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// GET /catalog/{name}: an item the caller may order, with its questions.
func (h *handlers) getCatalogItem(w http.ResponseWriter, r *http.Request) {
	var out catalogItemDetailDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		it, err := h.orderable(r, q, p)
		if err != nil {
			return err
		}
		out = catalogItemDetailDTO{catalogItemDTO: toCatalogItem(it), Description: it.Description, Form: toFormDTO(it.Form),
			CanOrderForOthers: p.Perms.CanRecord(authz.Update, "request"), App: it.App}
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// catalogStatesRequest is an order as far as it is filled in.
type catalogStatesRequest struct {
	Description string `json:"description"`
	// Fields are the answers so far, by question name.
	Fields map[string]any `json:"fields"`
}

// POST /catalog/{name}/states: whether each of an item's questions is shown, required and writable for an order
// with these answers, and which options a choice offers. Nothing is saved; ordering checks it all again.
func (h *handlers) catalogStates(w http.ResponseWriter, r *http.Request) {
	var req catalogStatesRequest
	if !decode(w, r, &req) {
		return
	}
	var out formStatesDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		it, err := h.orderable(r, q, p)
		if err != nil {
			return err
		}
		out.States, out.Values, err = h.svc.DraftStates(r.Context(), q, actorOf(p), records.CreateInput{Type: "request", Title: it.Label,
			Description: req.Description, Fields: req.Fields, CatalogItem: it.Name})
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// orderRequest orders an item.
type orderRequest struct {
	// Fields are the answers to the item's questions, by name.
	Fields map[string]any `json:"fields"`
	// Description is anything else the requester wants to say, when the item asks (its form places description).
	Description string `json:"description"`
	// RequesterID orders it for someone else (needs records:update:request); the caller by default.
	RequesterID *uuid.UUID `json:"requester_id"`
}

// POST /catalog/{name}/request with {fields, description?, requester_id?}: order an item. It files a request, named
// after the item and the answers, and starts the item's fulfilment workflow (approvals, tasks for the teams involved).
// Answers are checked against the item's questions like any form.
func (h *handlers) orderCatalogItem(w http.ResponseWriter, r *http.Request) {
	var req orderRequest
	if !decode(w, r, &req) {
		return
	}
	var out ticketDTO
	if !h.tx(w, r, func(q *dbgen.Queries, p auth.Principal) error {
		it, err := h.orderable(r, q, p)
		if err != nil {
			return err
		}
		saved, err := h.svc.Create(r.Context(), q, actorOf(p), ruleruns.Cause{}, records.CreateInput{Type: "request", Title: it.Label,
			Description: req.Description, RequesterID: req.RequesterID, Fields: req.Fields, CatalogItem: it.Name})
		if err != nil {
			return err
		}
		out = ticketDTO{Ticket: saved}
		return h.detail(r.Context(), q, p, &out)
	}) {
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/api/v1/tickets/%s", out.ID))
	writeJSON(w, http.StatusCreated, out)
}
