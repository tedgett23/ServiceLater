package apps

import (
	"encoding/json"
	"strconv"

	"gopkg.in/yaml.v3"
)

// CatalogItemYAML is a catalog item as its file, keys in the order an author writes them. The layouts are always
// written out, so the order of the questions never depends on the order of a map. With replace, the file replaces
// the definition below it whole instead of merging into it.
func CatalogItemYAML(it *CatalogItem, replace bool) ([]byte, error) {
	doc := &yaml.Node{Kind: yaml.MappingNode}
	if replace {
		addScalar(doc, OverrideKey, OverrideReplace)
	}
	addScalar(doc, "kind", "catalog_item")
	addScalar(doc, "name", it.Name)
	addScalar(doc, "label", it.Label)
	addScalar(doc, "category", it.Category)
	for _, f := range []struct{ key, val string }{{"icon", it.Icon}, {"summary", it.Summary}, {"description", it.Description}} {
		if f.val != "" {
			addScalar(doc, f.key, f.val)
		}
	}
	if it.Order != 0 {
		doc.Content = append(doc.Content, scalar("order"), intNode(it.Order))
	}
	if a := it.AvailableTo; a != nil {
		m := &yaml.Node{Kind: yaml.MappingNode}
		for _, l := range []struct {
			key  string
			vals []string
		}{{"roles", a.Roles}, {"groups", a.Groups}} {
			if len(l.vals) > 0 {
				m.Content = append(m.Content, scalar(l.key), stringList(l.vals))
			}
		}
		doc.Content = append(doc.Content, scalar("available_to"), m)
	}
	if it.Title != nil {
		v, err := valueNode(it.Title)
		if err != nil {
			return nil, err
		}
		doc.Content = append(doc.Content, scalar("title"), v)
	}
	if it.Workflow != "" {
		addScalar(doc, "workflow", it.Workflow)
	}
	f := it.Form
	if f == nil {
		f = &Form{}
	}
	if err := appendFormBody(doc, f); err != nil {
		return nil, err
	}
	return yaml.Marshal(doc)
}

// FormYAML is a form as its file (forms/<type>.yaml). With replace, it replaces the form below it whole.
func FormYAML(f *Form, replace bool) ([]byte, error) {
	doc := &yaml.Node{Kind: yaml.MappingNode}
	if replace {
		addScalar(doc, OverrideKey, OverrideReplace)
	}
	addScalar(doc, "kind", "form")
	addScalar(doc, "record_type", f.RecordType)
	if err := appendFormBody(doc, f); err != nil {
		return nil, err
	}
	return yaml.Marshal(doc)
}

// FormOverride is what the studio layer holds for a form edited in the form editor, over base (the form as the
// layers below make it): only what changed, or the whole form with override: replace when the smallest override
// would not give exactly the edited form. It is nil when nothing changed.
func FormOverride(base []byte, f *Form) ([]byte, error) {
	want, err := FormYAML(f, false)
	if err != nil {
		return nil, err
	}
	diff, err := OverrideFor(base, want)
	if err != nil || diff == nil {
		return diff, err
	}
	if merged, err := mergeYAML(base, diff); err == nil && sameForm(merged, want) {
		return diff, nil
	}
	return FormYAML(f, true)
}

func sameForm(a, b []byte) bool {
	norm := func(data []byte) (string, bool) {
		var p Problems
		f := parseForm("", data, &p)
		if f == nil {
			return "", false
		}
		out, err := json.Marshal(f)
		return string(out), err == nil
	}
	x, ok1 := norm(a)
	y, ok2 := norm(b)
	return ok1 && ok2 && x == y
}

// sectionNode writes a section: its heading, then its fields or columns.
func sectionNode(s Section) *yaml.Node {
	sec := &yaml.Node{Kind: yaml.MappingNode}
	if s.Label != "" {
		addScalar(sec, "section", s.Label)
	}
	if len(s.Columns) > 0 {
		cols := &yaml.Node{Kind: yaml.SequenceNode}
		for _, c := range s.Columns {
			cols.Content = append(cols.Content, stringList(c))
		}
		sec.Content = append(sec.Content, scalar("columns"), cols)
		return sec
	}
	sec.Content = append(sec.Content, scalar("fields"), stringList(s.Fields))
	if len(s.Wide) > 0 {
		sec.Content = append(sec.Content, scalar("wide"), stringList(s.Wide))
	}
	return sec
}

