package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/cmdb"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
)

// maxExportRows bounds an export; a CMDB bigger than this is exported by other means.
const maxExportRows = 100000

// importRowDTO is what an import does, or would do, with one row of the file.
type importRowDTO struct {
	// Row is the line of the file the row is on (the header is line 1).
	Row  int    `json:"row"`
	Name string `json:"name"`
	// Class is the item's class, for a file that covers a class and those extending it.
	Class string `json:"class,omitempty"`
	// Action is create, update, unchanged or error.
	Action string `json:"action"`
	// ID is the item created or updated (for an item row once applied, or one already there).
	ID *uuid.UUID `json:"id,omitempty"`
	// Changes are the columns an update changes.
	Changes []string `json:"changes,omitempty"`
	// Problem says why the row cannot be imported.
	Problem string `json:"problem,omitempty"`
}

// importReportDTO is the result of reading an import file, and of applying it when asked.
type importReportDTO struct {
	// Applied says the changes were made; otherwise this is a preview, or the import was refused for its problems.
	Applied bool `json:"applied"`
	// Counts are the rows by action.
	Counts map[string]int `json:"counts"`
	Rows   []importRowDTO `json:"rows"`
}

func newImportReport(n int) importReportDTO {
	return importReportDTO{Counts: map[string]int{"create": 0, "update": 0, "unchanged": 0, "error": 0}, Rows: make([]importRowDTO, 0, n)}
}

func (rep *importReportDTO) add(row importRowDTO) {
	rep.Counts[row.Action]++
	rep.Rows = append(rep.Rows, row)
}

// readImport reads an import's body: a CSV file of at most cmdb.MaxImportBytes.
func readImport(w http.ResponseWriter, r *http.Request) (header []string, rows []cmdb.Row, ok bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cmdb.MaxImportBytes))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the file is larger than %d MB: split it", cmdb.MaxImportBytes>>20))
		return nil, nil, false
	} else if err != nil {
		writeError(w, http.StatusBadRequest, "the file could not be read")
		return nil, nil, false
	}
	header, rows, err = cmdb.ReadCSV(data)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return nil, nil, false
	}
	return header, rows, true
}

// csvHeaders makes the answer a CSV file to save under this name.
func csvHeaders(w http.ResponseWriter, filename string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
}

// classFamily is a class and every class extending it (its family): the class first, then the others nearest first
// and by label, so files list their columns in a steady order.
func (h *handlers) classFamily(c *apps.Class) []*apps.Class {
	var rest []*apps.Class
	for _, name := range h.apps.ClassAndDescendants(c.Name)[1:] {
		if d := h.apps.Class(name); d != nil {
			rest = append(rest, d)
		}
	}
	slices.SortFunc(rest, func(a, b *apps.Class) int {
		if n := len(a.Ancestors) - len(b.Ancestors); n != 0 {
			return n
		}
		return strings.Compare(a.Label, b.Label)
	})
	return append([]*apps.Class{c}, rest...)
}

func classNames(family []*apps.Class) []string {
	out := make([]string, len(family))
	for i, c := range family {
		out[i] = c.Name
	}
	return out
}

// GET /cmdb/csv/items/{class}?template= (cmdb:read): a class's items, and those of the classes extending it, as a CSV
// file, one per row, in the format an import reads; with template=true, the header row only. When other classes
// extend it, a class column says which class each row is, and the columns are every field of the family.
func (h *handlers) exportCIs(w http.ResponseWriter, r *http.Request) {
	class := h.apps.Class(chi.URLParam(r, "class"))
	if class == nil {
		writeError(w, http.StatusNotFound, "there is no such class: see GET /cmdb/model")
		return
	}
	family := h.classFamily(class)
	fields := cmdb.FamilyFields(family)
	out := [][]string{cmdb.ItemTemplateHeader(family)}
	template := r.URL.Query().Get("template") == "true"
	if !template && !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		items, err := q.ListCIsForExport(r.Context(), classNames(family))
		if err != nil {
			return err
		}
		if len(items) > maxExportRows {
			return invalid(fmt.Sprintf("there are more than %d %s items to export", maxExportRows, class.Label))
		}
		for _, c := range items {
			values := records.DecodeFields(c.Fields)
			row := []string{c.Name}
			if len(family) > 1 {
				row = append(row, c.Class)
			}
			row = append(row, c.Status, c.Description, c.OwnerGroup.String, c.SupportGroup.String)
			for _, f := range fields {
				row = append(row, cmdb.FieldText(values[f.Name]))
			}
			out = append(out, row)
		}
		return nil
	}) {
		return
	}
	name := class.Name + ".csv"
	if template {
		name = class.Name + "-template.csv"
	}
	csvHeaders(w, name)
	w.WriteHeader(http.StatusOK)
	_ = cmdb.WriteCSV(w, out) // too late to answer otherwise
}

