package apps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

const goodForm = `kind: form
record_type: incident
fields:
  category:
    type: choice
    label: Category
    options: [hardware, { value: network, label: Network }]
  site:
    type: text
    label: Site
    visible_when: has(record.fields.category) && record.fields.category == "hardware"
  seats: { type: number, label: Seats, min: 1, default: 1 }
  description:
    label: What happened
    required_when: record.priority <= 2
layout:
  create: [title, description, category, site, seats]
  detail:
    - section: Where
      fields: [site, seats, category]
`

const fieldRule = `kind: rule
name: route-network
record_type: incident
timing: before
on: [create]
when: has(record.fields.category) && record.fields.category == "network"
run:
  - action: set
    field: fields.site
    value: Data centre
`

func formApp(form string, more ...string) map[string]string {
	files := map[string]string{"app.yaml": goodManifest, "default/forms/incident.yaml": form}
	for i := 0; i+1 < len(more); i += 2 {
		files[more[i]] = more[i+1]
	}
	return files
}

// A form compiles to its fields (extra and core), its layouts and its conditions, and the rules of the same app are
// checked against the fields it declares and keep their types, so they compile the same way from storage.
func TestAFormCompilesAndItsAppsRulesUseItsFields(t *testing.T) {
	c, p := compile(t, formApp(goodForm, "default/rules/route.yaml", fieldRule))
	if p != nil {
		t.Fatal(p)
	}
	var f *Form
	var r *Rule
	for _, d := range c.Definitions {
		if d.Form != nil {
			f = d.Form
		}
		if d.Rule != nil {
			r = d.Rule
		}
	}
	if f == nil || r == nil {
		t.Fatal("missing definitions")
	}
	if d, ok := f.Field("description"); !ok || !d.Core() || d.Type != FieldTextarea || d.Label != "What happened" {
		t.Errorf("a core field with the form's label: %+v", d)
	}
	if o := f.byName["category"].Options; len(o) != 2 || o[0].Label != "hardware" || o[1].Label != "Network" {
		t.Errorf("options: %+v", o)
	}
	if got := f.Reactive(); strings.Join(got, ",") != "category,priority" {
		t.Errorf("reactive: %v", got)
	}
	if v := f.Defaults(); v["seats"] != int64(1) {
		t.Errorf("defaults: %v", v)
	}
	if len(r.Fields) != 2 || r.Fields["category"] != expr.FieldText || r.Fields["site"] != expr.FieldText {
		t.Errorf("the rule keeps the fields it uses: %v", r.Fields)
	}

	// Stored and read back, the form and the rule compile as they did.
	var back Form
	if err := json.Unmarshal([]byte(mustJSON(t, f)), &back); err != nil {
		t.Fatal(err)
	}
	back.compile()
	in := expr.Input{Record: &expr.Record{Priority: 1, Fields: map[string]any{"category": "network"}}, Old: &expr.Record{}, User: &expr.User{}}
	st, err := back.States(context.Background(), in)
	if err != nil || !st["site"].Hidden || !st["description"].Required || st["category"].Required {
		t.Errorf("states: %+v %v", st, err)
	}
	var rb Rule
	if err := json.Unmarshal([]byte(mustJSON(t, r)), &rb); err != nil {
		t.Fatal(err)
	}
	rb.compile()
	if prog, err := rb.Condition(); err != nil || prog == nil {
		t.Errorf("stored rule: %v", err)
	}
}

