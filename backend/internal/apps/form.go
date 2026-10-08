package apps

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

// Kinds of form field. A core field also has priority, which only the core priority field is.
const (
	FieldText     = "text"
	FieldTextarea = "textarea"
	FieldNumber   = "number"
	FieldBoolean  = "boolean"
	FieldDate     = "date"
	FieldDateTime = "datetime"
	FieldChoice   = "choice"
	FieldPriority = "priority"
)

// FormFieldTypes are the kinds of extra field a form can declare.
var FormFieldTypes = []string{FieldText, FieldTextarea, FieldNumber, FieldBoolean, FieldDate, FieldDateTime, FieldChoice}

const (
	maxFormFields    = 100
	maxFormSections  = 20
	maxFieldOptions  = 200
	maxFormChecks    = 20
	defaultMaxText   = 1000
	maxTextLength    = 10000
	maxLabel         = 100
	maxHelp          = 500
	maxOptionValue   = 100
	fieldNamePattern = `[a-z][a-z0-9_]{0,39}`
)

var fieldNameRE = regexp.MustCompile(`^` + fieldNamePattern + `$`)

// Option is one value a choice field offers. With When, it is offered only while that holds.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
	When  string `json:"when,omitempty"`

	when    *expr.Program
	whenErr error
}

func (o *Option) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		o.Value, o.Label = n.Value, n.Value
		return nil
	}
	var f struct {
		Value string `yaml:"value"`
		Label string `yaml:"label"`
		When  string `yaml:"when"`
	}
	if err := decodeNodeStrict(n, &f); err != nil {
		return err
	}
	o.Value, o.Label, o.When = f.Value, f.Label, f.When
	if o.Label == "" {
		o.Label = o.Value
	}
	return nil
}

// FormField is a field as a form declares it: an extra field (Type set), or a core field's label, help and conditions
// (Type empty, filled in from CoreFields when the form is compiled).
type FormField struct {
	Name    string   `json:"name"`
	Type    string   `json:"type,omitempty"`
	Label   string   `json:"label,omitempty"`
	Help    string   `json:"help,omitempty"`
	Options []Option `json:"options,omitempty"`
	// Required always, or RequiredWhen this holds. A field that is hidden is never required.
	Required     bool   `json:"required,omitempty"`
	RequiredWhen string `json:"required_when,omitempty"`
	VisibleWhen  string `json:"visible_when,omitempty"`
	// ReadOnlyWhen stops a person changing the field while it holds; rules and workflows still may.
	ReadOnlyWhen string `json:"read_only_when,omitempty"`
	Default      *Value `json:"default,omitempty"`
	MaxLength    int    `json:"max_length,omitempty"`
	Min          *int64 `json:"min,omitempty"`
	Max          *int64 `json:"max,omitempty"`

	core                        bool
	required, visible, readOnly *expr.Program
	requiredErr, visErr, roErr  error
}

// Core reports whether the field is one of the record's own columns rather than an extra field.
func (f *FormField) Core() bool { return f.core }

// Section is a group of fields shown together on a record, under a heading.
type Section struct {
	Label  string   `json:"label,omitempty" yaml:"section"`
	Fields []string `json:"fields" yaml:"fields"`
}

// Form is how a record type's fields are laid out and checked (docs/apps-platform.md §8): the extra fields it adds,
// the core fields it places, which appear when filing one and on the record, and when each is required, shown or
// read-only. The server enforces all of it on every save; the UI only shows what the server says.
type Form struct {
	RecordType string      `json:"record_type"`
	Fields     []FormField `json:"fields"`
	Create     []string    `json:"create"`
	Detail     []Section   `json:"detail"`
	// Validate are checks on the whole record: each When must hold, or the save is refused with its Error.
	Validate []Guard `json:"validate,omitempty"`

	App     string `json:"-"`
	Version string `json:"-"`
	// Builtin marks the form a record type has when no app gives it one.
	Builtin bool `json:"-"`

	byName map[string]*FormField
}