// POST /cmdb/csv/items/{class}?apply=&skip_errors= (cmdb:manage) with a CSV file as the body: import items of a class
// and of the classes extending it. A row's class is its class cell (a name or label in the family; empty is the
// file's class). Each row is matched to an item of its class by name (case aside): a new name adds an item, a known
// one updates it, and an empty cell leaves that value as it is (an import never clears a value, removes an item or
// moves one to another class). A new name that another class of the family already has is a problem, not a second
// item: set the class to update that one. Groups are named;
// field cells take what the class's fields take, or a choice's label. Without apply=true nothing is written: the
// answer says what each row would do. With it, the import happens in one transaction, and only if no row has a
// problem (422 with the report otherwise), unless skip_errors=true, which imports the other rows.
func (h *handlers) importCIs(w http.ResponseWriter, r *http.Request) {
	class := h.apps.Class(chi.URLParam(r, "class"))
	if class == nil {
		writeError(w, http.StatusNotFound, "there is no such class: see GET /cmdb/model")
		return
	}
	header, rows, ok := readImport(w, r)
	if !ok {
		return
	}
	family := h.classFamily(class)
	layout, err := cmdb.ItemHeader(family, header)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	apply, skip := r.URL.Query().Get("apply") == "true", r.URL.Query().Get("skip_errors") == "true"
	rep := newImportReport(len(rows))
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		groups, err := groupIDsByName(r, q)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(rows))
		for _, row := range rows {
			names = append(names, strings.ToLower(row.Cell(layout.Core[cmdb.ColName])))
		}
		found, err := q.FindCIsByName(r.Context(), dbgen.FindCIsByNameParams{Classes: classNames(family), Names: names})
		if err != nil {
			return err
		}
		existing := make(map[string][]dbgen.FindCIsByNameRow, len(found)) // by lowercased name: one per class
		for _, c := range found {
			existing[strings.ToLower(c.Name)] = append(existing[strings.ToLower(c.Name)], c)
		}
		type write struct {
			index  int
			id     uuid.UUID
			params dbgen.UpdateCIParams
		}
		var writes []write
		seen := map[string]int{}
		for _, row := range rows {
			out, params := h.importCIRow(family, layout, row, groups, existing, seen)
			if out.Action == "create" || out.Action == "update" {
				id := uuid.Nil
				if out.ID != nil {
					id = *out.ID
				}
				writes = append(writes, write{index: len(rep.Rows), id: id, params: params})
			}
			rep.add(out)
		}
		if !apply || (rep.Counts["error"] > 0 && !skip) || len(writes) == 0 {
			rep.Applied = apply && (rep.Counts["error"] == 0 || skip)
			return nil
		}
		if err := q.QuietCIChanges(r.Context()); err != nil {
			return err
		}
		for _, wr := range writes {
			p := wr.params
			if wr.id == uuid.Nil {
				id, err := q.CreateCI(r.Context(), dbgen.CreateCIParams{Class: p.Class, Name: p.Name, Status: p.Status, Description: p.Description,
					OwnerGroupID: p.OwnerGroupID, SupportGroupID: p.SupportGroupID, Fields: p.Fields})
				if err != nil {
					return h.ciWriteError(err, p.Class, p.Name)
				}
				rep.Rows[wr.index].ID = &id
				continue
			}
			p.ID = wr.id
			if _, err := q.UpdateCI(r.Context(), p); err != nil {
				return h.ciWriteError(err, p.Class, p.Name)
			}
		}
		rep.Applied = true
		return q.NotifyCIBulk(r.Context())
	}) {
		return
	}
	if apply && !rep.Applied {
		writeJSON(w, http.StatusUnprocessableEntity, rep)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// importCIRow works out what one row of an items file does, and the item it would write.
func (h *handlers) importCIRow(family []*apps.Class, layout cmdb.ItemLayout, row cmdb.Row, groups map[string]uuid.UUID,
	existing map[string][]dbgen.FindCIsByNameRow, seen map[string]int) (importRowDTO, dbgen.UpdateCIParams) {
	name := row.Cell(layout.Core[cmdb.ColName])
	out := importRowDTO{Row: row.Line, Name: name, Action: "error"}
	cell := func(col string) string {
		i, ok := layout.Core[col]
		if !ok {
			return ""
		}
		return row.Cell(i)
	}
	fail := func(msg string) (importRowDTO, dbgen.UpdateCIParams) {
		out.Problem = msg
		return out, dbgen.UpdateCIParams{}
	}
	class := family[0]
	if s := cell(cmdb.ColClass); s != "" {
		c := h.classByCell(s)
		if c == nil || !slices.Contains(family, c) {
			return fail(fmt.Sprintf("class must be %s", classLabels(family)))
		}
		class = c
	}
	if len(family) > 1 {
		out.Class = class.Name
	}
	if name == "" {
		return fail("the name is empty")
	}
	key := strings.ToLower(name)
	if line, dup := seen[key]; dup {
		return fail(fmt.Sprintf("row %d has the same name", line))
	}
	var match *dbgen.FindCIsByNameRow
	for i, e := range existing[key] {
		if e.Class == class.Name {
			match = &existing[key][i]
		}
	}
	if match == nil && len(existing[key]) > 0 {
		other := h.apps.Class(existing[key][0].Class)
		label := existing[key][0].Class
		if other != nil {
			label = other.Label
		}
		return fail(fmt.Sprintf("there is already a %s called %s: set class to %s to update it", strings.ToLower(label), existing[key][0].Name, label))
	}
	// The name is this row's from here on: a later row with it is a second go at the same item.
	seen[key] = row.Line

	req := ciRequest{Name: &name}
	if s := cell(cmdb.ColStatus); s != "" {
		v, ok := cmdb.StatusCell(s)
		if !ok {
			return fail("status must be Planned, Active, In maintenance or Retired")
		}
		req.Status = &v
	}
	if s := cell(cmdb.ColDescription); s != "" {
		req.Description = &s
	}
	for _, g := range []struct {
		col string
		to  *nullable[uuid.UUID]
	}{{cmdb.ColOwnerGroup, &req.OwnerGroupID}, {cmdb.ColSupportGroup, &req.SupportGroupID}} {
		s := cell(g.col)
		if s == "" {
			continue
		}
		id, ok := groups[strings.ToLower(s)]
		if !ok {
			return fail(fmt.Sprintf("there is no group called %s", s))
		}
		*g.to = nullable[uuid.UUID]{Set: true, Value: &id}
	}
	for _, f := range cmdb.FamilyFields(family) {
		i, ok := layout.Fields[f.Name]
		if !ok || row.Cell(i) == "" {
			continue
		}
		fd, has := class.Field(f.Name)
		if !has {
			return fail(fmt.Sprintf("%s items have no %s", class.Label, f.Label))
		}
		v, msg := cmdb.FieldCell(fd, row.Cell(i))
		if msg != "" {
			return fail(fd.Label + " " + msg)
		}
		if req.Fields == nil {
			req.Fields = map[string]any{}
		}
		req.Fields[f.Name] = v
	}

	cur, creating := dbgen.UpdateCIParams{Class: class.Name, Status: "active", Fields: []byte("{}")}, true
	if match != nil {
		cur, creating = dbgen.UpdateCIParams(*match), false
		id := match.ID
		out.ID = &id
	}
	next, err := h.applyCI(req, cur, creating)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			return fail(ae.msg)
		}
		return fail(err.Error())
	}
	if creating {
		out.Action = "create"
		return out, next
	}
	out.Changes = ciChanges(cur, next)
	out.Action = "update"
	if len(out.Changes) == 0 {
		out.Action = "unchanged"
	}
	return out, next
}

