package apps

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// WorkflowYAML is a workflow as its file: the keys in the order an author writes them, each step with only the fields
// its type uses. It is what the workflow builder saves into the studio layer; with replace, the file replaces the
// shipped definition of the same name whole instead of merging into it (see mergeYAML).
func WorkflowYAML(w *Workflow, replace bool) ([]byte, error) {
	doc := &yaml.Node{Kind: yaml.MappingNode}
	if replace {
		addScalar(doc, OverrideKey, OverrideReplace)
	}
	addScalar(doc, "kind", "workflow")
	addScalar(doc, "name", w.Name)
	addScalar(doc, "record_type", w.RecordType)
	addScalar(doc, "start", w.Start)
	steps := &yaml.Node{Kind: yaml.MappingNode}
	for _, s := range w.Steps {
		step := &yaml.Node{Kind: yaml.MappingNode}
		addScalar(step, "type", s.Type)
		for _, f := range []struct{ key, val string }{{"if", s.If}, {"for", s.For}, {"until", s.Until}, {"to", s.To},
			{"approvers", s.Approvers}, {"rule", s.Rule}, {"timeout", s.Timeout}} {
			if f.val != "" {
				addScalar(step, f.key, f.val)
			}
		}
		for _, list := range []struct {
			key  string
			vals []NamedValue
		}{{"vars", s.Vars}, {"set", s.Set}} {
			if len(list.vals) == 0 {
				continue
			}
			m := &yaml.Node{Kind: yaml.MappingNode}
			for _, nv := range list.vals {
				v, err := valueNode(nv.Value)
				if err != nil {
					return nil, fmt.Errorf("step %s: %s: %w", s.ID, nv.Name, err)
				}
				m.Content = append(m.Content, scalar(nv.Name), v)
			}
			step.Content = append(step.Content, scalar(list.key), m)
		}
		if s.Body != nil {
			v, err := valueNode(s.Body)
			if err != nil {
				return nil, fmt.Errorf("step %s: body: %w", s.ID, err)
			}
			step.Content = append(step.Content, scalar("body"), v)
		}
		if s.Internal {
			step.Content = append(step.Content, scalar("internal"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
		}
		for _, f := range []struct{ key, val string }{{"next", s.Next}, {"then", s.Then}, {"else", s.Else}, {"on_timeout", s.OnTimeout}} {
			if f.val != "" {
				addScalar(step, f.key, f.val)
			}
		}
		steps.Content = append(steps.Content, scalar(s.ID), step)
	}
	doc.Content = append(doc.Content, scalar("steps"), steps)
	if len(w.Layout) > 0 {
		layout := &yaml.Node{Kind: yaml.MappingNode}
		for _, s := range w.Steps {
			if p, ok := w.Layout[s.ID]; ok {
				pos := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
				pos.Content = append(pos.Content, scalar("x"), intNode(p.X), scalar("y"), intNode(p.Y))
				layout.Content = append(layout.Content, scalar(s.ID), pos)
			}
		}
		doc.Content = append(doc.Content, scalar("layout"), layout)
	}
	return yaml.Marshal(doc)
}

func scalar(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }

func intNode(i int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(i)}
}

func addScalar(m *yaml.Node, key, val string) {
	m.Content = append(m.Content, scalar(key), scalar(val))
}

// valueNode writes a value as UnmarshalYAML reads it back: an expression as =text, a literal as its own type.
func valueNode(v *Value) (*yaml.Node, error) {
	if v == nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	if v.Expr != "" {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "=" + v.Expr, Style: yaml.DoubleQuotedStyle}, nil
	}
	switch l := v.Literal.(type) {
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: l, Style: yaml.DoubleQuotedStyle}, nil
	case int64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(l, 10)}, nil
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(l)}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(l)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	return nil, fmt.Errorf("a value is text, a whole number, true or false, or an =expression")
}

// WorkflowOverride is what the studio layer holds for a workflow edited in the builder, over base (the workflow as
// the layers below make it): only what changed, as an override that merges, so whatever the app changes later in the
// parts the edit left alone still arrives on upgrade. It is nil when the edit changes nothing. If merging the smallest
// override would not give exactly the edited workflow, it is the whole workflow with override: replace instead.
func WorkflowOverride(base []byte, w *Workflow) ([]byte, error) {
	want, err := WorkflowYAML(w, false)
	if err != nil {
		return nil, err
	}
	diff, err := OverrideFor(base, want)
	if err != nil || diff == nil {
		return diff, err
	}
	if merged, err := mergeYAML(base, diff); err == nil && sameWorkflow(merged, want) {
		return diff, nil
	}
	return WorkflowYAML(w, true)
}

// sameWorkflow reports whether two workflow files define the same workflow, whatever their key and step order.
func sameWorkflow(a, b []byte) bool {
	norm := func(data []byte) (string, bool) {
		var f workflowFile
		if decodeStrict(data, &f) != nil {
			return "", false
		}
		steps := slices.Clone([]Step(f.Steps))
		slices.SortFunc(steps, func(x, y Step) int { return strings.Compare(x.ID, y.ID) })
		out, err := json.Marshal(Workflow{Name: f.Name, RecordType: f.RecordType, Start: f.Start, Steps: steps, Layout: f.Layout})
		return string(out), err == nil
	}
	x, ok1 := norm(a)
	y, ok2 := norm(b)
	return ok1 && ok2 && x == y
}
