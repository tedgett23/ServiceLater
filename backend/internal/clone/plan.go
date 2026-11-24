package clone

import (
	"fmt"
	"strings"
)

// Every table in the database has a policy here, so a new table is a decision, not an accident
// (TestEveryTableHasAClonePolicy): left out, taken whole, or filtered by the kept records and people.
//
// The policies are SQL over temporary tables the export fills first, in its own transaction:
//
//	clone_tasks (id)        the records kept
//	clone_runs (id)         the workflow runs kept (of kept records, or of none)
//	clone_users (id, n)     the people kept, numbered for masking
//	clone_mask (field)      the extra fields marked mask: true, when they are masked
//	clone_notes (id)        the notifications kept, with the logs
//
// Every row is read as t.

type policy struct {
	// where filters the rows ("" takes them all; "false" none).
	where func(p Profile) string
	// cols replaces some columns' values (masking, links to rows left out).
	cols func(p Profile) map[string]string
	// brings are columns naming people a kept row needs, for People: referenced. Their where must not depend on
	// clone_users.
	brings []string
}

// Never copied (docs/backup-and-clone.md §2, "What the target keeps"): sign-in state, settings and secrets, queued
// work and counters, the record of snapshots and clones, API keys (production's would be live credentials on the
// target), single sign-on, the query key, workflow schedules (they start counting from the clone) and webhook calls.
var never = []string{
	"sessions", "rate_limits", "rate_counts", "jobs", "goose_db_version",
	"snapshots", "snapshot_restores", "clone_profiles", "clone_packages",
	"settings", "secrets", "sso_providers", "sso_group_maps", "user_identities", "query_keys", "api_keys",
	"workflow_schedules", "workflow_webhook_deliveries",
}

const masked = "'Masked text.'"

func all(Profile) string { return "" }

func task(col string) string {
	return fmt.Sprintf("EXISTS (SELECT 1 FROM clone_tasks k WHERE k.id = t.%s)", col)
}

func taskOrNull(col string) string { return fmt.Sprintf("(t.%s IS NULL OR %s)", col, task(col)) }

func user(col string) string {
	return fmt.Sprintf("EXISTS (SELECT 1 FROM clone_users k WHERE k.id = t.%s)", col)
}

func userOrNull(col string) string { return fmt.Sprintf("(t.%s IS NULL OR %s)", col, user(col)) }

func run(col string) string {
	return fmt.Sprintf("EXISTS (SELECT 1 FROM clone_runs k WHERE k.id = t.%s)", col)
}

// clearTask and clearUser empty a link to a record or person left out.
func clearTask(col string) string { return fmt.Sprintf("CASE WHEN %s THEN t.%s END", task(col), col) }

func clearUser(col string) string { return fmt.Sprintf("CASE WHEN %s THEN t.%s END", user(col), col) }

func text(col string) string {
	return fmt.Sprintf("CASE WHEN t.%[1]s IS NULL OR t.%[1]s = '' THEN t.%[1]s ELSE %s END", col, masked)
}

func cols(pairs ...string) func(Profile) map[string]string {
	return func(Profile) map[string]string {
		out := map[string]string{}
		for i := 0; i+1 < len(pairs); i += 2 {
			out[pairs[i]] = pairs[i+1]
		}
		return out
	}
}

func logs(where string) func(Profile) string {
	return func(p Profile) string {
		if !p.Logs {
			return "false"
		}
		return where
	}
}

// person is a row of users that is a person: not an app's principal, not a service account.
const person = "t.app_name IS NULL AND NOT EXISTS (SELECT 1 FROM service_accounts s WHERE s.user_id = t.id)"

