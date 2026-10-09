package apps

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

// CatalogItem is something people may request from the service catalog (docs/apps-platform.md §8.4): a new laptop,
// access to an application. Ordering one files a request; its form asks the questions, and its workflow (approval,
// tasks for the teams involved) fulfils it.
type CatalogItem struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Category string `json:"category"`
	// Summary is a line under the label in the catalog; Description the item's page, in Markdown.
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`
	// Icon is one of CatalogIcons.
	Icon  string `json:"icon,omitempty"`
	Order int    `json:"order,omitempty"`
	// AvailableTo limits who sees and may order it: people holding any of the roles or in any of the groups. Absent,
	// everyone signed in may.
	AvailableTo *Audience `json:"available_to,omitempty"`
	// Title is the request's summary: text, or =an expression over the answers (record.fields.x). The label by default.
	Title *Value `json:"title,omitempty"`
	// Workflow fulfils it: a workflow of the same app for requests, started as the request is filed.
	Workflow string `json:"workflow,omitempty"`
	// Form is its questions, in the form field syntax, for a request (record_type request).
	Form *Form `json:"form"`

	App     string `json:"-"`
	Version string `json:"-"`
}

// Audience is who may see something: anyone holding one of the roles, or in one of the groups.
type Audience struct {
	Roles  []string `json:"roles,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

// CatalogIcons are the pictures an item may have, so the catalog never loads anything from an app.
var CatalogIcons = []string{"laptop", "phone", "monitor", "printer", "software", "key", "person", "network", "server", "building", "box", "question"}

const (
	maxCatalogLabel   = 100
	maxCatalogSummary = 200
	maxCatalogText    = 20000
	maxAudience       = 50
)

