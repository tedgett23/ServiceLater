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
	bm, om := documentMap(&b), documentMap(&o)
	if bm == nil || om == nil {
		return nil, errors.New("both the shipped file and the override must be YAML maps to be combined")
	}
	return yaml.Marshal(mergeMaps(bm, om))
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