// CoreField is one of a record's own columns as a form can place it.
type CoreField struct {
	Name      string
	Type      string
	Label     string
	Options   []Option
	MaxLength int
	// Default is what a new record has unless it is filled in.
	Default any
	// RecordType is the type whose own table has the column; empty for the columns every record has.
	RecordType string
	// OnCreate fields may be on the form for filing one; Detail fields may be laid out on the record (title,
	// description and priority have places of their own there).
	OnCreate, Detail bool
}

var levelOptions = []Option{{Value: "", Label: "Not assessed"}, {Value: "low", Label: "Low"}, {Value: "medium", Label: "Medium"}, {Value: "high", Label: "High"}}

// CoreFields are the record columns a form can lay out, with the label each has unless a form gives another.
var CoreFields = []CoreField{
	{Name: "title", Type: FieldText, Label: "Summary", MaxLength: 200, OnCreate: true},
	{Name: "description", Type: FieldTextarea, Label: "Details", MaxLength: 10000, OnCreate: true},
	{Name: "priority", Type: FieldPriority, Label: "Priority", OnCreate: true, Default: int64(3),
		Options: []Option{{Value: "1", Label: "Critical"}, {Value: "2", Label: "High"}, {Value: "3", Label: "Medium"}, {Value: "4", Label: "Low"}}},
	{Name: "resolution", Type: FieldTextarea, Label: "Resolution notes", MaxLength: 10000, RecordType: "incident", Detail: true},
	{Name: "change_type", Type: FieldChoice, Label: "Type", RecordType: "change", OnCreate: true, Detail: true, Default: "normal",
		Options: []Option{{Value: "standard", Label: "Standard"}, {Value: "normal", Label: "Normal"}, {Value: "emergency", Label: "Emergency"}}},
	{Name: "risk", Type: FieldChoice, Label: "Risk", RecordType: "change", Options: levelOptions, OnCreate: true, Detail: true},
	{Name: "impact", Type: FieldChoice, Label: "Impact", RecordType: "change", Options: levelOptions, OnCreate: true, Detail: true},
	{Name: "planned_start", Type: FieldDateTime, Label: "Planned start", RecordType: "change", OnCreate: true, Detail: true},
	{Name: "planned_end", Type: FieldDateTime, Label: "Planned end", RecordType: "change", OnCreate: true, Detail: true},
	{Name: "implementation_plan", Type: FieldTextarea, Label: "Implementation plan", MaxLength: 10000, RecordType: "change", OnCreate: true, Detail: true},
	{Name: "backout_plan", Type: FieldTextarea, Label: "Back-out plan", MaxLength: 10000, RecordType: "change", OnCreate: true, Detail: true},
	{Name: "test_plan", Type: FieldTextarea, Label: "Test plan", MaxLength: 10000, RecordType: "change", OnCreate: true, Detail: true},
}

// coreField finds a core field a record type has.
func coreField(recordType, name string) (CoreField, bool) {
	for _, c := range CoreFields {
		if c.Name == name && (c.RecordType == "" || c.RecordType == recordType) {
			return c, true
		}
	}
	return CoreField{}, false
}

// commonColumns are what every record has besides the core fields a form can place.
var commonColumns = []string{"id", "type", "number", "reference", "status", "state_category", "requester_id", "requester_name",
	"assignee_id", "assignee_name", "assignment_group", "assignment_group_id", "created_at", "updated_at", "fields"}

// reservedName reports whether name is one of a record type's own columns, which an extra field may not be called. A
// column of another type (a change's impact) is free: on an incident it can only mean an extra field.
func reservedName(recordType, name string) bool {
	_, core := coreField(recordType, name)
	return core || slices.Contains(commonColumns, name)
}