var (
	categoryRE = regexp.MustCompile(`^\S(.{0,58}\S)?$`)
	// roleNameRE matches app roles (cab-member) and the built-in base roles (end_user).
	roleNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,62}$`)
)

type catalogFile struct {
	Kind        string         `yaml:"kind"`
	Name        string         `yaml:"name"`
	Label       string         `yaml:"label"`
	Category    string         `yaml:"category"`
	Summary     string         `yaml:"summary"`
	Description string         `yaml:"description"`
	Icon        string         `yaml:"icon"`
	Order       int            `yaml:"order"`
	AvailableTo *Audience      `yaml:"available_to"`
	Title       *Value         `yaml:"title"`
	Workflow    string         `yaml:"workflow"`
	Fields      orderedFields  `yaml:"fields"`
	Layout      formLayoutFile `yaml:"layout"`
	Validate    []Guard        `yaml:"validate"`
}

func parseCatalogItem(file string, data []byte, p *Problems) *CatalogItem {
	var f catalogFile
	if err := decodeStrict(data, &f); err != nil {
		p.add(file, "%v", err)
		return nil
	}
	if f.Kind != "catalog_item" {
		p.add(file, "kind must be catalog_item in this folder, got %q", f.Kind)
		return nil
	}
	it := &CatalogItem{Name: f.Name, Label: f.Label, Category: f.Category, Summary: f.Summary, Description: f.Description,
		Icon: f.Icon, Order: f.Order, AvailableTo: f.AvailableTo, Title: f.Title, Workflow: f.Workflow}
	form := &Form{RecordType: "request", Fields: f.Fields, Create: f.Layout.Create, Detail: f.Layout.Detail, Validate: f.Validate, TitleGiven: true}
	// Left out, the layouts are the questions in order: asked when ordering, shown on the request.
	if len(form.Create) == 0 && len(form.Detail) == 0 {
		var names []string
		for _, fd := range form.Fields {
			if _, core := coreField("request", fd.Name); !core || fd.Name == "description" {
				names = append(names, fd.Name)
			}
		}
		form.Create = names
		var detail []string
		for _, n := range names {
			if n != "description" {
				detail = append(detail, n)
			}
		}
		if len(detail) > 0 {
			form.Detail = []Section{{Label: "Details", Fields: detail}}
		}
	}
	it.Form = form
	before := len(*p)
	it.validate(file, p)
	if len(*p) > before {
		return nil
	}
	return it
}

func (it *CatalogItem) validate(file string, p *Problems) {
	if !ruleNameRE.MatchString(it.Name) {
		p.add(file, "name %q must be 2-63 characters: lowercase letters, digits and hyphens, starting with a letter", it.Name)
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(it.Label)); n == 0 || n > maxCatalogLabel {
		p.add(file, "label is required, at most %d characters", maxCatalogLabel)
	}
	if !categoryRE.MatchString(it.Category) {
		p.add(file, "category is required: a heading of at most 60 characters, such as Hardware")
	}
	if utf8.RuneCountInString(it.Summary) > maxCatalogSummary {
		p.add(file, "summary is longer than %d characters", maxCatalogSummary)
	}
	if len(it.Description) > maxCatalogText {
		p.add(file, "description is longer than %d characters", maxCatalogText)
	}
	if it.Icon != "" && !slices.Contains(CatalogIcons, it.Icon) {
		p.add(file, "icon %q is not one of: %s", it.Icon, strings.Join(CatalogIcons, ", "))
	}
	if a := it.AvailableTo; a != nil {
		if len(a.Roles) == 0 && len(a.Groups) == 0 {
			p.add(file, "available_to needs roles or groups (leave it out for everyone)")
		}
		if len(a.Roles)+len(a.Groups) > maxAudience {
			p.add(file, "available_to may name at most %d roles and groups", maxAudience)
		}
		for _, r := range a.Roles {
			if !roleNameRE.MatchString(r) {
				p.add(file, "available_to: roles: %q is not a role name", r)
			}
		}
		for _, g := range a.Groups {
			if strings.TrimSpace(g) == "" || utf8.RuneCountInString(g) > 100 {
				p.add(file, "available_to: groups: %q is not a group name", g)
			}
		}
	}
	if it.Workflow != "" && !ruleNameRE.MatchString(it.Workflow) {
		p.add(file, "workflow %q is not a workflow name", it.Workflow)
	}
	if it.Title != nil && it.Title.Expr == "" {
		if s, ok := it.Title.Literal.(string); !ok || strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > 200 {
			p.add(file, "title must be text of at most 200 characters, or =an expression")
		}
	}
	at := len(*p)
	it.Form.validate(file, p)
	if len(*p) > at {
		return
	}
	it.compile()
	if it.Title != nil && it.Title.err != nil {
		p.add(file, "title: %v", it.Title.err)
	}
}

// compile prepares the item's expressions: its form's conditions, and its title, which reads the answers.
func (it *CatalogItem) compile() {
	if it.Form != nil {
		it.Form.compile()
	}
	if it.Title != nil {
		env, err := expr.RuleFor(it.Form.Types())
		it.Title.compile(env, err, expr.String)
	}
}

// TitleProgram is the item's title, or why it cannot be worked out. Nil with no error means the label.
func (it *CatalogItem) TitleProgram() (*Value, error) {
	if it.Title == nil {
		return nil, nil
	}
	return it.Title, it.Title.err
}

// AvailableToAll says everyone signed in may order it.
func (it *CatalogItem) AvailableToAll() bool { return it.AvailableTo == nil }

// checkCatalogWorkflows checks what only the whole app can tell: an item's workflow is one of the app's, for requests.
func checkCatalogWorkflows(defs []Definition, p *Problems) {
	workflows := map[string]*Workflow{}
	for _, d := range defs {
		if d.Workflow != nil {
			workflows[d.Workflow.Name] = d.Workflow
		}
	}
	for _, d := range defs {
		it := d.CatalogItem
		if it == nil || it.Workflow == "" {
			continue
		}
		switch wf := workflows[it.Workflow]; {
		case wf == nil:
			p.add(d.File, "workflow: there is no workflow %q in this app (workflows: %s)", it.Workflow, workflowNames(workflows))
		case wf.RecordType != "request" && wf.RecordType != "task":
			p.add(d.File, "workflow: %q is for %s records, but an item files a request", it.Workflow, wf.RecordType)
		}
	}
}

// addItemFields adds an item's questions to the app's request fields, so its rules and workflows read them typed. Two
// items asking a question of the same name must agree on its type. The form set's own request form is never changed:
// a copy takes its place.
func (fs formSet) addItemFields(it *CatalogItem, file string, p *Problems) {
	base := fs["request"]
	merged := &Form{RecordType: "request"}
	if base != nil {
		cp := *base
		cp.Fields = slices.Clone(base.Fields)
		merged = &cp
	}
	have := map[string]string{}
	for _, fd := range merged.Fields {
		have[fd.Name] = fd.Type
	}
	for _, fd := range it.Form.Fields {
		if fd.core {
			continue
		}
		if t, ok := have[fd.Name]; ok {
			if t != fd.Type {
				p.add(file, "fields: %s is a %s here and a %s in another of this app's items or its request form: one name, one type", fd.Name, fd.Type, t)
			}
			continue
		}
		merged.Fields = append(merged.Fields, fd)
		have[fd.Name] = fd.Type
	}
	fs["request"] = merged
}
