package apps

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/servicelater/servicelater/backend/internal/authz"
)

// BuiltInRoles are the base roles every person has one of (users.role). They are the platform's, so an app cannot
// define or replace them, and they are not granted: they are set on the person.
var BuiltInRoles = []string{"admin", "agent", "end_user"}

// Role is a named set of permissions an app ships in roles/. Admins grant it to people or groups; a person can do
// what their base role and every role granted to them (or to a group they are in) allows. Role names are global:
// two apps cannot both define one.
type Role struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Permissions []string `json:"permissions"`
}

type roleFile struct {
	Kind        string   `yaml:"kind"`
	Name        string   `yaml:"name"`
	Label       string   `yaml:"label"`
	Description string   `yaml:"description"`
	Permissions []string `yaml:"permissions"`
}

func parseRole(file string, data []byte, p *Problems) *Role {
	var f roleFile
	if err := decodeStrict(data, &f); err != nil {
		p.add(file, "%v", err)
		return nil
	}
	if f.Kind != "role" {
		p.add(file, "kind must be role in this folder, got %q", f.Kind)
		return nil
	}
	r := &Role{Name: f.Name, Label: strings.TrimSpace(f.Label), Description: f.Description, Permissions: f.Permissions}
	before := len(*p)
	r.validate(file, p)
	if len(*p) > before {
		return nil
	}
	slices.Sort(r.Permissions) // stored in one order, so reordering the file is not a change
	return r
}

func (r *Role) validate(file string, p *Problems) {
	switch {
	case slices.Contains(BuiltInRoles, r.Name):
		p.add(file, "name %q is a built-in role, which apps cannot define", r.Name)
	case !ruleNameRE.MatchString(r.Name):
		p.add(file, "name %q must be 2-63 characters: lowercase letters, digits and hyphens, starting with a letter", r.Name)
	}
	if n := utf8.RuneCountInString(r.Label); n < 1 || n > 100 {
		p.add(file, "label is required: what admins see, at most 100 characters")
	}
	if utf8.RuneCountInString(r.Description) > 1000 {
		p.add(file, "description must be at most 1000 characters")
	}
	if len(r.Permissions) == 0 {
		p.add(file, "permissions is required: what this role allows")
	}
	seen := map[string]bool{}
	for _, perm := range r.Permissions {
		switch {
		case !authz.Known(perm):
			p.add(file, "permissions: %q is not a permission (available: %s)", perm, permissionNames())
		case seen[perm]:
			p.add(file, "permissions: %q is listed twice", perm)
		}
		seen[perm] = true
	}
}

func permissionNames() string {
	names := make([]string, len(authz.Catalog))
	for i, c := range authz.Catalog {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}