// FormFor is the form a record type has when no app gives it one: its core fields, those that can be set when
// filing on the form for filing, the rest of its own on the record.
func FormFor(recordType string) *Form {
	f := &Form{RecordType: recordType, Builtin: true}
	var detail []string
	for _, c := range CoreFields {
		if c.RecordType != "" && c.RecordType != recordType {
			continue
		}
		f.Fields = append(f.Fields, FormField{Name: c.Name})
		if c.OnCreate && (c.RecordType == "" || c.Name == "change_type") {
			f.Create = append(f.Create, c.Name)
		}
		if c.Detail {
			detail = append(detail, c.Name)
		}
	}
	if len(detail) > 0 {
		f.Detail = []Section{{Fields: detail}}
	}
	f.compile()
	return f
}

// ---- YAML ----

type formFile struct {
	Kind       string         `yaml:"kind"`
	RecordType string         `yaml:"record_type"`
	Fields     orderedFields  `yaml:"fields"`
	Layout     formLayoutFile `yaml:"layout"`
	Validate   []Guard        `yaml:"validate"`
}

type formLayoutFile struct {
	Create []string  `yaml:"create"`
	Detail []Section `yaml:"detail"`
}

type formFieldFile struct {
	Type         string   `yaml:"type"`
	Label        string   `yaml:"label"`
	Help         string   `yaml:"help"`
	Options      []Option `yaml:"options"`
	Required     bool     `yaml:"required"`
	RequiredWhen string   `yaml:"required_when"`
	VisibleWhen  string   `yaml:"visible_when"`
	ReadOnlyWhen string   `yaml:"read_only_when"`
	Default      *Value   `yaml:"default"`
	MaxLength    int      `yaml:"max_length"`
	Min          *int64   `yaml:"min"`
	Max          *int64   `yaml:"max"`
}

// orderedFields reads the fields map keeping the author's order.
type orderedFields []FormField

func (o *orderedFields) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: fields must be a map of field name to its definition", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		var f formFieldFile
		if err := decodeNodeStrict(val, &f); err != nil {
			return fmt.Errorf("field %q: %w", key.Value, err)
		}
		*o = append(*o, FormField{Name: key.Value, Type: f.Type, Label: f.Label, Help: f.Help, Options: f.Options,
			Required: f.Required, RequiredWhen: f.RequiredWhen, VisibleWhen: f.VisibleWhen, ReadOnlyWhen: f.ReadOnlyWhen,
			Default: f.Default, MaxLength: f.MaxLength, Min: f.Min, Max: f.Max})
	}
	return nil
}

func parseForm(file string, data []byte, p *Problems) *Form {
	var f formFile
	if err := decodeStrict(data, &f); err != nil {
		p.add(file, "%v", err)
		return nil
	}
	if f.Kind != "form" {
		p.add(file, "kind must be form in this folder, got %q", f.Kind)
		return nil
	}
	form := &Form{RecordType: f.RecordType, Fields: f.Fields, Create: f.Layout.Create, Detail: f.Layout.Detail, Validate: f.Validate}
	before := len(*p)
	form.validate(file, p)
	if len(*p) > before {
		return nil
	}
	return form
}

// ---- validation ----

func (f *Form) validate(file string, p *Problems) {
	before := len(*p)
	f.validateStructure(file, p)
	if len(*p) > before {
		return
	}
	f.compile()
	for i := range f.Fields {
		fd := &f.Fields[i]
		for _, c := range []struct {
			what string
			err  error
		}{{"required_when", fd.requiredErr}, {"visible_when", fd.visErr}, {"read_only_when", fd.roErr}} {
			if c.err != nil {
				p.add(file, "fields: %s: %s: %v", fd.Name, c.what, c.err)
			}
		}
		for _, o := range fd.Options {
			if o.whenErr != nil {
				p.add(file, "fields: %s: option %q: when: %v", fd.Name, o.Value, o.whenErr)
			}
		}
	}
	for i, g := range f.Validate {
		if g.err != nil {
			p.add(file, "validate[%d]: when: %v", i, g.err)
		}
	}
	// What a condition reads must be a field the record has.
	for _, prog := range f.programs() {
		for _, lk := range prog.Lookups() {
			p.add(file, "lookups are not available in a form (it reads lookups.%s)", lk)
		}
	}
}

