// Package authz is the permission model: what a person (or, later, an app) may do, as named permissions that roles
// bundle. Go checks a permission for a fast refusal before the database (auth.RequirePermission); Postgres checks the
// same names in RLS (app_can, migration 00019), and that is the check that counts.
//
// A person's permissions are the union of their base role (users.role: admin, agent or end_user), the roles granted
// to them, and the roles granted to any group they are in. A deactivated person has none.
package authz

import (
	"slices"
	"strings"
)

// Permission names. The database knows them only as text, so these must match the roles seeded in migration 00019
// and the policies that call app_can.
const (
	TicketsReadAll   = "tickets:read_all"  // see every ticket, not only the ones you asked for
	TicketsUpdate    = "tickets:update"    // edit and move tickets, file them for others, be assigned one
	TicketsDelete    = "tickets:delete"    // delete a ticket
	CommentsInternal = "comments:internal" // read and write internal notes
	CommentsDelete   = "comments:delete"   // delete a comment
	WorkflowsReadAll = "workflows:read_all"
	WorkflowsRun     = "workflows:run" // start, cancel and advance workflow runs on any ticket
	UsersRead        = "users:read"    // the people directory and pickers
	UsersManage      = "users:manage"  // add, edit, deactivate and delete people, and set passwords
	GroupsManage     = "groups:manage" // create and change groups and who is in them
	RolesManage      = "roles:manage"  // grant and revoke roles
)

// Permission is one entry of the catalog.
type Permission struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Catalog is every permission the platform checks, for the admin UI and for validating role definitions.
var Catalog = []Permission{
	{TicketsReadAll, "See every ticket, not only the ones they asked for"},
	{TicketsUpdate, "Edit and move tickets, file tickets for others, and be assigned tickets"},
	{TicketsDelete, "Delete tickets"},
	{CommentsInternal, "Read and write internal notes"},
	{CommentsDelete, "Delete comments"},
	{WorkflowsReadAll, "See every workflow run"},
	{WorkflowsRun, "Start, cancel and advance workflow runs on any ticket"},
	{UsersRead, "Look people up in the directory and pickers"},
	{UsersManage, "Add, edit, deactivate and delete people, and set their passwords"},
	{GroupsManage, "Create and change groups and their members"},
	{RolesManage, "Grant and revoke roles"},
}

// Known reports whether name is a permission the platform checks.
func Known(name string) bool {
	return slices.ContainsFunc(Catalog, func(p Permission) bool { return p.Name == name })
}

// Set is a person's permissions.
type Set []string

// Has reports whether the set grants p.
func (s Set) Has(p string) bool { return slices.Contains(s, p) }

// Any reports whether the set grants at least one of ps.
func (s Set) Any(ps ...string) bool { return slices.ContainsFunc(ps, s.Has) }

// Parse reads the transaction-local form app_begin stores (",a,b,"), as current_setting('app.permissions') returns it.
func Parse(setting string) Set {
	var out Set
	for _, p := range strings.Split(setting, ",") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