// ciChanges names what differs between an item and its next version: core columns, then fields, by name.
func ciChanges(a, b dbgen.UpdateCIParams) []string {
	var out []string
	for _, c := range []struct {
		name string
		same bool
	}{{cmdb.ColName, a.Name == b.Name}, {cmdb.ColStatus, a.Status == b.Status}, {cmdb.ColDescription, a.Description == b.Description},
		{cmdb.ColOwnerGroup, a.OwnerGroupID == b.OwnerGroupID}, {cmdb.ColSupportGroup, a.SupportGroupID == b.SupportGroupID}} {
		if !c.same {
			out = append(out, c.name)
		}
	}
	before, after := records.DecodeFields(a.Fields), records.DecodeFields(b.Fields)
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	var fields []string
	for k := range keys {
		x, _ := json.Marshal(before[k])
		y, _ := json.Marshal(after[k])
		if !bytes.Equal(x, y) {
			fields = append(fields, k)
		}
	}
	slices.Sort(fields)
	return append(out, fields...)
}

// classLabels lists a family's classes for a message: "Hardware, Server or Computer".
func classLabels(family []*apps.Class) string {
	labels := make([]string, len(family))
	for i, c := range family {
		labels[i] = c.Label
	}
	if len(labels) == 1 {
		return labels[0]
	}
	return strings.Join(labels[:len(labels)-1], ", ") + " or " + labels[len(labels)-1]
}