func (f *Form) validateStructure(file string, p *Problems) {
	switch {
	case f.RecordType == "task":
		p.add(file, "record_type: a form is for one record type; forms for task are not supported yet")
		return
	case !slices.Contains(KnownRecordTypes, f.RecordType):
		p.add(file, "record_type %q is not a record type this version provides (available: %v)", f.RecordType, KnownRecordTypes)
		return
	}
	if len(f.Fields) > maxFormFields {
		p.add(file, "a form may have at most %d fields, this has %d", maxFormFields, len(f.Fields))
	}
	seen := map[string]bool{}
	for i := range f.Fields {
		fd := &f.Fields[i]
		at := "fields: " + fd.Name
		if seen[fd.Name] {
			p.add(file, "%s: listed twice", at)
		}
		seen[fd.Name] = true
		if c, ok := coreField(f.RecordType, fd.Name); ok {
			fd.core = true
			if fd.Type != "" || len(fd.Options) > 0 || fd.Default != nil || fd.MaxLength != 0 || fd.Min != nil || fd.Max != nil {
				p.add(file, "%s is a %s's own field: a form can change its label, help and when it is required, shown or read-only, not its type, options, default or limits", at, recordTypeWord(f.RecordType))
			}
			if c.Name == "title" && (fd.VisibleWhen != "" || fd.ReadOnlyWhen != "") {
				p.add(file, "%s: every record has a summary, so it is always shown and can always be written", at)
			}
		} else {
			fd.validateExtra(f.RecordType+" records", func(n string) bool { return reservedName(f.RecordType, n) }, at, file, p)
		}
		if utf8.RuneCountInString(fd.Label) > maxLabel {
			p.add(file, "%s: label is longer than %d characters", at, maxLabel)
		}
		if utf8.RuneCountInString(fd.Help) > maxHelp {
			p.add(file, "%s: help is longer than %d characters", at, maxHelp)
		}
		if fd.Required && fd.RequiredWhen != "" {
			p.add(file, "%s: give required or required_when, not both", at)
		}
	}
	f.index()

	// The layouts place every extra field somewhere, and only fields the record has.
	placed := map[string]bool{}
	inCreate := map[string]bool{}
	for i, name := range f.Create {
		at := fmt.Sprintf("layout: create[%d]", i)
		if inCreate[name] {
			p.add(file, "%s: %s is listed twice", at, name)
		}
		inCreate[name] = true
		placed[name] = true
		if fd, ok := f.byName[name]; ok && !fd.core {
			continue
		}
		c, ok := coreField(f.RecordType, name)
		switch {
		case !ok:
			p.add(file, "%s: %q is not a field of %s records: declare it under fields", at, name, f.RecordType)
		case !c.OnCreate:
			p.add(file, "%s: %s cannot be filled in when filing a record", at, name)
		}
	}
	if !inCreate["title"] {
		p.add(file, "layout: create must include title: every record needs a summary")
	}
	if len(f.Detail) > maxFormSections {
		p.add(file, "layout: detail may have at most %d sections", maxFormSections)
	}
	inDetail := map[string]bool{}
	for i, s := range f.Detail {
		if len(s.Fields) == 0 {
			p.add(file, "layout: detail[%d]: a section needs fields", i)
		}
		if utf8.RuneCountInString(s.Label) > maxLabel {
			p.add(file, "layout: detail[%d]: label is longer than %d characters", i, maxLabel)
		}
		for _, name := range s.Fields {
			at := fmt.Sprintf("layout: detail[%d]", i)
			if inDetail[name] {
				p.add(file, "%s: %s is on the record twice", at, name)
			}
			inDetail[name] = true
			placed[name] = true
			if fd, ok := f.byName[name]; ok && !fd.core {
				continue
			}
			c, ok := coreField(f.RecordType, name)
			switch {
			case !ok:
				p.add(file, "%s: %q is not a field of %s records: declare it under fields", at, name, f.RecordType)
			case !c.Detail:
				p.add(file, "%s: %s has a place of its own on the record, so it cannot be laid out", at, name)
			}
		}
	}
	for _, fd := range f.Fields {
		if fd.core {
			continue
		}
		if !placed[fd.Name] {
			p.add(file, "fields: %s is in neither layout: add it to layout.create or layout.detail", fd.Name)
		}
		if fd.Required && !inCreate[fd.Name] && fd.Default == nil {
			p.add(file, "fields: %s is required, so it must be on layout.create or have a default", fd.Name)
		}
	}
	if len(f.Validate) > maxFormChecks {
		p.add(file, "validate may have at most %d checks", maxFormChecks)
	}
	for i, g := range f.Validate {
		if g.When == "" || g.Error == "" {
			p.add(file, "validate[%d]: needs when (what must hold) and error (what the person sees otherwise)", i)
		}
	}
}