func TestFormsAreCheckedAtInstall(t *testing.T) {
	with := func(old, new string) string { return strings.Replace(goodForm, old, new, 1) }
	for name, tc := range map[string]struct{ src, want string }{
		"task form":       {with("record_type: incident", "record_type: task"), "forms for task are not supported yet"},
		"bad type":        {with("type: text", "type: colour"), `type must be one of text, textarea, number, boolean, date, datetime, choice, got "colour"`},
		"no type":         {with("    type: text\n", ""), "an extra field needs a type"},
		"bad name":        {with("  site:\n", "  Site:\n"), "a field name is 1-40 characters"},
		"core name":       {with("  site:\n", "  status:\n"), "have a field of their own called status"},
		"core type":       {with("    label: What happened\n", "    label: What happened\n    type: text\n"), "a form can change its label, help and when"},
		"no options":      {with("    options: [hardware, { value: network, label: Network }]\n", ""), "a choice needs options"},
		"dup option":      {with("[hardware, {", "[hardware, hardware, {"), `option "hardware" is listed twice`},
		"options on text": {with("    label: Site\n", "    label: Site\n    options: [a]\n"), "only a choice has options"},
		"min on text":     {with("    label: Site\n", "    label: Site\n    min: 1\n"), "only a number has min and max"},
		"bad default":     {with("default: 1", "default: one"), "default: takes a whole number"},
		"bad condition":   {with("record.priority <= 2", "record.prioity <= 2"), "fields: description: required_when:"},
		"unknown field":   {with(`record.fields.category == "hardware"`, `record.fields.categry == "hardware"`), "undefined field 'categry'"},
		"lookups":         {with("record.priority <= 2", "lookups.open > 1"), "lookups are not available in a form"},
		"not laid out":    {strings.Replace(with("[site, seats, category]", "[site, category]"), "site, seats]", "site]", 1), "seats is in neither layout"},
		"layout unknown":  {with("[title, description, category, site, seats]", "[title, description, category, site, seats, colour]"), `"colour" is not a field of incident records`},
		"layout twice":    {with("[site, seats, category]", "[site, seats, category, site]"), "site is on the record twice"},
		"no title":        {with("[title, description, ", "[description, "), "layout: create must include title"},
		"not on create":   {with("[title, description, ", "[title, resolution, description, "), "resolution cannot be filled in when filing"},
		"core in detail":  {with("[site, seats, category]", "[site, seats, category, priority]"), "priority has a place of its own"},
		"bad check":       {goodForm + "validate:\n  - when: record.title\n    error: x\n", "validate[0]: when: the expression must produce true or false"},
		"check no error":  {goodForm + "validate:\n  - when: \"true\"\n", "needs when (what must hold) and error"},
	} {
		_, p := compile(t, formApp(tc.src))
		if p == nil || !strings.Contains(p.Error(), tc.want) {
			t.Errorf("%s: want %q in %v", name, tc.want, p)
		}
	}
	// A required field must be on the form for filing, or have a default.
	required := strings.Replace(strings.Replace(goodForm, "    label: Site\n", "    label: Site\n    required: true\n", 1),
		"[title, description, category, site, seats]", "[title, description, category, seats]", 1)
	if _, p := compile(t, formApp(required)); p == nil || !strings.Contains(p.Error(), "site is required, so it must be on layout.create") {
		t.Errorf("required but not on create: %v", p)
	}

	// Other definitions are checked against the app's own form for their record type.
	for name, tc := range map[string]struct{ rule, want string }{
		"undeclared":   {strings.Replace(fieldRule, `record.fields.category == "network"`, `record.fields.colour == "red"`, 1), "undefined field 'colour'"},
		"wrong type":   {strings.Replace(fieldRule, `record.fields.category == "network"`, `record.fields.seats == "2"`, 1), "no matching overload"},
		"set unknown":  {strings.Replace(fieldRule, "field: fields.site", "field: fields.floor", 1), `has no field "floor"`},
		"set as other": {strings.Replace(fieldRule, "value: Data centre", "value: =record.fields.seats", 1), "must produce text"},
		"other type":   {strings.Replace(fieldRule, "record_type: incident", "record_type: change", 1), "undefined field 'category'"},
	} {
		_, p := compile(t, formApp(goodForm, "default/rules/route.yaml", tc.rule))
		if p == nil || !strings.Contains(p.Error(), tc.want) {
			t.Errorf("%s: want %q in %v", name, tc.want, p)
		}
	}
	// Without the form, the same rule cannot read the field.
	if _, p := compile(t, map[string]string{"app.yaml": goodManifest, "default/rules/route.yaml": fieldRule}); p == nil ||
		!strings.Contains(p.Error(), "this app's form") {
		t.Errorf("a rule in an app without the form: %v", p)
	}
}

// Without an app's form a record type has one made of its own fields, so the UI always has a form to show.
func TestEveryTypeHasAForm(t *testing.T) {
	f := FormFor("change")
	if strings.Join(f.Create, ",") != "title,description,priority,change_type" || len(f.Detail) != 1 || len(f.Detail[0].Fields) != 8 {
		t.Errorf("change: %+v", f)
	}
	if f := FormFor("incident"); strings.Join(f.Detail[0].Fields, ",") != "resolution" {
		t.Errorf("incident: %+v", f.Detail)
	}
	if f := FormFor("problem"); len(f.Detail) != 0 || len(f.Create) != 3 {
		t.Errorf("problem: %+v", f)
	}
}
