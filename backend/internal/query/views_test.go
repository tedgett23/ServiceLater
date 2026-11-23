package query_test

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/query"
)

// viewWorld is a small instance seen by four people who may all run queries:
//   - Ada, an admin: everything, restricted groups included;
//   - Ann, an analyst (records:read:*, query:run): every record but the one routed to the restricted HR group, no
//     internal notes, no CMDB, no directory;
//   - Ron, an end user with query:run only: what he requested, published articles for everyone, himself;
//   - Hal, in HR with Ann's permissions: the HR record too, because he is in the group.
type viewWorld struct {
	pool               *pgxpool.Pool
	r                  *query.Runner
	ada, ann, ron, hal uuid.UUID
	mine, hrs, theirs  uuid.UUID // Ron's incident; Ada's incident routed to HR; Ada's change
	ci                 uuid.UUID
	appPrincipal       uuid.UUID
}

func newViewWorld(t *testing.T) *viewWorld {
	t.Helper()
	pool, r, ada, _ := setup(t)
	ctx := context.Background()
	w := &viewWorld{pool: pool, r: r, ada: ada}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	id := func(sql string, args ...any) uuid.UUID {
		t.Helper()
		var v uuid.UUID
		if err := pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return v
	}
	person := func(name, role string) uuid.UUID {
		return id(`INSERT INTO users (email, name, role) VALUES ($1, $2, $3) RETURNING id`, strings.ToLower(name)+"@example.com", name, role)
	}
	w.ann, w.ron, w.hal = person("Ann", "end_user"), person("Ron", "end_user"), person("Hal", "end_user")
	w.appPrincipal = id(`INSERT INTO users (email, name, app_name) VALUES ('app@example.com', 'Some app', 'some-app') RETURNING id`)
	exec(`INSERT INTO roles (name, label, permissions) VALUES
		('test-analyst', 'Analyst', '{records:read:*,query:run}'), ('test-querier', 'Querier', '{query:run}')`)
	exec(`INSERT INTO role_grants (role, user_id) VALUES ('test-analyst', $1), ('test-analyst', $2), ('test-querier', $3)`, w.ann, w.hal, w.ron)
	hr := id(`INSERT INTO groups (name, restricted) VALUES ('HR', true) RETURNING id`)
	exec(`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, hr, w.hal)

	task := func(typ, title string, requester uuid.UUID, group any) uuid.UUID {
		v := id(`INSERT INTO tasks (type, title, requester_id, assignment_group_id) VALUES ($1, $2, $3, $4) RETURNING id`, typ, title, requester, group)
		exec(fmt.Sprintf(`INSERT INTO %ss (task_id) VALUES ($1)`, typ), v)
		return v
	}
	w.mine = task("incident", "Ron's printer", w.ron, nil)
	w.hrs = task("incident", "A grievance", ada, hr)
	w.theirs = task("change", "Patch the servers", ada, nil)
	exec(`INSERT INTO task_comments (task_id, author_id, body, internal) VALUES
		($1, $2, 'On it', false), ($1, $2, 'Ron again', true), ($3, $2, 'Confidential', false)`, w.mine, ada, w.hrs)
	exec(`INSERT INTO task_slas (task_id, app, sla, definition, checksum, stage, target_ms)
		VALUES ($1, 'x', 'respond', '{"label": "Respond"}', 'c', 'paused', 1000), ($2, 'x', 'respond', '{}', 'c', 'paused', 1000)`, w.mine, w.hrs)
	w.ci = id(`INSERT INTO cis (class, name) VALUES ('server', 'db-01') RETURNING id`)
	exec(`INSERT INTO task_cis (task_id, ci_id) VALUES ($1, $2)`, w.theirs, w.ci)
	exec(`INSERT INTO kb_articles (title, status, audience) VALUES
		('Reset your password', 'published', 'everyone'), ('Restart the VPN', 'published', 'internal'), ('Half written', 'draft', 'everyone')`)
	return w
}

// rows runs sql as someone and gives the first column of every row, as text, sorted.
func (w *viewWorld) rows(t *testing.T, who uuid.UUID, sql string) []string {
	t.Helper()
	res, err := w.r.Run(context.Background(), query.Request{User: who, SQL: sql})
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	out := []string{}
	for _, row := range res.Rows {
		out = append(out, fmt.Sprint(row[0]))
	}
	sort.Strings(out)
	return out
}

func TestQueryViewsShowWhatEachPersonMaySee(t *testing.T) {
	w := newViewWorld(t)
	names := func(ids ...uuid.UUID) []string {
		out := []string{}
		for _, id := range ids {
			out = append(out, id.String())
		}
		sort.Strings(out)
		return out
	}
	for _, c := range []struct {
		name, sql string
		want      map[uuid.UUID][]string
	}{
		{"records", "SELECT id FROM records", map[uuid.UUID][]string{
			w.ada: names(w.mine, w.hrs, w.theirs), w.ann: names(w.mine, w.theirs), w.ron: names(w.mine), w.hal: names(w.mine, w.hrs, w.theirs)}},
		{"incidents", "SELECT record_id FROM incidents", map[uuid.UUID][]string{
			w.ada: names(w.mine, w.hrs), w.ann: names(w.mine), w.ron: names(w.mine), w.hal: names(w.mine, w.hrs)}},
		{"changes", "SELECT record_id FROM changes", map[uuid.UUID][]string{
			w.ada: names(w.theirs), w.ann: names(w.theirs), w.ron: {}}},
		{"comments", "SELECT body FROM comments", map[uuid.UUID][]string{
			w.ada: {"Confidential", "On it", "Ron again"}, w.ann: {"On it"}, w.ron: {"On it"}, w.hal: {"Confidential", "On it"}}},
		{"slas", "SELECT label FROM slas", map[uuid.UUID][]string{
			w.ada: {"Respond", "respond"}, w.ann: {"Respond"}, w.ron: {"Respond"}}},
		{"people", "SELECT name FROM people", map[uuid.UUID][]string{
			w.ada: {"Ada", "Ann", "Bob", "Hal", "Ron"}, w.ann: {"Ann"}, w.ron: {"Ron"}}},
		{"cis", "SELECT name FROM cis", map[uuid.UUID][]string{w.ada: {"db-01"}, w.ann: {}, w.ron: {}}},
		{"record_cis", "SELECT record_id FROM record_cis", map[uuid.UUID][]string{w.ada: names(w.theirs), w.ann: {}}},
		{"articles", "SELECT title FROM articles", map[uuid.UUID][]string{
			w.ada: {"Half written", "Reset your password", "Restart the VPN"}, w.ann: {"Reset your password"}, w.ron: {"Reset your password"}}},
		{"groups", "SELECT name FROM groups", map[uuid.UUID][]string{w.ron: {"HR"}}},
		{"record history", "SELECT DISTINCT subject_id FROM history WHERE subject = 'task'", map[uuid.UUID][]string{
			w.ann: names(w.mine, w.theirs), w.ron: names(w.mine)}},
		// Only this item's: history outlives the items other tests made and removed.
		{"item history", "SELECT DISTINCT subject_id FROM history WHERE subject = 'ci' AND subject_id = '" + w.ci.String() + "'", map[uuid.UUID][]string{
			w.ada: names(w.ci), w.ann: {}}},
	} {
		for who, want := range c.want {
			if got := w.rows(t, who, c.sql); !slices.Equal(got, want) {
				t.Errorf("%s as %s: got %v, want %v", c.name, w.nameOf(who), got, want)
			}
		}
	}

	// References are the server's: prefix and number.
	if got := w.rows(t, w.ron, "SELECT reference FROM records"); len(got) != 1 || !regexp.MustCompile(`^INC\d+$`).MatchString(got[0]) {
		t.Errorf("reference: %v", got)
	}
	// An API key narrowed to query:run sees what Ron sees: no record permission, so only what the person requested
	// (here nothing: Ada requested none of Ron's).
	res, err := w.r.Run(context.Background(), query.Request{User: w.ada, Scopes: []string{"query:run"}, SQL: "SELECT id FROM records WHERE requester_id <> $1::uuid",
		Params: []string{w.ada.String()}})
	if err != nil || len(res.Rows) != 0 {
		t.Errorf("narrowed key: %v %v", res, err)
	}
}

func (w *viewWorld) nameOf(id uuid.UUID) string {
	switch id {
	case w.ada:
		return "Ada"
	case w.ann:
		return "Ann"
	case w.ron:
		return "Ron"
	case w.hal:
		return "Hal"
	}
	return id.String()
}

// Every query view filters by the signed identity and nothing the person controls, keeps the person's predicates
// behind its own (security_barrier), and says what it and each column hold (the editor shows it).
func TestQueryViewsTrustOnlyTheIdentity(t *testing.T) {
	pool, _, _, _ := setup(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DROP VIEW IF EXISTS query.test_people"); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT c.relname::text, pg_get_viewdef(c.oid, true),
			coalesce(c.reloptions @> '{security_barrier=true}', false), obj_description(c.oid, 'pg_class') IS NOT NULL,
			coalesce((SELECT array_agg(a.attname::text ORDER BY a.attnum) FROM pg_attribute a
			          WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped AND col_description(c.oid, a.attnum) IS NULL), '{}')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'query'
		WHERE c.relkind IN ('v', 'm') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	trusted := regexp.MustCompile(`app_query_identity\(\)|\bquery\.\w+`)
	forbidden := regexp.MustCompile(`current_setting|set_config|app_can|app_current_user_id|app_on_behalf_of|app_sees_task|app_user_|app_hidden|app_permissions_of|current_user|session_user`)
	n := 0
	for rows.Next() {
		var name, def string
		var barrier, described bool
		var undescribed []string
		if err := rows.Scan(&name, &def, &barrier, &described, &undescribed); err != nil {
			t.Fatal(err)
		}
		n++
		if !barrier {
			t.Errorf("%s is not a security_barrier view", name)
		}
		if !described {
			t.Errorf("%s has no COMMENT", name)
		}
		if len(undescribed) > 0 {
			t.Errorf("%s: columns without a COMMENT: %v", name, undescribed)
		}
		if m := forbidden.FindString(def); m != "" {
			t.Errorf("%s reads %s, which the person writing the query controls", name, m)
		}
		if !trusted.MatchString(def) {
			t.Errorf("%s neither checks app_query_identity() nor goes through another query view:\n%s", name, def)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 15 {
		t.Errorf("only %d query views", n)
	}
}

// Every view can be read through the query role. Planning a query on a table inlines the SQL functions in its index
// expressions as the person running it, so a function there that names the public schema (which the role may not
// use) broke every query on that table (app_record_words did, for tasks).
func TestEveryQueryViewCanBeRead(t *testing.T) {
	w := newViewWorld(t)
	ctx := context.Background()
	if _, err := w.pool.Exec(ctx, "DROP VIEW IF EXISTS query.test_people"); err != nil {
		t.Fatal(err)
	}
	views, err := w.r.Schema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		sql := "SELECT * FROM " + v.Name
		if _, err := w.r.Run(ctx, query.Request{User: w.ada, SQL: sql}); err != nil {
			t.Errorf("%s: %v", sql, err)
		}
		// With a filter on every column too, so that each index can be considered.
		for _, c := range v.Columns {
			sql := fmt.Sprintf("SELECT 1 FROM %s WHERE %s IS NOT NULL", v.Name, c.Name)
			if _, err := w.r.Run(ctx, query.Request{User: w.ada, SQL: sql}); err != nil {
				t.Errorf("%s: %v", sql, err)
			}
		}
	}
}