func recordTypeWord(t string) string {
	if t == "incident" {
		return "an incident"
	}
	return "a " + t
}

// validateExtra checks a field a form (or a CMDB class) declares itself. owner names what has it ("incident records")
// and reserved says which names its own columns take.
func (fd *FormField) validateExtra(owner string, reserved func(string) bool, at, file string, p *Problems) {
	switch {
	case !fieldNameRE.MatchString(fd.Name):
		p.add(file, "%s: a field name is 1-40 characters: lowercase letters, digits and underscores, starting with a letter", at)
	case reserved(fd.Name):
		p.add(file, "%s: %s have a field of their own called %s, so an extra field cannot use the name", at, owner, fd.Name)
	}
	if !slices.Contains(FormFieldTypes, fd.Type) {
		if fd.Type == "" {
			p.add(file, "%s: an extra field needs a type (%s); %s have no field of that name", at, strings.Join(FormFieldTypes, ", "), owner)
		} else {
			p.add(file, "%s: type must be one of %s, got %q", at, strings.Join(FormFieldTypes, ", "), fd.Type)
		}
		return
	}
	if fd.Label == "" {
		p.add(file, "%s: needs a label", at)
	}
	if fd.Type == FieldChoice {
		switch n := len(fd.Options); {
		case n == 0:
			p.add(file, "%s: a choice needs options", at)
		case n > maxFieldOptions:
			p.add(file, "%s: a choice may have at most %d options", at, maxFieldOptions)
		}
		seen := map[string]bool{}
		for _, o := range fd.Options {
			switch {
			case o.Value == "":
				p.add(file, "%s: an option needs a value (leaving the field empty is not choosing)", at)
			case utf8.RuneCountInString(o.Value) > maxOptionValue:
				p.add(file, "%s: option %q is longer than %d characters", at, o.Value, maxOptionValue)
			case seen[o.Value]:
				p.add(file, "%s: option %q is listed twice", at, o.Value)
			}
			seen[o.Value] = true
		}
	} else if len(fd.Options) > 0 {
		p.add(file, "%s: only a choice has options", at)
	}
	if fd.MaxLength != 0 && fd.Type != FieldText && fd.Type != FieldTextarea {
		p.add(file, "%s: only text has a max_length", at)
	} else if fd.MaxLength < 0 || fd.MaxLength > maxTextLength {
		p.add(file, "%s: max_length must be 1 to %d", at, maxTextLength)
	}
	if (fd.Min != nil || fd.Max != nil) && fd.Type != FieldNumber {
		p.add(file, "%s: only a number has min and max", at)
	} else if fd.Min != nil && fd.Max != nil && *fd.Min > *fd.Max {
		p.add(file, "%s: min is more than max", at)
	}
	if fd.Default != nil {
		switch {
		case fd.Default.Expr != "":
			p.add(file, "%s: default is a value, not an expression", at)
		default:
			if _, msg := fd.Check(fd.Default.Literal); msg != "" {
				p.add(file, "%s: default: %s", at, msg)
			}
		}
	}
}

