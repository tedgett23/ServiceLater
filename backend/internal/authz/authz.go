// Package authz is the permission model: what a person (or, later, an app) may do, as named permissions that roles
// bundle. Go checks a permission for a fast refusal before the database (auth.RequirePermission); Postgres checks the
// same names in RLS (app_can, migrations/00001_foundation.sql), and that is the check that counts.
//
// A person's permissions are the union of their base role (users.role: admin, agent or end_user), the roles granted
// to them, and the roles granted to any group they are in. A deactivated person has none.
//
// Record permissions are scoped by record type: records:<verb>:<type>, with * for every type (records:update:*). The
// verbs are fixed, so a new record type adds catalog entries (RecordCatalog), never a new check in code: code asks
// Set.CanRecord(verb, type), and SQL app_can_record(verb, type).
package authz

import (
	"regexp"
	"slices"
	"strings"
)

// Permission names. The database knows them only as text, so these must match the roles seeded in migrations/00004_access.sql
// and the policies that call app_can.
const (
	CommentsInternal = "comments:internal" // read and write internal notes
	CommentsDelete   = "comments:delete"   // delete a comment
	WorkflowsReadAll = "workflows:read_all"
	WorkflowsRun     = "workflows:run"     // start, cancel and advance workflow runs on any ticket
	WorkflowsEdit    = "workflows:edit"    // change, add and reset workflows in the builder
	CatalogManage    = "catalog:manage"    // change, add and reset service catalog items in the editor
	FormsManage      = "forms:manage"      // change record types' forms in the form editor
	ScriptsEdit      = "scripts:edit"      // change scripts in workflows and rules, and test-run them
	SLAsManage       = "slas:manage"       // choose which hours each SLA counts
	ReportsRead      = "reports:read"      // read reports (the SLA report)
	QueryRun         = "query:run"         // run read-only SQL queries over the query views
	InstanceBackup   = "instance:backup"   // take, download and restore snapshots of the instance
	UsersRead        = "users:read"        // the people directory and pickers
	UsersManage      = "users:manage"      // add, edit, deactivate and delete people, and set passwords
	GroupsManage     = "groups:manage"     // create and change groups and who is in them
	RolesManage      = "roles:manage"      // grant and revoke roles
	FreezesManage    = "freezes:manage"    // declare, change and lift change freeze windows
	CalendarsManage  = "calendars:manage"  // add, change and remove the business calendars SLAs count by
	CMDBRead         = "cmdb:read"         // see configuration items, how they relate, and which records they are linked to
	CMDBManage       = "cmdb:manage"       // add, change and remove configuration items and their relationships
	SettingsManage   = "settings:manage"   // change instance settings (email) and set the secrets they use
	KnowledgeWrite   = "knowledge:write"   // draft articles, edit drafts and send them for review
	KnowledgePublish = "knowledge:publish" // publish, change, retire and delete articles
	// KnowledgeReadInternal reads published articles meant for staff (audience internal).
	KnowledgeReadInternal = "knowledge:read_internal"
	// RestrictedRead sees records routed to restricted groups the person is not in (still only types they may read).
	RestrictedRead = "restricted:read"
)

// Record verbs: what records:<verb>:<type> allows on records of that type. A requester always sees, and comments on,
// their own records, whatever the type.
const (
	Read   = "read"   // see every record of the type, not only the ones you asked for
	Create = "create" // file one (unless the type is open to everyone); with Update, file one for someone else
	Update = "update" // edit and move them, and be assigned one
	Delete = "delete" // delete them
)

// Verbs are the record verbs, in catalog order.
var Verbs = []string{Read, Create, Update, Delete}

// AllTypes in a record permission means every record type.
const AllTypes = "*"

// Record is the permission for a verb on a record type (or AllTypes).
func Record(verb, recordType string) string { return "records:" + verb + ":" + recordType }

// ParseRecord splits a record permission into its verb and type; ok is false for anything else.
func ParseRecord(perm string) (verb, recordType string, ok bool) {
	rest, found := strings.CutPrefix(perm, "records:")
	if !found {
		return "", "", false
	}
	verb, recordType, found = strings.Cut(rest, ":")
	if !found || !slices.Contains(Verbs, verb) || !recordTypeRE.MatchString(recordType) {
		return "", "", false
	}
	return verb, recordType, true
}

var recordTypeRE = regexp.MustCompile(`^(\*|[a-z][a-z0-9_]{1,30})$`)

var verbDescriptions = map[string]string{
	Read:   "See every %s, not only the ones they asked for",
	Create: "File %s records (and, with update, file them for someone else)",
	Update: "Edit and move %s records, and be assigned them",
	Delete: "Delete %s records",
}