// groupIDsByName maps every group's name (lowercased) to its id, for import cells that name groups.
func groupIDsByName(r *http.Request, q *dbgen.Queries) (map[string]uuid.UUID, error) {
	gs, err := q.ListGroups(r.Context())
	out := make(map[string]uuid.UUID, len(gs))
	for _, g := range gs {
		out[strings.ToLower(g.Name)] = g.ID
	}
	return out, err
}

// classByCell is the class a cell names, by name or label.
func (h *handlers) classByCell(s string) *apps.Class {
	if c := h.apps.Class(strings.ToLower(s)); c != nil {
		return c
	}
	for _, c := range h.apps.Classes() {
		if strings.EqualFold(c.Label, s) {
			return c
		}
	}
	return nil
}

// relationshipByCell is the relationship type a cell names, by name or by how it reads forwards ("depends on").
func (h *handlers) relationshipByCell(s string) *apps.Relationship {
	if rel := h.apps.Relationship(strings.ToLower(strings.ReplaceAll(s, " ", "_"))); rel != nil {
		return rel
	}
	for _, rel := range h.apps.Relationships() {
		if strings.EqualFold(rel.Label, s) {
			return rel
		}
	}
	return nil
}

// GET /cmdb/csv/relationships?template= (cmdb:read): every relationship between items as a CSV file, in the format an
// import reads (from_class, from_name, type, to_class, to_name); with template=true, the header row only.
func (h *handlers) exportCIRelationships(w http.ResponseWriter, r *http.Request) {
	out := [][]string{cmdb.RelationshipColumns}
	template := r.URL.Query().Get("template") == "true"
	if !template && !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		rels, err := q.ListCIRelationshipsForExport(r.Context())
		if err != nil {
			return err
		}
		if len(rels) > maxExportRows {
			return invalid(fmt.Sprintf("there are more than %d relationships to export", maxExportRows))
		}
		for _, rel := range rels {
			out = append(out, []string{rel.FromClass, rel.FromName, rel.Type, rel.ToClass, rel.ToName})
		}
		return nil
	}) {
		return
	}
	name := "relationships.csv"
	if template {
		name = "relationships-template.csv"
	}
	csvHeaders(w, name)
	w.WriteHeader(http.StatusOK)
	_ = cmdb.WriteCSV(w, out) // too late to answer otherwise
}