// ---- compiling ----

func (f *Form) index() {
	f.byName = map[string]*FormField{}
	for i := range f.Fields {
		fd := &f.Fields[i]
		if _, ok := coreField(f.RecordType, fd.Name); ok && fd.Type == "" {
			fd.core = true
		}
		f.byName[fd.Name] = fd
	}
}

// Types are the form's extra fields as expressions see them.
func (f *Form) Types() expr.FieldTypes {
	out := expr.FieldTypes{}
	for _, fd := range f.Fields {
		if !fd.core {
			out[fd.Name] = exprKind(fd.Type)
		}
	}
	return out
}

func exprKind(fieldType string) string {
	switch fieldType {
	case FieldNumber:
		return expr.FieldNumber
	case FieldBoolean:
		return expr.FieldBool
	case FieldDate, FieldDateTime:
		return expr.FieldTime
	}
	return expr.FieldText
}

// compile compiles every condition. One that fails is remembered and blocks saves until the app is fixed, the way a
// guard that cannot be checked blocks its move.
func (f *Form) compile() {
	f.index()
	env, envErr := expr.RuleFor(f.Types())
	cond := func(src string) (*expr.Program, error) {
		if src == "" || envErr != nil {
			return nil, envErr
		}
		return env.Compile(src)
	}
	for i := range f.Fields {
		fd := &f.Fields[i]
		fd.required, fd.requiredErr = cond(fd.RequiredWhen)
		fd.visible, fd.visErr = cond(fd.VisibleWhen)
		fd.readOnly, fd.roErr = cond(fd.ReadOnlyWhen)
		for j := range fd.Options {
			o := &fd.Options[j]
			o.when, o.whenErr = cond(o.When)
		}
	}
	for i := range f.Validate {
		g := &f.Validate[i]
		g.program, g.err = cond(g.When)
	}
}

func (f *Form) programs() (out []*expr.Program) {
	for _, fd := range f.Fields {
		for _, p := range []*expr.Program{fd.required, fd.visible, fd.readOnly} {
			if p != nil {
				out = append(out, p)
			}
		}
		for _, o := range fd.Options {
			if o.when != nil {
				out = append(out, o.when)
			}
		}
	}
	for _, g := range f.Validate {
		if g.program != nil {
			out = append(out, g.program)
		}
	}
	return out
}

// Field finds a field the form declares or a core field of the record type; ok is false for neither.
func (f *Form) Field(name string) (FormField, bool) {
	if fd, ok := f.byName[name]; ok {
		return f.resolved(*fd), true
	}
	if _, ok := coreField(f.RecordType, name); ok {
		return f.resolved(FormField{Name: name, core: true}), true
	}
	return FormField{}, false
}

// Extra finds an extra field the form declares.
func (f *Form) Extra(name string) (*FormField, bool) {
	fd, ok := f.byName[name]
	if !ok || fd.core {
		return nil, false
	}
	return fd, true
}

// resolved is a field with a core field's type, options and defaults filled in.
func (f *Form) resolved(fd FormField) FormField {
	if !fd.core {
		return fd
	}
	c, _ := coreField(f.RecordType, fd.Name)
	fd.Type, fd.Options, fd.MaxLength = c.Type, c.Options, c.MaxLength
	if c.Default != nil {
		fd.Default = &Value{Literal: c.Default}
	}
	if fd.Label == "" {
		fd.Label = c.Label
	}
	if c.Name == "title" {
		fd.Required = true
	}
	return fd
}

