package apps

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// The CMDB's definition kinds (docs/apps-platform.md §8.2). A class says what a configuration item can be and which
// fields it has (classes/); a relationship type says how two items can relate, and whether trouble spreads along it
// (relationships/). The items themselves are data people edit (the cis table), not definitions.

// CIStatuses are the statuses every configuration item can have, in the order they are offered. The cis table's
// CHECK lists the same values.
var CIStatuses = []Option{{Value: "planned", Label: "Planned"}, {Value: "active", Label: "Active"},
	{Value: "maintenance", Label: "In maintenance"}, {Value: "retired", Label: "Retired"}}

// ciColumns are what every item has besides its class's fields.
var ciColumns = []string{"id", "class", "name", "status", "description", "owner_group", "owner_group_id",
	"support_group", "support_group_id", "fields", "created_at", "updated_at", "created_by"}

// cmdbNameRE is a class or relationship type name, as the cis and ci_relationships CHECKs allow.
var cmdbNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{1,39}$`)

const (
	maxClassFields = 100
	maxClassDepth  = 5
)

// Class is a kind of configuration item. It may extend another class of the same app, and then has that class's
// fields before its own; an item of a class also counts as one of each class it extends (filtering servers by
// hardware finds them). Operators add classes, or fields to shipped ones, in the app's local layer.
type Class struct {
	Name        string      `json:"name"`
	Label       string      `json:"label"`
	Description string      `json:"description,omitempty"`
	Extends     string      `json:"extends,omitempty"`
	Fields      []FormField `json:"fields,omitempty"` // its own, in order

	App     string `json:"-"`
	Version string `json:"-"`
	// Ancestors are the classes it extends, nearest first, and All its fields with theirs first. The registry works
	// them out, since a class's parent is a separate definition.
	Ancestors []string    `json:"-"`
	All       []FormField `json:"-"`
}

// Field finds one of the class's fields, its own or inherited.
func (c *Class) Field(name string) (*FormField, bool) {
	for i := range c.All {
		if c.All[i].Name == name {
			return &c.All[i], true
		}
	}
	return nil, false
}

// Defaults are the fields a new item of the class starts with.
func (c *Class) Defaults() map[string]any {
	out := map[string]any{}
	for _, fd := range c.All {
		if fd.Default != nil && fd.Default.Literal != nil {
			if v, msg := fd.Check(fd.Default.Literal); msg == "" {
				out[fd.Name] = v
			}
		}
	}
	return out
}

// Relationship is a way two items can relate, read from the first to the second: "payroll depends on postgres-01",
// and backwards with Reverse: "postgres-01 is used by payroll". With Impact, trouble with the second item affects the
// first: the impact of an outage follows such relationships backwards.
type Relationship struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Reverse string `json:"reverse"`
	Impact  bool   `json:"impact,omitempty"`

	App     string `json:"-"`
	Version string `json:"-"`
}

// ---- YAML ----

type classFile struct {
	Kind        string        `yaml:"kind"`
	Name        string        `yaml:"name"`
	Label       string        `yaml:"label"`
	Description string        `yaml:"description"`
	Extends     string        `yaml:"extends"`
	Fields      orderedFields `yaml:"fields"`
}

func parseClass(file string, data []byte, p *Problems) *Class {
	var f classFile
	if err := decodeStrict(data, &f); err != nil {
		p.add(file, "%v", err)
		return nil
	}
	if f.Kind != "class" {
		p.add(file, "kind must be class in this folder, got %q", f.Kind)
		return nil
	}
	c := &Class{Name: f.Name, Label: strings.TrimSpace(f.Label), Description: f.Description, Extends: f.Extends, Fields: f.Fields}
	before := len(*p)
	c.validate(file, p)
	if len(*p) > before {
		return nil
	}
	return c
}

func (c *Class) validate(file string, p *Problems) {
	if !cmdbNameRE.MatchString(c.Name) {
		p.add(file, "name %q must be 2-40 characters: lowercase letters, digits and underscores, starting with a letter", c.Name)
	}
	if n := utf8.RuneCountInString(c.Label); n < 1 || n > maxLabel {
		p.add(file, "label is required: what people see, at most %d characters", maxLabel)
	}
	if utf8.RuneCountInString(c.Description) > maxHelp {
		p.add(file, "description must be at most %d characters", maxHelp)
	}
	if c.Extends == c.Name && c.Name != "" {
		p.add(file, "extends: a class cannot extend itself")
	}
	if len(c.Fields) > maxClassFields {
		p.add(file, "a class may have at most %d fields, this has %d", maxClassFields, len(c.Fields))
	}
	seen := map[string]bool{}
	for i := range c.Fields {
		fd := &c.Fields[i]
		at := "fields: " + fd.Name
		if seen[fd.Name] {
			p.add(file, "%s: listed twice", at)
		}
		seen[fd.Name] = true
		fd.validateExtra("configuration items", func(n string) bool { return slices.Contains(ciColumns, n) }, at, file, p)
		if utf8.RuneCountInString(fd.Label) > maxLabel {
			p.add(file, "%s: label is longer than %d characters", at, maxLabel)
		}
		if utf8.RuneCountInString(fd.Help) > maxHelp {
			p.add(file, "%s: help is longer than %d characters", at, maxHelp)
		}
		// An item is not a record: there is nothing for a condition to read yet.
		if fd.RequiredWhen != "" || fd.VisibleWhen != "" || fd.ReadOnlyWhen != "" ||
			slices.ContainsFunc(fd.Options, func(o Option) bool { return o.When != "" }) {
			p.add(file, "%s: a class's fields have no conditions (required_when, visible_when, read_only_when or an option's when)", at)
		}
	}
}

type relationshipFile struct {
	Kind    string `yaml:"kind"`
	Name    string `yaml:"name"`
	Label   string `yaml:"label"`
	Reverse string `yaml:"reverse"`
	Impact  bool   `yaml:"impact"`
}

func parseRelationship(file string, data []byte, p *Problems) *Relationship {
	var f relationshipFile
	if err := decodeStrict(data, &f); err != nil {
		p.add(file, "%v", err)
		return nil
	}
	if f.Kind != "relationship" {
		p.add(file, "kind must be relationship in this folder, got %q", f.Kind)
		return nil
	}
	r := &Relationship{Name: f.Name, Label: strings.TrimSpace(f.Label), Reverse: strings.TrimSpace(f.Reverse), Impact: f.Impact}
	before := len(*p)
	if !cmdbNameRE.MatchString(r.Name) {
		p.add(file, "name %q must be 2-40 characters: lowercase letters, digits and underscores, starting with a letter", r.Name)
	}
	for _, l := range []struct{ what, v string }{{"label", r.Label}, {"reverse", r.Reverse}} {
		if n := utf8.RuneCountInString(l.v); n < 1 || n > maxLabel {
			p.add(file, "%s is required (how the relationship reads %s), at most %d characters", l.what,
				map[string]string{"label": "from the first item", "reverse": "from the second"}[l.what], maxLabel)
		}
	}
	if len(*p) > before {
		return nil
	}
	return r
}

// checkClasses checks what one class says about another: each extends a class of the same app, without a loop, no
// deeper than maxClassDepth, and never declares a field a class it extends already has.
func checkClasses(defs []Definition, p *Problems) {
	classes := map[string]*Class{}
	files := map[string]string{}
	for _, d := range defs {
		if d.Class != nil {
			classes[d.Class.Name] = d.Class
			files[d.Class.Name] = d.File
		}
	}
	for name, c := range classes {
		file := files[name]
		if c.Extends == "" {
			continue
		}
		if classes[c.Extends] == nil {
			p.add(file, "extends: there is no class %q in this app", c.Extends)
			continue
		}
		own := map[string]bool{}
		for _, fd := range c.Fields {
			own[fd.Name] = true
		}
		at, depth := c, 0
		for at.Extends != "" {
			depth++
			parent := classes[at.Extends]
			if parent == nil {
				break // reported on the class that names it
			}
			if parent == c || depth > maxClassDepth {
				p.add(file, "extends: classes may extend each other at most %d deep, and never in a loop", maxClassDepth)
				break
			}
			for _, fd := range parent.Fields {
				if own[fd.Name] {
					p.add(file, "fields: %s: %s already has a field called %s", fd.Name, parent.Name, fd.Name)
				}
			}
			at = parent
		}
	}
}

// resolveClasses works out each class's ancestors and full list of fields. A parent that is missing (an app removed
// since) ends the chain there.
func resolveClasses(classes map[string]*Class) {
	for _, c := range classes {
		c.Ancestors, c.All = nil, nil
		chain := []*Class{c}
		for at := c; at.Extends != "" && len(chain) <= maxClassDepth; {
			parent := classes[at.Extends]
			if parent == nil || slices.Contains(chain, parent) {
				break
			}
			c.Ancestors = append(c.Ancestors, parent.Name)
			chain = append(chain, parent)
			at = parent
		}
		seen := map[string]bool{}
		for i := len(chain) - 1; i >= 0; i-- {
			for _, fd := range chain[i].Fields {
				if !seen[fd.Name] {
					seen[fd.Name] = true
					c.All = append(c.All, fd)
				}
			}
		}
	}
}
