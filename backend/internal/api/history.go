package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/records"
)

const maxHistory = 500

// historyEntryDTO is one thing that happened to a record or an item. Values are given as stored (old, new) and as
// people read them (old_text, new_text: names rather than ids, labels rather than codes), resolved for the reader:
// something they may not see reads as "an item" or "a record".
type historyEntryDTO struct {
	ID int64     `json:"id"`
	At time.Time `json:"at"`
	// ActorName is who did it; ActorApp is set when an app did, for OnBehalfOfName. Both are empty for ServiceLater
	// itself (the SLA timer, say).
	ActorName      *string `json:"actor_name"`
	ActorApp       *string `json:"actor_app,omitempty"`
	OnBehalfOfName *string `json:"on_behalf_of_name,omitempty"`
	// Action is created, changed, linked, unlinked or deleted.
	Action string `json:"action"`
	// Field is what changed as stored (status, fields.category, ci...); Label is what people call it.
	Field string `json:"field"`
	Label string `json:"label"`
	Old   any    `json:"old"`
	New   any    `json:"new"`
	// OldText and NewText are the values as people read them.
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
	// Kind says how to show the values: text, time (RFC 3339, for the reader's time zone) or long (show the before and
	// after rather than inline).
	Kind string `json:"kind"`
}

// historyNames resolves the ids in a history to what the reader may see of them.
type historyNames struct {
	users, groups, cis, articles, tasks map[uuid.UUID]string
}

func idsIn(raw []byte, key string) []uuid.UUID {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	pick := func(x any) []uuid.UUID {
		if s, ok := x.(string); ok {
			if id, err := uuid.Parse(s); err == nil {
				return []uuid.UUID{id}
			}
		}
		return nil
	}
	if key == "" {
		return pick(v)
	}
	if m, ok := v.(map[string]any); ok {
		return pick(m[key])
	}
	return nil
}

func (h *handlers) resolveHistory(ctx context.Context, q *dbgen.Queries, rows []dbgen.ListHistoryRow) (historyNames, error) {
	n := historyNames{users: map[uuid.UUID]string{}, groups: map[uuid.UUID]string{}, cis: map[uuid.UUID]string{},
		articles: map[uuid.UUID]string{}, tasks: map[uuid.UUID]string{}}
	var users, groups, cis, articles, tasks []uuid.UUID
	for _, r := range rows {
		both := func(key string) []uuid.UUID { return append(idsIn(r.Old, key), idsIn(r.New, key)...) }
		switch r.Field {
		case "assignee_id", "requester_id":
			users = append(users, both("")...)
		case "assignment_group_id", "owner_group_id", "support_group_id":
			groups = append(groups, both("")...)
		case "ci":
			cis = append(cis, both("id")...)
		case "relationship":
			cis = append(cis, append(both("to"), both("from")...)...)
		case "article":
			articles = append(articles, both("id")...)
		case "ticket":
			tasks = append(tasks, both("id")...)
		case "problem_id":
			tasks = append(tasks, both("")...)
		case "":
			if r.Action == "created" {
				users = append(users, idsIn(r.New, "requester_id")...)
			}
		}
	}
	if len(users) > 0 {
		rows, err := q.UserNamesByID(ctx, users)
		if err != nil {
			return n, err
		}
		for _, u := range rows {
			n.users[u.ID] = u.Name
		}
	}
	if len(groups) > 0 {
		rows, err := q.GroupNamesByID(ctx, groups)
		if err != nil {
			return n, err
		}
		for _, g := range rows {
			n.groups[g.ID] = g.Name
		}
	}
	if len(cis) > 0 {
		rows, err := q.GetCIsByID(ctx, cis)
		if err != nil {
			return n, err
		}
		for _, c := range rows {
			n.cis[c.ID] = c.Name
		}
	}
	if len(articles) > 0 {
		rows, err := q.ArticleTitlesByID(ctx, articles)
		if err != nil {
			return n, err
		}
		for _, a := range rows {
			n.articles[a.ID] = articleRef(a.Number) + " " + a.Title
		}
	}
	if len(tasks) > 0 {
		rows, err := q.TaskRefsByID(ctx, tasks)
		if err != nil {
			return n, err
		}
		for _, t := range rows {
			n.tasks[t.ID] = t.Reference + " " + t.Title
		}
	}
	return n, nil
}

