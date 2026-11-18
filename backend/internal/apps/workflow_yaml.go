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
			{"approvers", s.Approvers}, {"groups", s.Groups}, {"rule", s.Rule}, {"workflow", s.Workflow}, {"items", s.Items}, {"each", s.Each},
			{"method", s.Method}, {"secret", s.Secret}, {"auth", s.Auth}, {"auth_header", s.AuthHeader}, {"record_type", s.RecordType},
			{"template", s.Template}, {"timeout", s.Timeout}} {
			if f.val != "" {
				addScalar(step, f.key, f.val)
			}
		}
		for _, list := range []struct {
			key  string
			vals []NamedValue
		}{{"vars", s.Vars}, {"set", s.Set}, {"inputs", s.Inputs}, {"headers", s.Headers}, {"fields", s.Fields}} {
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
		for _, f := range []struct {
			key string
			val *Value
		}{{"title", s.Title}, {"description", s.Description}, {"group", s.Group}, {"body", s.Body}, {"record", s.Record}, {"url", s.URL}, {"parent", s.Parent}} {
			if f.val == nil {
				continue
			}
			v, err := valueNode(f.val)
			if err != nil {
				return nil, fmt.Errorf("step %s: %s: %w", s.ID, f.key, err)
			}
			step.Content = append(step.Content, scalar(f.key), v)
		}
		if s.Wait != nil && !*s.Wait {
			step.Content = append(step.Content, scalar("wait"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"})
		}
		if s.Internal {
			step.Content = append(step.Content, scalar("internal"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
		}
		if s.KeepGoing {
			step.Content = append(step.Content, scalar("keep_going"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
		}
		for _, f := range []struct {
			key string
			val int
		}{{"parallel", s.Parallel}, {"retries", s.Retries}} {
			if f.val != 0 {
				step.Content = append(step.Content, scalar(f.key), intNode(f.val))
			}
		}
		if len(s.Recipients) > 0 {
			step.Content = append(step.Content, scalar("recipients"), stringList(s.Recipients))
		}
		if s.Find != nil {
			n, err := queryNode(s.Find)
			if err != nil {
				return nil, fmt.Errorf("step %s: find: %w", s.ID, err)
			}
			step.Content = append(step.Content, scalar("find"), n)
		}
		if s.Script != "" {
			// A block, so the script reads in the file as it was written.
			step.Content = append(step.Content, scalar("script"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.LiteralStyle,
				Value: strings.TrimRight(s.Script, "\n") + "\n"})
		}
		for _, f := range []struct{ key, val string }{{"next", s.Next}, {"then", s.Then}, {"else", s.Else}, {"on_timeout", s.OnTimeout}, {"on_error", s.OnError}} {
			if f.val != "" {
				addScalar(step, f.key, f.val)
			}
		}
		if len(s.Branches) > 0 {
			b := &yaml.Node{Kind: yaml.MappingNode}
			for _, name := range s.BranchNames() {
				b.Content = append(b.Content, scalar(name), scalar(s.Branches[name]))
			}
			step.Content = append(step.Content, scalar("branches"), b)
		}
		steps.Content = append(steps.Content, scalar(s.ID), step)
	}
	if sc := w.Schedule; sc != nil {
		n := &yaml.Node{Kind: yaml.MappingNode}
		addScalar(n, "cron", sc.Cron)
		if sc.Timezone != "" {
			addScalar(n, "timezone", sc.Timezone)
		}
		if sc.ForEach != nil {
			q, err := queryNode(sc.ForEach)
			if err != nil {
				return nil, fmt.Errorf("schedule: for_each: %w", err)
			}
			n.Content = append(n.Content, scalar("for_each"), q)
		}
		doc.Content = append(doc.Content, scalar("schedule"), n)
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

// queryNode writes a query's type, filters (in a fixed order) and limit.
func queryNode(q *RecordQuery) (*yaml.Node, error) {
	n := &yaml.Node{Kind: yaml.MappingNode}
	addScalar(n, "type", q.Type)
	if len(q.Where) > 0 {
		where := &yaml.Node{Kind: yaml.MappingNode}
		for _, field := range sortedKeys(q.Where) {
			ops := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
			for _, op := range sortedKeys(q.Where[field]) {
				v, err := valueNode(q.Where[field][op])
				if err != nil {
					return nil, fmt.Errorf("where.%s.%s: %w", field, op, err)
				}
				ops.Content = append(ops.Content, scalar(op), v)
			}
			where.Content = append(where.Content, scalar(field), ops)
		}
		n.Content = append(n.Content, scalar("where"), where)
	}
	if q.Limit != 0 {
		n.Content = append(n.Content, scalar("limit"), intNode(q.Limit))
	}
	return n, nil
}
