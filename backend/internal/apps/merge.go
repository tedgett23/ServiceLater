package apps

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// mergeYAML applies a local override on top of a shipped definition.
//
//   - Maps merge key by key, recursively, so a local file can change one label and leave the rest to the shipped
//     file. Shipped changes to anything the override does not mention flow through on upgrade.
//   - Anything else (a scalar, or a list such as `transitions`) is replaced as a whole.
//   - A key set to null in the override removes that key, which is how a local file deletes a state.
//   - Keys the shipped file does not have are added after the shipped ones, so ordering stays stable.
//   - An override whose top level says `override: replace` is not merged: it replaces the shipped definition whole
//     (that key removed). The workflow builder writes these, so a step it deletes is gone rather than merged back.
func mergeYAML(base, over []byte) ([]byte, error) {
	var b, o yaml.Node
	if err := yaml.Unmarshal(base, &b); err != nil {
		return nil, fmt.Errorf("the shipped file is not valid YAML: %w", err)
	}
	if err := yaml.Unmarshal(over, &o); err != nil {
		return nil, fmt.Errorf("the override is not valid YAML: %w", err)
	}
	if len(o.Content) == 0 { // an empty override changes nothing
		return base, nil
	}
	om := documentMap(&o)
	if om != nil && replaces(om) {
		return yaml.Marshal(withoutKey(om, OverrideKey))
	}
	bm := documentMap(&b)
	if bm == nil || om == nil {
		return nil, errors.New("both the shipped file and the override must be YAML maps to be combined")
	}
	return yaml.Marshal(mergeMaps(bm, om))
}

// OverrideKey, set to OverrideReplace at the top of an override, makes it replace the shipped definition.
const (
	OverrideKey     = "override"
	OverrideReplace = "replace"
)

func replaces(m *yaml.Node) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == OverrideKey {
			return m.Content[i+1].Value == OverrideReplace
		}
	}
	return false
}

func withoutKey(m *yaml.Node, key string) *yaml.Node {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: m.Tag, Style: m.Style}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			out.Content = append(out.Content, m.Content[i], m.Content[i+1])
		}
	}
	return out
}

func documentMap(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

func isNull(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.Tag == "!!null" }

func mergeMaps(base, over *yaml.Node) *yaml.Node {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: base.Tag, Style: base.Style, Content: append([]*yaml.Node{}, base.Content...)}
	find := func(key string) int {
		for i := 0; i+1 < len(out.Content); i += 2 {
			if out.Content[i].Value == key {
				return i
			}
		}
		return -1
	}
	for i := 0; i+1 < len(over.Content); i += 2 {
		key, val := over.Content[i], over.Content[i+1]
		at := find(key.Value)
		switch {
		case isNull(val):
			if at >= 0 {
				out.Content = append(out.Content[:at], out.Content[at+2:]...)
			}
		case at >= 0 && out.Content[at+1].Kind == yaml.MappingNode && val.Kind == yaml.MappingNode:
			out.Content[at+1] = mergeMaps(out.Content[at+1], val)
		case at >= 0:
			out.Content[at+1] = val
		default:
			out.Content = append(out.Content, key, val)
		}
	}
	return out
}

// withoutReplace removes `override: replace` from a file that has nothing below it to replace (a definition added in
// the studio layer); ok is false when the file does not say it.
func withoutReplace(data []byte) ([]byte, bool) {
	var n yaml.Node
	if yaml.Unmarshal(data, &n) != nil {
		return nil, false
	}
	m := documentMap(&n)
	if m == nil || !replaces(m) {
		return nil, false
	}
	out, err := yaml.Marshal(withoutKey(m, OverrideKey))
	return out, err == nil
}

// OverrideFor is the smallest override that, merged over base by mergeYAML, gives want: the keys whose values differ
// (maps recursively, anything else whole), null for keys want no longer has. It is nil when want and base are the
// same, so there is nothing to override. Both must be YAML maps.
func OverrideFor(base, want []byte) ([]byte, error) {
	var b, w yaml.Node
	if err := yaml.Unmarshal(base, &b); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(want, &w); err != nil {
		return nil, err
	}
	bm, wm := documentMap(&b), documentMap(&w)
	if bm == nil || wm == nil {
		return nil, errors.New("both files must be YAML maps")
	}
	diff := diffMaps(bm, wm)
	if diff == nil {
		return nil, nil
	}
	return yaml.Marshal(diff)
}

// diffMaps is what over must say for mergeMaps(base, over) to equal want, or nil when they are equal already.
func diffMaps(base, want *yaml.Node) *yaml.Node {
	out := &yaml.Node{Kind: yaml.MappingNode}
	baseAt := map[string]*yaml.Node{}
	for i := 0; i+1 < len(base.Content); i += 2 {
		baseAt[base.Content[i].Value] = base.Content[i+1]
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(want.Content); i += 2 {
		key, val := want.Content[i], want.Content[i+1]
		seen[key.Value] = true
		was, ok := baseAt[key.Value]
		switch {
		case !ok:
			out.Content = append(out.Content, key, val)
		case was.Kind == yaml.MappingNode && val.Kind == yaml.MappingNode:
			if d := diffMaps(was, val); d != nil {
				out.Content = append(out.Content, key, d)
			}
		case !sameNode(was, val):
			out.Content = append(out.Content, key, val)
		}
	}
	for i := 0; i+1 < len(base.Content); i += 2 {
		if key := base.Content[i]; !seen[key.Value] {
			out.Content = append(out.Content, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"})
		}
	}
	if len(out.Content) == 0 {
		return nil
	}
	return out
}

// sameNode compares two YAML values by what they mean, not how they are written (quotes, flow style, comments).
func sameNode(a, b *yaml.Node) bool {
	if a.Kind == yaml.AliasNode {
		a = a.Alias
	}
	if b.Kind == yaml.AliasNode {
		b = b.Alias
	}
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case yaml.ScalarNode:
		return a.ShortTag() == b.ShortTag() && a.Value == b.Value
	case yaml.MappingNode:
		return diffMaps(a, b) == nil && diffMaps(b, a) == nil
	case yaml.SequenceNode:
		if len(a.Content) != len(b.Content) {
			return false
		}
		for i := range a.Content {
			if !sameNode(a.Content[i], b.Content[i]) {
				return false
			}
		}
		return true
	}
	return false
}
