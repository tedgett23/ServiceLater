package clone

import (
	"slices"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/expr"
)

// MaskedFields are the extra fields marked mask: true in the installed forms and catalog items, by name.
func MaskedFields(r *apps.Registry) []string {
	var out []string
	add := func(f *apps.Form) {
		if f == nil {
			return
		}
		for _, fd := range f.Fields {
			if fd.Mask && !fd.Core() {
				out = append(out, fd.Name)
			}
		}
	}
	for _, t := range r.RecordTypes() {
		add(r.Form(t))
	}
	for _, it := range r.CatalogItems() {
		add(it.Form)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Compiler compiles exclusions in the rule environment, with each record type's form fields.
func Compiler(r *apps.Registry) func(recordType, when string) (*expr.Program, error) {
	return func(recordType, when string) (*expr.Program, error) {
		var fields expr.FieldTypes
		if f := r.Form(recordType); f != nil {
			fields = f.Types()
		}
		env, err := expr.RuleFor(fields)
		if err != nil {
			return nil, err
		}
		return env.Compile(when)
	}
}