// POST /cmdb/csv/relationships?apply=&skip_errors= (cmdb:manage) with a CSV file as the body: relate items, one
// relationship per row, reading "from <type> to". Items are named by class (name or label) and name, and must exist
// already (import the items first); the type is its name or how it reads ("depends on"). A relationship already
// there is left alone; an import never removes one. apply and skip_errors work as for items.
func (h *handlers) importCIRelationships(w http.ResponseWriter, r *http.Request) {
	header, rows, ok := readImport(w, r)
	if !ok {
		return
	}
	cols, err := cmdb.RelationshipHeader(header)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	apply, skip := r.URL.Query().Get("apply") == "true", r.URL.Query().Get("skip_errors") == "true"
	rep := newImportReport(len(rows))
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		// The items every row names, found in one query.
		var classes, names []string
		for _, row := range rows {
			for _, end := range []string{"from", "to"} {
				if c := h.classByCell(row.Cell(cols[end+"_class"])); c != nil {
					classes, names = append(classes, c.Name), append(names, strings.ToLower(row.Cell(cols[end+"_name"])))
				}
			}
		}
		found, err := q.FindCIsByClassAndName(r.Context(), dbgen.FindCIsByClassAndNameParams{Classes: classes, Names: names})
		if err != nil {
			return err
		}
		items := make(map[string]uuid.UUID, len(found))
		ids := make([]uuid.UUID, 0, len(found))
		for _, c := range found {
			items[c.Class+"\x00"+c.Lname] = c.ID
			ids = append(ids, c.ID)
		}
		there, err := q.ListCIRelationshipsFrom(r.Context(), ids)
		if err != nil {
			return err
		}
		known := make(map[string]bool, len(there))
		for _, rel := range there {
			known[rel.FromCi.String()+rel.Type+rel.ToCi.String()] = true
		}
		var writes []dbgen.CreateCIRelationshipIfNewParams
		for _, row := range rows {
			out := importRowDTO{Row: row.Line, Action: "error"}
			var ends [2]uuid.UUID
			for i, end := range []string{"from", "to"} {
				cn, n := row.Cell(cols[end+"_class"]), row.Cell(cols[end+"_name"])
				c := h.classByCell(cn)
				switch {
				case out.Problem != "":
				case cn == "" || n == "":
					out.Problem = fmt.Sprintf("%s_class and %s_name are both needed", end, end)
				case c == nil:
					out.Problem = fmt.Sprintf("there is no class %q", cn)
				default:
					id, ok := items[c.Name+"\x00"+strings.ToLower(n)]
					if !ok {
						out.Problem = fmt.Sprintf("there is no %s called %s", strings.ToLower(c.Label), n)
					}
					ends[i] = id
				}
			}
			rel := h.relationshipByCell(row.Cell(cols["type"]))
			out.Name = row.Cell(cols["from_name"]) + " " + row.Cell(cols["type"]) + " " + row.Cell(cols["to_name"])
			switch {
			case out.Problem != "":
			case rel == nil:
				out.Problem = fmt.Sprintf("there is no relationship type %q", row.Cell(cols["type"]))
			case ends[0] == ends[1]:
				out.Problem = "an item cannot be related to itself"
			default:
				out.Name = row.Cell(cols["from_name"]) + " " + rel.Label + " " + row.Cell(cols["to_name"])
				key := ends[0].String() + rel.Name + ends[1].String()
				if known[key] {
					out.Action = "unchanged"
				} else {
					known[key] = true
					out.Action = "create"
					writes = append(writes, dbgen.CreateCIRelationshipIfNewParams{FromCi: ends[0], ToCi: ends[1], Type: rel.Name})
				}
			}
			rep.add(out)
		}
		if !apply || (rep.Counts["error"] > 0 && !skip) || len(writes) == 0 {
			rep.Applied = apply && (rep.Counts["error"] == 0 || skip)
			return nil
		}
		if err := q.QuietCIChanges(r.Context()); err != nil {
			return err
		}
		for _, wr := range writes {
			if _, err := q.CreateCIRelationshipIfNew(r.Context(), wr); err != nil {
				return err
			}
		}
		rep.Applied = true
		return q.NotifyCIBulk(r.Context())
	}) {
		return
	}
	if apply && !rep.Applied {
		writeJSON(w, http.StatusUnprocessableEntity, rep)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
