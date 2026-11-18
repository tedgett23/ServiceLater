package api_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

type formDef struct {
	RecordType string           `json:"record_type"`
	App        string           `json:"app"`
	Builtin    bool             `json:"builtin"`
	Shipped    bool             `json:"shipped"`
	Edited     bool             `json:"edited"`
	Override   string           `json:"override"`
	YAML       string           `json:"yaml"`
	Values     map[string]int64 `json:"values"`
	CoreFields []struct {
		Name string `json:"name"`
	} `json:"core_fields"`
	Form map[string]any `json:"form"`
}

// The form editor: a type's form is changed in its app's studio layer and is live at once. A type with no form gets
// one in the app that owns it, columns and all; a field taken off keeps its values; a field with values keeps its
// type; a change that breaks a rule is refused with the reason, and the check says so before saving.
func TestTheFormEditor(t *testing.T) {
	h, pool, reg := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	fay := newActor(t, h, pool, "fay@x.test", "end_user")
	reload := func() {
		t.Helper()
		if err := reg.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	expect(t, agent.do(h, "GET", "/api/v1/forms", ""), 403, "an agent opening the editor")
	customRole(t, h, pool, admin, "form-keeper", fay, "forms:manage")

	list := decodeBody[struct {
		Items []struct {
			RecordType  string `json:"record_type"`
			App         string `json:"app"`
			Builtin     bool   `json:"builtin"`
			Catalog     bool   `json:"catalog"`
			ExtraFields int    `json:"extra_fields"`
		} `json:"items"`
	}](t, fay.do(h, "GET", "/api/v1/forms", ""))
	byType := map[string]int{}
	for i, it := range list.Items {
		byType[it.RecordType] = i
	}
	if i, ok := byType["incident"]; !ok || list.Items[i].App != "incident-management" || list.Items[i].Builtin || list.Items[i].ExtraFields != 4 {
		t.Errorf("incident: %+v", list.Items)
	}
	if i, ok := byType["work_task"]; !ok || list.Items[i].App != "service-catalog" || !list.Items[i].Builtin {
		t.Errorf("tasks: %+v", list.Items)
	}
	if i, ok := byType["request"]; !ok || !list.Items[i].Catalog {
		t.Errorf("requests: %+v", list.Items)
	}
	expect(t, fay.do(h, "GET", "/api/v1/forms/request/definition", ""), 409, "a request's form")

	// Tasks have the built-in form; adding fields and a section in columns makes a form in service-catalog.
	def := decodeBody[formDef](t, fay.do(h, "GET", "/api/v1/forms/work_task/definition", ""))
	if !def.Builtin || def.Shipped || def.App != "service-catalog" || len(def.CoreFields) == 0 {
		t.Fatalf("the task form: %+v", def)
	}
	form := def.Form
	form["fields"] = append(form["fields"].([]any),
		map[string]any{"name": "category", "type": "choice", "label": "Category", "options": []any{
			map[string]any{"value": "hardware", "label": "Hardware"}, map[string]any{"value": "software", "label": "Software"}}},
		map[string]any{"name": "asset_tag", "type": "text", "label": "Hardware asset tag",
			"visible_when": `has(record.fields.category) && record.fields.category == "hardware"`},
		map[string]any{"name": "due_date", "type": "date", "label": "Due date"})
	form["create_sections"] = []any{map[string]any{"fields": append(form["create"].([]any), "category", "asset_tag")}}
	form["detail"] = append(form["detail"].([]any), map[string]any{"label": "Work", "columns": []any{[]any{"category", "asset_tag"}, []any{"due_date"}}})

	// The check finds mistakes before saving: a condition naming a field that is not there.
	bad, _ := json.Marshal(map[string]any{"form": withCondition(form, "asset_tag", `record.fields.categry == "hardware"`)})
	if p := decodeBody[struct{ Problems []string }](t, fay.do(h, "POST", "/api/v1/forms/work_task/definition/check", string(bad))).Problems; len(p) == 0 || !strings.Contains(strings.Join(p, " "), "categry") {
		t.Errorf("the check: %v", p)
	}
	good, _ := json.Marshal(map[string]any{"form": form})
	if p := decodeBody[struct{ Problems []string }](t, fay.do(h, "POST", "/api/v1/forms/work_task/definition/check", string(good))).Problems; len(p) != 0 {
		t.Fatalf("the check of a good form: %v", p)
	}
	w := fay.do(h, "PUT", "/api/v1/forms/work_task/definition", string(good))
	expect(t, w, 200, "saving the task form")
	saved := decodeBody[formDef](t, w)
	if saved.Builtin || !saved.Edited || saved.Shipped || !strings.Contains(saved.Override, "kind: form") || !strings.Contains(saved.Override, "columns:") {
		t.Errorf("saved: %+v\n%s", saved, saved.Override)
	}
	reload()
	public := fay.do(h, "GET", "/api/v1/forms/work_task", "").Body.String()
	if !strings.Contains(public, `"columns":[["category","asset_tag"],["due_date"]]`) || !strings.Contains(public, `"create_sections"`) {
		t.Errorf("not live: %s", public)
	}

	// Undo: the built-in form again.
	w = fay.do(h, "DELETE", "/api/v1/forms/work_task/edits", "")
	expect(t, w, 200, "undoing the task form")
	if d := decodeBody[formDef](t, w); !d.Builtin || d.Edited {
		t.Errorf("after undo: %+v", d)
	}

	// The incident form: values are counted; a field with values keeps its type; a field a rule reads stays.
	expect(t, admin.do(h, "POST", "/api/v1/tickets", `{"title":"Printer","fields":{"category":"hardware","subcategory":"printer"}}`), 201, "filing an incident")
	inc := decodeBody[formDef](t, fay.do(h, "GET", "/api/v1/forms/incident/definition", ""))
	if !inc.Shipped || inc.Builtin || inc.Values["category"] != 1 || inc.Values["subcategory"] != 1 {
		t.Fatalf("the incident form: %+v", inc)
	}
	retyped, _ := json.Marshal(map[string]any{"form": withField(inc.Form, "category", func(f map[string]any) { f["type"] = "text"; delete(f, "options") })})
	refusal(t, fay.do(h, "PUT", "/api/v1/forms/incident/definition", string(retyped)), 422, "category holds values in 1 records, so it stays a choice field", "changing a field's type")
	noUrgency, _ := json.Marshal(map[string]any{"form": without(inc.Form, "urgency")})
	refusal(t, fay.do(h, "PUT", "/api/v1/forms/incident/definition", string(noUrgency)), 422, "urgency", "taking off a field a rule reads")

	// Taking subcategory off saves, as only that change, and the incident keeps its value.
	noSub, _ := json.Marshal(map[string]any{"form": without(inc.Form, "subcategory")})
	w = fay.do(h, "PUT", "/api/v1/forms/incident/definition", string(noSub))
	expect(t, w, 200, "taking off subcategory")
	if o := decodeBody[formDef](t, w).Override; !strings.Contains(o, "subcategory: null") || strings.Contains(o, "Laptop or desktop") {
		t.Errorf("the override is not just the removal:\n%s", o)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM tasks WHERE fields->>'subcategory' = 'printer'`); n != 1 {
		t.Errorf("the value was not kept: %d", n)
	}
	if v := decodeBody[formDef](t, fay.do(h, "GET", "/api/v1/forms/incident/definition", "")).Values; v["subcategory"] != 1 {
		t.Errorf("values after removal: %v", v)
	}
	expect(t, fay.do(h, "DELETE", "/api/v1/forms/incident/edits", ""), 200, "undoing the incident form")
}

// withField changes one field of a form (as JSON) with fn.
func withField(form map[string]any, name string, fn func(map[string]any)) map[string]any {
	out := cloneJSON(form)
	for _, f := range out["fields"].([]any) {
		if m := f.(map[string]any); m["name"] == name {
			fn(m)
		}
	}
	return out
}

func withCondition(form map[string]any, name, cond string) map[string]any {
	return withField(form, name, func(f map[string]any) { f["visible_when"] = cond })
}

// without takes a field off a form (as JSON): its definition and its places in the layouts.
func without(form map[string]any, name string) map[string]any {
	out := cloneJSON(form)
	out["fields"] = slices.DeleteFunc(out["fields"].([]any), func(f any) bool { return f.(map[string]any)["name"] == name })
	drop := func(list []any) []any { return slices.DeleteFunc(list, func(x any) bool { return x == name }) }
	for _, key := range []string{"create_sections", "detail"} {
		for _, s := range out[key].([]any) {
			sec := s.(map[string]any)
			sec["fields"] = drop(sec["fields"].([]any))
			if cols, ok := sec["columns"].([]any); ok {
				for i, c := range cols {
					cols[i] = drop(c.([]any))
				}
			}
		}
	}
	out["create"] = drop(out["create"].([]any))
	return out
}

func cloneJSON(v map[string]any) map[string]any {
	raw, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}