// RecordCatalog is the record permissions for these record types (as labels by type), every-type ones first.
func RecordCatalog(types []string, labels map[string]string) []Permission {
	var out []Permission
	for _, t := range append([]string{AllTypes}, types...) {
		label := labels[t]
		if t == AllTypes {
			label = "every type of"
		} else if label == "" {
			label = t
		}
		for _, v := range Verbs {
			desc := strings.Replace(verbDescriptions[v], "%s", label, 1)
			if t == AllTypes && v == Read {
				desc = "See every record of every type"
			}
			out = append(out, Permission{Name: Record(v, t), Description: desc})
		}
	}
	return out
}

// Permission is one entry of the catalog.
type Permission struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Catalog is every permission the platform checks other than the record ones (RecordCatalog), for the admin UI and for
// validating role definitions.
var Catalog = []Permission{
	{CommentsInternal, "Read and write internal notes"},
	{CommentsDelete, "Delete comments"},
	{WorkflowsReadAll, "See every workflow run"},
	{WorkflowsRun, "Start, cancel and advance workflow runs on any ticket"},
	{WorkflowsEdit, "Change, add and reset workflows in the workflow builder"},
	{CatalogManage, "Change, add and reset service catalog items, and see every item"},
	{FormsManage, "Change record types' forms: their fields, conditions and layout"},
	{ScriptsEdit, "Change the Starlark scripts in workflow steps and rules, and test-run them"},
	{SLAsManage, "Choose which hours each SLA counts, by priority, team or any condition"},
	{ReportsRead, "Read reports: how records met their SLAs"},
	{QueryRun, "Run read-only SQL queries over what they may see, and keep a history of them"},
	{InstanceBackup, "Take, download and restore snapshots of the whole instance (a restore replaces everything)"},
	{UsersRead, "Look people up in the directory and pickers"},
	{UsersManage, "Add, edit, deactivate and delete people, and set their passwords"},
	{GroupsManage, "Create and change groups and their members"},
	{RolesManage, "Grant and revoke roles"},
	{FreezesManage, "Declare, change and lift change freezes"},
	{CalendarsManage, "Add, change and remove business calendars"},
	{CMDBRead, "See configuration items, how they relate and the records linked to them"},
	{CMDBManage, "Add, change and remove configuration items and their relationships"},
	{SettingsManage, "Change instance settings such as email, and set the passwords they use"},
	{KnowledgeWrite, "Draft knowledge articles and send them for review"},
	{KnowledgePublish, "Publish, change, retire and delete knowledge articles"},
	{KnowledgeReadInternal, "Read knowledge articles meant for staff"},
	{RestrictedRead, "See records routed to restricted groups they are not in"},
}

// Known reports whether name is a permission the platform checks: one of the Catalog, or a well-formed record
// permission (whether its type exists is for the caller, which knows the record types, to check).
func Known(name string) bool {
	if _, _, ok := ParseRecord(name); ok {
		return true
	}
	return slices.ContainsFunc(Catalog, func(p Permission) bool { return p.Name == name })
}

// Set is a person's permissions.
type Set []string

// Has reports whether the set grants p.
func (s Set) Has(p string) bool { return slices.Contains(s, p) }

// Any reports whether the set grants at least one of ps.
func (s Set) Any(ps ...string) bool { return slices.ContainsFunc(ps, s.Has) }

// CanRecord reports whether the set allows verb on records of this type, by name or for every type. It mirrors
// app_can_record in SQL.
func (s Set) CanRecord(verb, recordType string) bool {
	return s.Has(Record(verb, recordType)) || s.Has(Record(verb, AllTypes))
}

// CanRecordAny reports whether the set allows verb on records of at least one type.
func (s Set) CanRecordAny(verb string) bool {
	prefix := "records:" + verb + ":"
	return slices.ContainsFunc(s, func(p string) bool { return strings.HasPrefix(p, prefix) })
}

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

// Narrow is what is left of perms once narrowed to scopes, as an API key narrowed to some permissions has: a scope
// perms grants is kept; records:<verb>:<type> is kept when perms grants that verb on every type; records:<verb>:*
// keeps only the record permissions of that verb perms has. It never grants more than perms. It mirrors
// app_narrow_permissions in SQL (TestNarrowingMatchesTheDatabase).
func Narrow(perms Set, scopes []string) Set {
	out := Set{}
	add := func(p string) {
		if !out.Has(p) {
			out = append(out, p)
		}
	}
	for _, s := range scopes {
		verb, typ, isRecord := ParseRecord(s)
		switch {
		case perms.Has(s):
			add(s)
		case isRecord && typ != AllTypes && perms.Has(Record(verb, AllTypes)):
			add(s)
		case isRecord && typ == AllTypes:
			for _, p := range perms {
				if v, _, ok := ParseRecord(p); ok && v == verb {
					add(p)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}