// appendFormBody writes a form's fields, layouts and checks into a definition's document.
func appendFormBody(doc *yaml.Node, f *Form) error {
	fields := &yaml.Node{Kind: yaml.MappingNode}
	for _, fd := range f.Fields {
		n, err := fieldNode(fd)
		if err != nil {
			return err
		}
		fields.Content = append(fields.Content, scalar(fd.Name), n)
	}
	doc.Content = append(doc.Content, scalar("fields"), fields)
	layout := &yaml.Node{Kind: yaml.MappingNode}
	// A form for filing that is one plain section is written as the list it is.
	if len(f.CreateSections) > 1 || len(f.CreateSections) == 1 && !f.CreateSections[0].Plain() {
		create := &yaml.Node{Kind: yaml.SequenceNode}
		for _, s := range f.CreateSections {
			create.Content = append(create.Content, sectionNode(s))
		}
		layout.Content = append(layout.Content, scalar("create"), create)
	} else {
		layout.Content = append(layout.Content, scalar("create"), stringList(f.Create))
	}
	detail := &yaml.Node{Kind: yaml.SequenceNode}
	for _, s := range f.Detail {
		detail.Content = append(detail.Content, sectionNode(s))
	}
	layout.Content = append(layout.Content, scalar("detail"), detail)
	doc.Content = append(doc.Content, scalar("layout"), layout)
	if len(f.Validate) > 0 {
		checks := &yaml.Node{Kind: yaml.SequenceNode}
		for _, g := range f.Validate {
			c := &yaml.Node{Kind: yaml.MappingNode}
			addScalar(c, "when", g.When)
			addScalar(c, "error", g.Error)
			checks.Content = append(checks.Content, c)
		}
		doc.Content = append(doc.Content, scalar("validate"), checks)
	}
	return nil
}

func stringList(vals []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, v := range vals {
		n.Content = append(n.Content, scalar(v))
	}
	return n
}

// fieldNode writes one question with only what it sets.
func fieldNode(fd FormField) (*yaml.Node, error) {
	m := &yaml.Node{Kind: yaml.MappingNode}
	for _, f := range []struct{ key, val string }{{"type", fd.Type}, {"label", fd.Label}, {"help", fd.Help}} {
		if f.val != "" {
			addScalar(m, f.key, f.val)
		}
	}
	if len(fd.Options) > 0 {
		opts := &yaml.Node{Kind: yaml.SequenceNode}
		for _, o := range fd.Options {
			on := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
			addScalar(on, "value", o.Value)
			addScalar(on, "label", o.Label)
			if o.When != "" {
				addScalar(on, "when", o.When)
			}
			opts.Content = append(opts.Content, on)
		}
		m.Content = append(m.Content, scalar("options"), opts)
	}
	if fd.Required {
		m.Content = append(m.Content, scalar("required"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	}
	for _, f := range []struct{ key, val string }{{"required_when", fd.RequiredWhen}, {"visible_when", fd.VisibleWhen}, {"read_only_when", fd.ReadOnlyWhen}} {
		if f.val != "" {
			addScalar(m, f.key, f.val)
		}
	}
	if fd.Default != nil {
		v, err := valueNode(fd.Default)
		if err != nil {
			return nil, err
		}
		m.Content = append(m.Content, scalar("default"), v)
	}
	if fd.MaxLength != 0 {
		m.Content = append(m.Content, scalar("max_length"), intNode(fd.MaxLength))
	}
	for _, f := range []struct {
		key string
		val *int64
	}{{"min", fd.Min}, {"max", fd.Max}} {
		if f.val != nil {
			m.Content = append(m.Content, scalar(f.key), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(*f.val, 10)})
		}
	}
	if fd.History != nil {
		m.Content = append(m.Content, scalar("history"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(*fd.History)})
	}
	if fd.Mask {
		m.Content = append(m.Content, scalar("mask"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	}
	return m, nil
}

// CatalogItemOverride is what the studio layer holds for an item edited in the catalog editor, over base (the item
// as the layers below make it): only what changed, so the app's later changes to the rest still arrive. It is nil
// when nothing changed. If merging the smallest override would not give exactly the edited item, it is the whole item
// with override: replace instead.
func CatalogItemOverride(base []byte, it *CatalogItem) ([]byte, error) {
	want, err := CatalogItemYAML(it, false)
	if err != nil {
		return nil, err
	}
	diff, err := OverrideFor(base, want)
	if err != nil || diff == nil {
		return diff, err
	}
	if merged, err := mergeYAML(base, diff); err == nil && sameCatalogItem(merged, want) {
		return diff, nil
	}
	return CatalogItemYAML(it, true)
}

// sameCatalogItem reports whether two item files define the same item.
func sameCatalogItem(a, b []byte) bool {
	norm := func(data []byte) (string, bool) {
		var p Problems
		it := parseCatalogItem("", data, &p)
		if it == nil {
			return "", false
		}
		out, err := json.Marshal(it)
		return string(out), err == nil
	}
	x, ok1 := norm(a)
	y, ok2 := norm(b)
	return ok1 && ok2 && x == y
}
