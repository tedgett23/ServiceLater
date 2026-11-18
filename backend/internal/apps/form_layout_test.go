package apps

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

const columnsForm = `kind: form
record_type: incident
fields:
  category: { type: choice, label: Category, options: [hardware, network] }
  site: { type: text, label: Site }
  seats: { type: number, label: Seats }
  notes: { type: textarea, label: Notes }
layout:
  create:
    - fields: [title, description]
    - section: Where
      columns: [[category, site], [seats]]
  detail:
    - section: Where
      columns: [[site, seats], [category]]
    - section: More
      fields: [notes]
      wide: [notes]
`

// A section may place its fields in columns (each read down in turn), and the form for filing may have sections too;
// the plain list of fields asked when filing, and each section's fields, follow from them.
func TestSectionsCanHaveColumns(t *testing.T) {
	c, p := compile(t, formApp(columnsForm))
	if p != nil {
		t.Fatal(p)
	}
	var f *Form
	for _, d := range c.Definitions {
		if d.Form != nil {
			f = d.Form
		}
	}
	if !slices.Equal(f.Create, []string{"title", "description", "category", "site", "seats"}) {
		t.Errorf("create: %v", f.Create)
	}
	if len(f.CreateSections) != 2 || f.CreateSections[1].Label != "Where" || len(f.CreateSections[1].Columns) != 2 {
		t.Errorf("create sections: %+v", f.CreateSections)
	}
	if !slices.Equal(f.Detail[0].Fields, []string{"site", "seats", "category"}) || !slices.Equal(f.Detail[1].Wide, []string{"notes"}) {
		t.Errorf("detail: %+v", f.Detail)
	}

	// A plain list is one plain section, as before.
	c, p = compile(t, formApp(goodForm))
	if p != nil {
		t.Fatal(p)
	}
	for _, d := range c.Definitions {
		if d.Form != nil && (len(d.Form.CreateSections) != 1 || !d.Form.CreateSections[0].Plain()) {
			t.Errorf("a plain create list: %+v", d.Form.CreateSections)
		}
	}
}

func TestColumnMistakesFailTheInstall(t *testing.T) {
	with := func(old, new string) string { return strings.Replace(columnsForm, old, new, 1) }
	for name, tc := range map[string]struct{ src, want string }{
		"four columns": {with("columns: [[site, seats], [category]]", "columns: [[site], [seats], [category], []]"), "at most 3 columns"},
		"both":         {with("columns: [[site, seats], [category]]", "columns: [[site, seats], [category]]\n      fields: [site]"), "give fields or columns, not both"},
		"wide in cols": {with("columns: [[site, seats], [category]]", "columns: [[site, seats], [category]]\n      wide: [site]"), "wide is for a section without columns"},
		"wide outside": {with("wide: [notes]", "wide: [site]"), "wide names site, which is not in the section"},
		"twice":        {with("[[site, seats], [category]]", "[[site, seats], [category, site]]"), "site is on the record twice"},
		"mixed create": {with("    - fields: [title, description]\n", "    - title\n"), "create mixes field names and sections"},
	} {
		_, p := compile(t, formApp(tc.src))
		if p == nil || !strings.Contains(p.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, p)
		}
	}
}

// The form editor saves with FormYAML: every shipped form, and one with columns, written back must read as the same.
func TestFormsWriteBackAsTheyRead(t *testing.T) {
	var forms []Definition
	for _, app := range []string{"incident-management", "change-management", "problem-management"} {
		b, err := LoadBundled(app)
		if err != nil {
			t.Fatal(err)
		}
		c, err := b.Compile()
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range c.Definitions {
			if d.Form != nil {
				forms = append(forms, d)
			}
		}
	}
	c, p := compile(t, formApp(columnsForm))
	if p != nil {
		t.Fatal(p)
	}
	for _, d := range c.Definitions {
		if d.Form != nil {
			forms = append(forms, d)
		}
	}
	if len(forms) != 4 {
		t.Fatalf("%d forms", len(forms))
	}
	for _, d := range forms {
		data, err := FormYAML(d.Form, false)
		if err != nil {
			t.Fatal(err)
		}
		var p Problems
		again := parseForm(d.Path, data, &p)
		if again == nil {
			t.Fatalf("%s does not read back: %v\n%s", d.Path, p, data)
		}
		want, _ := json.Marshal(d.Form)
		got, _ := json.Marshal(again)
		if string(want) != string(got) {
			t.Errorf("%s changed on the way back:\nwant %s\n got %s", d.Path, want, got)
		}
	}
}

// An edit keeps only what changed: a new field and a section switched to columns, not the whole form.
func TestAFormOverrideIsOnlyTheChange(t *testing.T) {
	var p Problems
	base := []byte(goodForm)
	f := parseForm("forms/incident.yaml", base, &p)
	if f == nil {
		t.Fatal(p)
	}
	f.Fields = append(f.Fields, FormField{Name: "floor", Type: FieldNumber, Label: "Floor"})
	f.Detail[0] = Section{Label: "Where", Columns: [][]string{{"site", "floor"}, {"seats", "category"}}}
	f.normalizeLayout()
	out, err := FormOverride(base, f)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "override: replace") || !strings.Contains(s, "floor:") || !strings.Contains(s, "columns:") || strings.Contains(s, "visible_when") {
		t.Errorf("the override:\n%s", s)
	}
	merged, err := mergeYAML(base, out)
	if err != nil {
		t.Fatal(err)
	}
	if again := parseForm("", merged, &p); again == nil || len(again.Detail[0].Columns) != 2 {
		t.Errorf("merged: %v %s", p, merged)
	}
}