func decodeValue(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

// historyText is how a field's value reads.
type historyText func(v any) string

func plainText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "Yes"
		}
		return "No"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func namedText(names map[uuid.UUID]string, hidden string) historyText {
	return func(v any) string {
		s, _ := v.(string)
		if s == "" {
			return ""
		}
		id, err := uuid.Parse(s)
		if err != nil {
			return s
		}
		if name, ok := names[id]; ok {
			return name
		}
		return hidden
	}
}

func idText(names map[uuid.UUID]string, hidden string) historyText {
	return func(v any) string {
		m, _ := v.(map[string]any)
		if m == nil {
			return ""
		}
		return namedText(names, hidden)(m["id"])
	}
}

// fieldText describes a form field's values: option labels for a choice, Yes/No, times for the reader.
func fieldText(fd apps.FormField) (historyText, string) {
	kind := "text"
	switch fd.Type {
	case apps.FieldTextarea:
		kind = "long"
	case apps.FieldDateTime:
		kind = "time"
	}
	return func(v any) string {
		if fd.Type == apps.FieldChoice || fd.Type == apps.FieldPriority {
			s := plainText(v)
			for _, o := range fd.Options {
				if o.Value == s {
					return o.Label
				}
			}
			return s
		}
		return plainText(v)
	}, kind
}

