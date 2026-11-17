package apps

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/servicelater/servicelater/backend/internal/authz"
)

// CoreRecordTypes are the record types the platform itself provides, each with a row in task_types from a migration.
// `task` is the base every type extends: a lifecycle for `task` is the default for any type without its own. Apps add
// types of their own with record_type definitions (docs/apps-platform.md §9.5); TestCoreRecordTypesMatchTheRegistry
// keeps this list and the core rows in step.
var CoreRecordTypes = []string{"task", "incident", "change", "request", "problem", "work_task"}

// IsCoreType reports whether a type is the platform's own (task included).
func IsCoreType(t string) bool { return slices.Contains(CoreRecordTypes, t) }

var (
	typeNameRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{1,30}$`)
	typePrefixRE = regexp.MustCompile(`^[A-Z]{2,5}$`)
)

// validTypeName reports whether a name could be a record type's: what task_types accepts.
func validTypeName(t string) bool { return typeNameRE.MatchString(t) }

const (
	maxTypeLabel     = 60
	maxTypeDescribed = 500
)

// RecordType is a kind of record an app adds (types/<name>.yaml): a row in task_types with its own prefix and
// numbering, whose records live in tasks and whose own fields are declared by the type's form.
type RecordType struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
	Label  string `json:"label"`
	Plural string `json:"plural"`
	// AnyoneMayFile lets everyone signed in file one (like incidents); otherwise it takes records:create:<type>.
	AnyoneMayFile bool   `json:"anyone_may_file"`
	Description   string `json:"description,omitempty"`

	App     string `json:"-"`
	Version string `json:"-"`
}

type recordTypeFile struct {
	Kind          string `yaml:"kind"`
	Name          string `yaml:"name"`
	Prefix        string `yaml:"prefix"`
	Label         string `yaml:"label"`
	Plural        string `yaml:"plural"`
	AnyoneMayFile bool   `yaml:"anyone_may_file"`
	Description   string `yaml:"description"`
}

func parseRecordType(file string, data []byte, p *Problems) *RecordType {
	var f recordTypeFile
	if err := decodeStrict(data, &f); err != nil {
		p.add(file, "%v", err)
		return nil
	}
	if f.Kind != "record_type" {
		p.add(file, "kind must be record_type in this folder, got %q", f.Kind)
		return nil
	}
	t := &RecordType{Name: f.Name, Prefix: f.Prefix, Label: strings.TrimSpace(f.Label), Plural: strings.TrimSpace(f.Plural),
		AnyoneMayFile: f.AnyoneMayFile, Description: strings.TrimSpace(f.Description)}
	if t.Plural == "" {
		t.Plural = t.Label + "s"
	}
	before := len(*p)
	switch {
	case IsCoreType(t.Name):
		p.add(file, "name %q is a core record type, which apps cannot define", t.Name)
	case !validTypeName(t.Name):
		p.add(file, "name %q must be 2-31 characters: lowercase letters, digits and underscores, starting with a letter (prefix it with your app's name, such as tyler_thing)", t.Name)
	}
	if !typePrefixRE.MatchString(t.Prefix) {
		p.add(file, "prefix %q must be 2-5 capital letters, such as TYL: references are the prefix and the number (TYL1)", t.Prefix)
	}
	switch {
	case t.Label == "":
		p.add(file, "label is required: what people call one, such as Thing")
	case utf8.RuneCountInString(t.Label) > maxTypeLabel || utf8.RuneCountInString(t.Plural) > maxTypeLabel:
		p.add(file, "label and plural must be at most %d characters", maxTypeLabel)
	}
	if utf8.RuneCountInString(t.Description) > maxTypeDescribed {
		p.add(file, "description must be at most %d characters", maxTypeDescribed)
	}
	if len(*p) > before {
		return nil
	}
	return t
}

// namedType is a record type a definition names, and where.
type namedType struct {
	file, what, typ string
}

// typesNamed lists every record type the app's definitions and manifest name.
func typesNamed(m *Manifest, defs []Definition) []namedType {
	var out []namedType
	perms := func(file string, list []string) {
		for _, perm := range list {
			if _, typ, ok := authz.ParseRecord(perm); ok && typ != authz.AllTypes {
				out = append(out, namedType{file, fmt.Sprintf("permission %q", perm), typ})
			}
		}
	}
	lookups := func(file string, ls Lookups) {
		for _, name := range sortedKeys(ls) {
			if l := ls[name]; l != nil {
				if _, known := LookupTargets[l.Count]; !known && validTypeName(l.Count) {
					out = append(out, namedType{file, "lookups." + name + ": count", l.Count})
				}
			}
		}
	}
	if m != nil {
		perms("app.yaml", m.Permissions)
	}
	for _, d := range defs {
		switch {
		case d.Lifecycle != nil:
			out = append(out, namedType{d.File, "record_type", d.Lifecycle.RecordType})
			for _, t := range d.Lifecycle.Transitions {
				lookups(d.File, t.Lookups)
			}
		case d.Form != nil:
			out = append(out, namedType{d.File, "record_type", d.Form.RecordType})
		case d.Rule != nil:
			out = append(out, namedType{d.File, "record_type", d.Rule.RecordType})
			lookups(d.File, d.Rule.Lookups)
		case d.Workflow != nil:
			out = append(out, namedType{d.File, "record_type", d.Workflow.RecordType})
		case d.SLA != nil:
			out = append(out, namedType{d.File, "record_type", d.SLA.RecordType})
		case d.Role != nil:
			perms(d.File, d.Role.Permissions)
		}
	}
	return out
}

// checkRecordTypes makes sure every record type the app names exists: a core type, one of its own record_type
// definitions, or a type of one of its dependencies. Compiling needs no database, so a dependency's type is
// recognised by its name, which must start with the dependency's (tyler_thing for the tyler app, as types are
// named), and is returned for the install to check against what that app defines. Any other name is a mistake.
func checkRecordTypes(m *Manifest, defs []Definition, p *Problems) (foreign []string) {
	own := map[string]bool{}
	prefixes := map[string]string{}
	for _, d := range defs {
		if t := d.RecordType; t != nil {
			own[t.Name] = true
			if other, dup := prefixes[t.Prefix]; dup {
				p.add(d.File, "prefix %s is already the prefix of %s in this app", t.Prefix, other)
			}
			prefixes[t.Prefix] = t.Name
		}
	}
	var depPrefixes []string
	if m != nil {
		for _, d := range m.Deps {
			depPrefixes = append(depPrefixes, strings.ReplaceAll(d.Name, "-", "_")+"_")
		}
	}
	fromDep := func(t string) bool {
		return slices.ContainsFunc(depPrefixes, func(p string) bool { return strings.HasPrefix(t, p) })
	}
	seen := map[string]bool{}
	for _, n := range typesNamed(m, defs) {
		switch {
		case IsCoreType(n.typ) || own[n.typ]:
		case validTypeName(n.typ) && fromDep(n.typ):
			if !seen[n.typ] {
				seen[n.typ] = true
				foreign = append(foreign, n.typ)
			}
		default:
			available := slices.DeleteFunc(slices.Clone(CoreRecordTypes), func(s string) bool { return s == "task" })
			for t := range own {
				available = append(available, t)
			}
			slices.Sort(available[len(CoreRecordTypes)-1:])
			what := n.what + " names the record type"
			if n.what == "record_type" {
				what = "record_type"
			}
			p.add(n.file, "%s %q is neither a core type, nor one this app defines, nor a dependency's (available: %s)",
				what, n.typ, strings.Join(available, ", "))
		}
	}
	slices.Sort(foreign)
	return foreign
}