var policies = map[string]policy{
	// Definitions and reference data come whole: apps, their files and studio edits, types, roles, groups,
	// calendars, freezes, the CMDB, the knowledge base and saved query views.
	"apps":                {where: all},
	"app_definitions":     {where: all},
	"app_files":           {where: all},
	"app_overrides":       {where: all, cols: cols("updated_by", clearUser("updated_by"))},
	"history_exclusions":  {where: all},
	"task_types":          {where: all},
	"roles":               {where: all},
	"groups":              {where: all},
	"business_calendars":  {where: all, cols: cols("created_by", clearUser("created_by"))},
	"freeze_windows":      {where: all, cols: cols("created_by", clearUser("created_by"))},
	"freeze_window_cis":   {where: all},
	"cis":                 {where: all, cols: cols("created_by", clearUser("created_by"))},
	"ci_relationships":    {where: all, cols: cols("created_by", clearUser("created_by"))},
	"kb_articles":         {where: all, cols: cols("author_id", clearUser("author_id"), "updated_by", clearUser("updated_by"))},
	"kb_article_versions": {where: all, cols: cols("saved_by", clearUser("saved_by"))},
	"saved_views":         {where: all, brings: []string{"owner_id"}},

	// People.
	"users": {where: func(Profile) string { return user("id") }, cols: func(p Profile) map[string]string {
		out := map[string]string{"manager_id": clearUser("manager_id")}
		n := "(SELECT k.n FROM clone_users k WHERE k.id = t.id)"
		if p.Mask.Emails {
			out["email"] = fmt.Sprintf("CASE WHEN %s THEN 'user-' || %s || '@masked.invalid' ELSE t.email END", person, n)
		}
		if p.Mask.Names {
			out["name"] = fmt.Sprintf("CASE WHEN %s THEN 'Person ' || %s ELSE t.name END", person, n)
		}
		return out
	}},
	"user_credentials": {where: func(p Profile) string {
		if !p.Passwords {
			return "false"
		}
		return user("user_id")
	}},
	"user_phones": {where: func(p Profile) string {
		if p.Mask.Phones {
			return "false"
		}
		return user("user_id")
	}},
	"service_accounts": {where: func(Profile) string { return user("user_id") }, cols: cols("created_by", clearUser("created_by"))},
	"group_members":    {where: func(Profile) string { return user("user_id") }},
	"role_grants": {where: func(Profile) string { return userOrNull("user_id") },
		cols: cols("granted_by", clearUser("granted_by"))},

	// Records and what belongs to them.
	"tasks": {where: func(Profile) string { return task("id") }, brings: []string{"requester_id", "assignee_id"},
		cols: func(p Profile) map[string]string {
			out := map[string]string{"parent_id": clearTask("parent_id")}
			if p.Mask.Text {
				out["description"] = text("description")
			}
			if p.Mask.Fields {
				out["fields"] = "t.fields - COALESCE((SELECT array_agg(m.field) FROM clone_mask m), '{}'::text[])"
			}
			return out
		}},
	"incidents": {where: func(Profile) string { return task("task_id") }, cols: cols("problem_id", clearTask("problem_id"))},
	"changes":   {where: func(Profile) string { return task("task_id") }, cols: cols("problem_id", clearTask("problem_id"))},
	"requests":  {where: func(Profile) string { return task("task_id") }},
	"task_comments": {where: func(p Profile) string {
		if p.InternalNotes {
			return task("task_id")
		}
		return task("task_id") + " AND NOT t.internal"
	}, brings: []string{"author_id", "on_behalf_of"}, cols: func(p Profile) map[string]string {
		if p.Mask.Text {
			return map[string]string{"body": text("body")}
		}
		return nil
	}},
	"task_cis":      {where: func(Profile) string { return task("task_id") }, cols: cols("created_by", clearUser("created_by"))},
	"task_articles": {where: func(Profile) string { return task("task_id") }, cols: cols("created_by", clearUser("created_by"))},
	"task_slas":     {where: func(Profile) string { return task("task_id") }},
	"attachments": {where: func(p Profile) string {
		if p.Attachments == AttachmentsNone {
			return "false"
		}
		w := task("task_id") + " AND t.status = 'ready'"
		if p.AttachmentMaxMB > 0 {
			w += fmt.Sprintf(" AND t.size <= %d", int64(p.AttachmentMaxMB)<<20)
		}
		return w
	}, brings: []string{"created_by"}},
	"record_history": {where: func(p Profile) string {
		if p.History == HistoryNone {
			return "false"
		}
		// Settings are the target's own, so their history is too.
		w := "((t.subject = 'task' AND " + task("subject_id") + ") OR t.subject = 'ci')"
		if p.History == HistoryRecent {
			w += fmt.Sprintf(" AND t.at > now() - interval '%d days'", p.HistoryDays)
		}
		return w
	}, brings: []string{"actor_id", "on_behalf_of"}, cols: func(p Profile) map[string]string {
		var hide []string
		if p.Mask.Text {
			hide = append(hide, "t.field = 'description'")
		}
		if p.Mask.Fields {
			hide = append(hide, "t.field IN (SELECT 'fields.' || m.field FROM clone_mask m)")
		}
		if len(hide) == 0 {
			return nil
		}
		when := "t.subject = 'task' AND (" + strings.Join(hide, " OR ") + ")"
		value := func(col string) string {
			return fmt.Sprintf("CASE WHEN %s AND t.%[2]s IS NOT NULL THEN to_jsonb(%s::text) ELSE t.%[2]s END", when, col, masked)
		}
		return map[string]string{"old": value("old"), "new": value("new")}
	}},
	"workflow_executions": {where: func(Profile) string { return run("id") }, brings: []string{"started_by", "run_as"},
		cols: func(p Profile) map[string]string {
			if p.Mask.Text {
				return map[string]string{"vars": "'{}'::jsonb"}
			}
			return nil
		}},
	"workflow_steps": {where: func(Profile) string { return run("execution_id") }, cols: func(p Profile) map[string]string {
		if p.Mask.Text {
			return map[string]string{"detail": "'{}'::jsonb"}
		}
		return nil
	}},
	"workflow_approvals": {where: func(Profile) string { return run("execution_id") }, brings: []string{"approver_id"},
		cols: func(p Profile) map[string]string {
			if p.Mask.Text {
				return map[string]string{"comment": text("comment")}
			}
			return nil
		}},

	// What was sent and run: only with Logs.
	"action_runs": {where: logs(taskOrNull("record_id"))},
	"query_runs":  {where: logs(user("user_id")), cols: cols("api_key_id", "NULL::uuid")},
	"notifications": {where: logs("EXISTS (SELECT 1 FROM clone_notes k WHERE k.id = t.id)"),
		cols: cols("actor_id", clearUser("actor_id"))},
	"email_deliveries": {where: logs(userOrNull("user_id")), cols: func(p Profile) map[string]string {
		out := map[string]string{"notification_id": "CASE WHEN EXISTS (SELECT 1 FROM clone_notes k WHERE k.id = t.notification_id) THEN t.notification_id END"}
		if p.Mask.Emails {
			out["to_address"] = "'masked@masked.invalid'"
		}
		return out
	}},
	"channel_deliveries": {where: logs(userOrNull("user_id") + " AND " + taskOrNull("task_id")), cols: func(p Profile) map[string]string {
		out := map[string]string{"actor_id": clearUser("actor_id"),
			"notification_id": "CASE WHEN EXISTS (SELECT 1 FROM clone_notes k WHERE k.id = t.notification_id) THEN t.notification_id END"}
		if p.Mask.Emails || p.Mask.Phones {
			out["to_address"] = "'masked'"
		}
		return out
	}},
	"inbound_emails": {where: logs(""), cols: func(p Profile) map[string]string {
		out := map[string]string{"task_id": clearTask("task_id"), "user_id": clearUser("user_id")}
		if p.Mask.Emails {
			out["from_address"] = "'masked@masked.invalid'"
		}
		if p.Mask.Text {
			out["subject"] = text("subject")
		}
		return out
	}},
}

// selectFor is the query giving a table's rows as the clone takes them, its columns in order.
func selectFor(table string, columns []string, p Profile) (string, bool) {
	pol, ok := policies[table]
	if !ok {
		return "", false
	}
	var over map[string]string
	if pol.cols != nil {
		over = pol.cols(p)
	}
	list := make([]string, len(columns))
	for i, c := range columns {
		if e, ok := over[c]; ok {
			list[i] = e + " AS " + quote(c)
		} else {
			list[i] = "t." + quote(c)
		}
	}
	q := "SELECT " + strings.Join(list, ", ") + " FROM " + quote(table) + " t"
	if w := pol.where(p); w != "" {
		q += " WHERE " + w
	}
	return q, true
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