func (h *handlers) toHistory(rows []dbgen.ListHistoryRow, describe func(r dbgen.ListHistoryRow) (label string, text historyText, kind string)) []historyEntryDTO {
	out := make([]historyEntryDTO, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- { // oldest first
		r := rows[i]
		e := historyEntryDTO{ID: r.ID, At: r.At.Time, ActorName: textPtr(r.ActorName), ActorApp: textPtr(r.ActorApp),
			OnBehalfOfName: textPtr(r.OnBehalfOfName), Action: r.Action, Field: r.Field, Old: decodeValue(r.Old), New: decodeValue(r.New)}
		label, text, kind := describe(r)
		e.Label, e.Kind = label, kind
		e.OldText, e.NewText = text(e.Old), text(e.New)
		out = append(out, e)
	}
	return out
}

// GET /tickets/{id}/history: what happened to a record, oldest first (the latest 500 changes): every field a person,
// a rule or a workflow changed, from what to what, and the items and articles linked to it. Anyone who may see the
// record sees its history.
func (h *handlers) getTicketHistory(w http.ResponseWriter, r *http.Request) {
	out := []historyEntryDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		t, err := loadTicket(r, q)
		if err != nil {
			return err
		}
		rows, err := q.ListHistory(r.Context(), dbgen.ListHistoryParams{Subject: "task", SubjectID: t.ID, Limit: maxHistory})
		if err != nil {
			return err
		}
		names, err := h.resolveHistory(r.Context(), q, rows)
		if err != nil {
			return err
		}
		item := ""
		if t.Type == "request" {
			full, err := records.Load(r.Context(), q, t.ID)
			if err != nil {
				return err
			}
			item = full.CatalogItem // its questions are labelled by its item's form
		}
		form, lc := h.apps.FormOf(t.Type, item), h.lifecycleFor(t.Type)
		out = h.toHistory(rows, func(row dbgen.ListHistoryRow) (string, historyText, string) {
			switch row.Field {
			case "":
				return "", func(v any) string {
					if m, ok := v.(map[string]any); ok {
						return plainText(m["title"])
					}
					return ""
				}, "text"
			case "status":
				return "Status", func(v any) string {
					if s, ok := v.(string); ok {
						return lc.Label(s)
					}
					return ""
				}, "text"
			case "assignee_id":
				return "Assignee", namedText(names.users, "someone"), "text"
			case "requester_id":
				return "Requester", namedText(names.users, "someone"), "text"
			case "assignment_group_id":
				return "Assignment group", namedText(names.groups, "a group"), "text"
			case "problem_id":
				return "Problem", namedText(names.tasks, "a problem"), "text"
			case "ci":
				return "Affected item", idText(names.cis, "an item"), "text"
			case "article":
				return "Article", idText(names.articles, "an article"), "text"
			case "sla_calendar":
				label := "SLA hours"
				var m struct{ SLA string }
				if json.Unmarshal(row.New, &m) == nil && m.SLA != "" {
					label = m.SLA + " hours"
				}
				return label, calendarText, "text"
			case "attachment":
				return "Attachment", func(v any) string {
					if m, ok := v.(map[string]any); ok {
						if s, ok := m["filename"].(string); ok {
							return s
						}
					}
					return ""
				}, "text"
			}
			name := strings.TrimPrefix(row.Field, "fields.")
			if fd, ok := form.Field(name); ok {
				text, kind := fieldText(fd)
				return fd.Label, text, kind
			}
			if apps.CoreFieldKind(name) == apps.FieldTextarea {
				return humanField(name), plainText, "long"
			}
			return humanField(name), plainText, "text"
		})
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// GET /cis/{id}/history (cmdb:read): what happened to a configuration item, oldest first (the latest 500 changes):
// its fields, its relationships and the records linked to it.
func (h *handlers) getCIHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	out := []historyEntryDTO{}
	if !h.tx(w, r, func(q *dbgen.Queries, _ auth.Principal) error {
		ci, err := q.GetCI(r.Context(), id)
		if err != nil {
			return err
		}
		rows, err := q.ListHistory(r.Context(), dbgen.ListHistoryParams{Subject: "ci", SubjectID: id, Limit: maxHistory})
		if err != nil {
			return err
		}
		names, err := h.resolveHistory(r.Context(), q, rows)
		if err != nil {
			return err
		}
		class := h.apps.Class(ci.Class)
		out = h.toHistory(rows, func(row dbgen.ListHistoryRow) (string, historyText, string) {
			switch row.Field {
			case "":
				return "", func(v any) string {
					if m, ok := v.(map[string]any); ok {
						return plainText(m["name"])
					}
					return ""
				}, "text"
			case "class":
				return "Class", func(v any) string { return h.classLabel(plainText(v)) }, "text"
			case "name":
				return "Name", plainText, "text"
			case "status":
				return "Status", func(v any) string {
					s := plainText(v)
					if i := slices.IndexFunc(apps.CIStatuses, func(o apps.Option) bool { return o.Value == s }); i >= 0 {
						return apps.CIStatuses[i].Label
					}
					return s
				}, "text"
			case "description":
				return "Description", plainText, "long"
			case "owner_group_id":
				return "Owned by", namedText(names.groups, "a group"), "text"
			case "support_group_id":
				return "Supported by", namedText(names.groups, "a group"), "text"
			case "ticket":
				return "Ticket", idText(names.tasks, "a record"), "text"
			case "relationship":
				return "Relationship", func(v any) string {
					m, _ := v.(map[string]any)
					if m == nil {
						return ""
					}
					typ := plainText(m["type"])
					label, other := typ, m["to"]
					if def := h.apps.Relationship(typ); def != nil {
						label = def.Label
						if other == nil {
							label = def.Reverse
						}
					}
					if other == nil {
						other = m["from"]
					}
					return label + " " + namedText(names.cis, "an item")(other)
				}, "text"
			}
			name := strings.TrimPrefix(row.Field, "fields.")
			if class != nil {
				if fd, ok := class.Field(name); ok {
					text, kind := fieldText(*fd)
					return fd.Label, text, kind
				}
			}
			return humanField(name), plainText, "text"
		})
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// humanField is the best name for a field nothing describes: planned_start becomes "Planned start".
func humanField(name string) string {
	s := strings.ReplaceAll(name, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// calendarText is an SLA clock's calendar as its history keeps it: the calendar's label then, else its name; none
// counts every hour.
func calendarText(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if s, ok := m["label"].(string); ok && s != "" {
		return s
	}
	if s, ok := m["calendar"].(string); ok && s != "" {
		return s
	}
	return "Every hour (24x7)"
}