// Placed lists every field on either layout, in order, each once, resolved.
func (f *Form) Placed() []FormField {
	var out []FormField
	seen := map[string]bool{}
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		if fd, ok := f.Field(name); ok {
			out = append(out, fd)
		}
	}
	for _, n := range f.Create {
		add(n)
	}
	for _, s := range f.Detail {
		for _, n := range s.Fields {
			add(n)
		}
	}
	for _, fd := range f.Fields { // a core field's conditions apply even where it has a place of its own (priority)
		add(fd.Name)
	}
	return out
}

// Reactive lists the fields whose value the form's conditions read: when one changes, the states may.
func (f *Form) Reactive() []string {
	var out []string
	for _, p := range f.programs() {
		for _, r := range p.Reads() {
			name := strings.TrimPrefix(r, "fields.")
			if _, ok := f.Field(name); ok && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// ---- values ----

// Check normalises an extra field's value as it is stored and reports what is wrong with it, if anything. Values
// arrive from JSON or from a rule: text, a whole number, true or false; dates and times are text (YYYY-MM-DD,
// RFC 3339) or a time a rule computed.
func (fd *FormField) Check(v any) (any, string) {
	switch fd.Type {
	case FieldText, FieldTextarea, FieldChoice:
		s, ok := v.(string)
		if !ok {
			return nil, "takes text"
		}
		limit := fd.MaxLength
		if limit == 0 {
			limit = defaultMaxText
			if fd.Type == FieldTextarea {
				limit = maxTextLength
			}
		}
		if utf8.RuneCountInString(s) > limit {
			return nil, fmt.Sprintf("is longer than %d characters", limit)
		}
		if fd.Type == FieldChoice && !slices.ContainsFunc(fd.Options, func(o Option) bool { return o.Value == s }) {
			return nil, "must be one of " + quoteAll(fd.optionValues())
		}
		return s, ""
	case FieldNumber:
		n, err := expr.FieldValue(expr.FieldNumber, v)
		if err != nil {
			return nil, "takes a whole number"
		}
		i := n.(int64)
		if fd.Min != nil && i < *fd.Min {
			return nil, fmt.Sprintf("must be at least %d", *fd.Min)
		}
		if fd.Max != nil && i > *fd.Max {
			return nil, fmt.Sprintf("must be at most %d", *fd.Max)
		}
		return i, ""
	case FieldBoolean:
		b, ok := v.(bool)
		if !ok {
			return nil, "takes true or false"
		}
		return b, ""
	case FieldDate:
		switch t := v.(type) {
		case time.Time:
			return t.UTC().Format(time.DateOnly), ""
		case string:
			if at, err := time.Parse(time.DateOnly, t); err == nil {
				return at.Format(time.DateOnly), ""
			}
		}
		return nil, "takes a date, as YYYY-MM-DD"
	case FieldDateTime:
		switch t := v.(type) {
		case time.Time:
			return t.UTC().Format(time.RFC3339), ""
		case string:
			if at, err := time.Parse(time.RFC3339, t); err == nil {
				return at.UTC().Format(time.RFC3339), ""
			}
		}
		return nil, "takes a date and time, as RFC 3339 (2026-10-07T09:30:00Z)"
	}
	return nil, "cannot be set"
}

func (fd *FormField) optionValues() []string {
	out := make([]string, len(fd.Options))
	for i, o := range fd.Options {
		out[i] = o.Value
	}
	return out
}

// FieldState is how a field is on a record right now: whether it is shown, required and writable, and for a choice
// whose options depend on the record, which of them are offered.
type FieldState struct {
	Hidden   bool `json:"hidden,omitempty"`
	Required bool `json:"required,omitempty"`
	ReadOnly bool `json:"read_only,omitempty"`
	// Options are the values offered, when not all of a choice's options are.
	Options []string `json:"options,omitempty"`
}

// States works out every placed field's state for the record in `in`. A condition that cannot be evaluated is an
// error: the caller refuses the save, as a guard that cannot be checked blocks its move.
func (f *Form) States(ctx context.Context, in expr.Input) (map[string]FieldState, error) {
	out := map[string]FieldState{}
	for _, fd := range f.Placed() {
		src := f.byName[fd.Name]
		if src == nil {
			src = &FormField{}
		}
		st := FieldState{Required: fd.Required}
		eval := func(p *expr.Program, err error, what string) (bool, error) {
			if err != nil {
				return false, fmt.Errorf("%s %s: %w", fd.Name, what, err)
			}
			if p == nil {
				return false, nil
			}
			ok, err := p.Eval(ctx, in)
			if err != nil {
				return false, fmt.Errorf("%s %s: %w", fd.Name, what, err)
			}
			return ok, nil
		}
		var err error
		if src.VisibleWhen != "" {
			var shown bool
			if shown, err = eval(src.visible, src.visErr, "visible_when"); err != nil {
				return nil, err
			}
			st.Hidden = !shown
		}
		if src.RequiredWhen != "" {
			if st.Required, err = eval(src.required, src.requiredErr, "required_when"); err != nil {
				return nil, err
			}
		}
		if st.Hidden {
			st.Required = false
		}
		if src.ReadOnlyWhen != "" {
			if st.ReadOnly, err = eval(src.readOnly, src.roErr, "read_only_when"); err != nil {
				return nil, err
			}
		}
		// A hidden field's options are not worked out: their conditions may read what is not filled in yet.
		if !fd.core && !st.Hidden && slices.ContainsFunc(src.Options, func(o Option) bool { return o.When != "" }) {
			st.Options = []string{}
			for _, o := range src.Options {
				ok := true
				if o.When != "" {
					if ok, err = eval(o.when, o.whenErr, "option "+o.Value); err != nil {
						return nil, err
					}
				}
				if ok {
					st.Options = append(st.Options, o.Value)
				}
			}
		}
		out[fd.Name] = st
	}
	return out, nil
}

// Checks runs the form's validate checks and returns the first one's error that does not hold, or "".
func (f *Form) Checks(ctx context.Context, in expr.Input) (string, error) {
	for i, g := range f.Validate {
		if g.err != nil {
			return "", fmt.Errorf("validate[%d]: %w", i, g.err)
		}
		ok, err := g.program.Eval(ctx, in)
		if err != nil {
			return "", fmt.Errorf("validate[%d]: %w", i, err)
		}
		if !ok {
			return g.Error, nil
		}
	}
	return "", nil
}

// ---- other definitions reading extra fields ----

// formSet is the forms an app has, by record type.
type formSet map[string]*Form

// fieldsUsed are the extra fields of recordType's form that a definition's text names (fields.name), with their
// types. A definition stores them, so it compiles the same way later whatever the form becomes; naming one in a
// string as well only adds an unused entry.
func (fs formSet) fieldsUsed(recordType string, text []byte) expr.FieldTypes {
	f := fs[recordType]
	if f == nil {
		return nil
	}
	var out expr.FieldTypes
	for _, fd := range f.Fields {
		if fd.core {
			continue
		}
		if regexp.MustCompile(`fields\s*\.\s*` + fd.Name + `\b`).Match(text) {
			if out == nil {
				out = expr.FieldTypes{}
			}
			out[fd.Name] = exprKind(fd.Type)
		}
	}
	return out
}

// HasReadOnly reports whether any field can be read-only.
func (f *Form) HasReadOnly() bool {
	return slices.ContainsFunc(f.Fields, func(fd FormField) bool { return fd.ReadOnlyWhen != "" })
}

// Defaults are the extra fields a new record starts with: each field's default, where it has one.
func (f *Form) Defaults() map[string]any {
	out := map[string]any{}
	for _, fd := range f.Fields {
		if !fd.core && fd.Default != nil && fd.Default.Literal != nil {
			if v, msg := fd.Check(fd.Default.Literal); msg == "" {
				out[fd.Name] = v
			}
		}
	}
	return out
}
