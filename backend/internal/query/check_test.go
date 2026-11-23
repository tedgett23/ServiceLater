package query

import (
	"errors"
	"strings"
	"testing"
)

var testViews = map[string]bool{"incidents": true, "comments": true}

func TestCheckAllowsReading(t *testing.T) {
	for _, sql := range []string{
		"SELECT * FROM incidents",
		"select reference, title from query.incidents where priority <= 2 order by opened_at desc limit 10;",
		"SELECT assignment_group, count(*) FILTER (WHERE breached) AS breached, avg(extract(epoch FROM resolved_at - opened_at)) FROM incidents GROUP BY 1 HAVING count(*) > 3",
		"WITH open AS (SELECT * FROM incidents WHERE status_category = 'open') SELECT o.reference FROM open o JOIN comments c ON c.record = o.reference",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 10) SELECT i FROM n",
		"SELECT date_trunc('week', opened_at) AS week, count(*) FROM incidents WHERE opened_at > now() - interval '90 days' GROUP BY 1",
		"SELECT reference, row_number() OVER (PARTITION BY assignment_group ORDER BY opened_at) FROM incidents",
		"SELECT fields ->> 'category', coalesce(nullif(title, ''), '(none)') FROM incidents WHERE title ILIKE $1 AND opened_at BETWEEN $2 AND $3",
		"SELECT CASE WHEN priority = 1 THEN 'P1' ELSE 'other' END, greatest(1, 2), current_date, opened_at AT TIME ZONE 'UTC' FROM incidents",
		"SELECT reference FROM incidents WHERE reference IN (SELECT record FROM comments) AND EXISTS (SELECT 1 FROM comments)",
		"SELECT x FROM generate_series(1, 3) AS g(x)",
		"SELECT '1'::int, CAST('2026-01-01' AS date), title::text FROM incidents",
		"SELECT reference FROM incidents UNION SELECT record FROM comments",
		"SELECT * FROM (VALUES (1, 'a'), (2, 'b')) v(n, s)",
		"SELECT substring(title FROM 1 FOR 5), position('x' IN title), trim(both ' ' FROM title) FROM incidents",
	} {
		if _, err := Check(sql, testViews); err != nil {
			t.Errorf("%s\nrefused: %v", sql, err)
		}
	}
}

func TestCheckRefusesEverythingElse(t *testing.T) {
	for sql, want := range map[string]string{ //nolint:gosec // SQL that names settings and secrets, not credentials
		// settings and identity
		"SELECT set_config('app.permissions', ',records:read:*,', true)":        "set_config() is not a function",
		"SELECT current_setting('query.token')":                                 "current_setting()",
		"SELECT * FROM incidents WHERE set_config('a', 'b', false) IS NOT NULL": "set_config()",
		"SELECT pg_catalog.set_config('a', 'b', false)":                         "pg_catalog.set_config()",
		"SELECT \"set_config\"('a', 'b', false)":                                "set_config()",
		"SET app.permissions = ',records:read:*,'":                              "only SELECT",
		"RESET ROLE":            "only SELECT",
		"SET ROLE servicelater": "only SELECT",
		"SELECT current_user":   "CURRENT_USER is not allowed",
		// other tables and schemas
		"SELECT * FROM tasks":                                                      "tasks is not something queries can read",
		"SELECT * FROM public.tasks":                                               "public.tasks",
		"SELECT * FROM pg_catalog.pg_authid":                                       "pg_catalog.pg_authid",
		"SELECT * FROM pg_class":                                                   "pg_class",
		"SELECT * FROM information_schema.tables":                                  "information_schema.tables",
		"SELECT * FROM query.incidents i, secrets s":                               "secrets",
		"SELECT * FROM otherdb.query.incidents":                                    "query.incidents is not",
		"SELECT * FROM incidents WHERE title = (SELECT key::text FROM query_keys)": "query_keys",
		// a WITH name is only visible where SQL makes it visible
		"WITH a AS (SELECT * FROM pg_class) SELECT * FROM a":                             "pg_class",
		"WITH a AS (SELECT * FROM a) SELECT * FROM a":                                    "a is not something",
		"SELECT * FROM (WITH pg_roles AS (SELECT 1) SELECT * FROM pg_roles) x, pg_roles": "pg_roles is not",
		// writing, in any form
		"INSERT INTO incidents VALUES (1)":                                        "only SELECT",
		"WITH x AS (DELETE FROM incidents RETURNING *) SELECT * FROM x":           "a WITH query may only SELECT",
		"WITH x AS (UPDATE incidents SET title = '' RETURNING *) SELECT * FROM x": "a WITH query may only SELECT",
		"SELECT * INTO copy FROM incidents":                                       "SELECT INTO makes a table",
		"SELECT * FROM incidents FOR UPDATE":                                      "FOR UPDATE",
		"SELECT * FROM incidents FOR SHARE":                                       "FOR UPDATE",
		"COPY (SELECT 1) TO '/tmp/x'":                                             "only SELECT",
		"CREATE TABLE x (a int)":                                                  "only SELECT",
		"DO $$ BEGIN END $$":                                                      "only SELECT",
		"SELECT 1; DELETE FROM incidents":                                         "one statement",
		"SELECT 1; SELECT 2":                                                      "one statement",
		"EXPLAIN ANALYZE SELECT 1":                                                "only SELECT",
		"PREPARE p AS SELECT 1":                                                   "only SELECT",
		"BEGIN":                                                                   "only SELECT",
		"COMMIT":                                                                  "only SELECT",
		"LISTEN x":                                                                "only SELECT",
		"LOCK TABLE incidents":                                                    "only SELECT",
		// functions that read files, other sessions, run SQL from text, wait or lock
		"SELECT pg_read_file('/etc/passwd')":                              "pg_read_file()",
		"SELECT pg_sleep(100)":                                            "pg_sleep()",
		"SELECT query_to_xml('select * from secrets', true, true, '')":    "query_to_xml()",
		"SELECT lo_import('/etc/passwd')":                                 "lo_import()",
		"SELECT pg_advisory_lock(1)":                                      "pg_advisory_lock()",
		"SELECT dblink('host=x', 'select 1')":                             "dblink()",
		"SELECT * FROM pg_stat_activity":                                  "pg_stat_activity",
		"SELECT pg_terminate_backend(1)":                                  "pg_terminate_backend()",
		"SELECT public.app_permissions_of(gen_random_uuid())":             "public.app_permissions_of()",
		"SELECT app_query_identity()":                                     "app_query_identity()",
		"SELECT txid_current()":                                           "txid_current()",
		"SELECT version()":                                                "version()",
		"SELECT nextval('task_number_incident')":                          "nextval()",
		"SELECT * FROM generate_series(1, 2) g, LATERAL pg_ls_dir('.') d": "pg_ls_dir()",
		// casts that look things up
		"SELECT 'secrets'::regclass":         "cast to regclass",
		"SELECT CAST('pg_sleep' AS regproc)": "cast to regproc",
		"SELECT '1'::public.mytype":          "cast to public.mytype",
		// operators from elsewhere and other syntax
		"SELECT 1 OPERATOR(public.+) 1":                                                                         "the operator public.+",
		"SELECT title FROM incidents ORDER BY title USING <":                                                    "USING",
		"SELECT xmlparse(document '<a/>')":                                                                      "XML",
		"SELECT title COLLATE \"C\" FROM incidents":                                                             "COLLATE",
		"SELECT * FROM incidents TABLESAMPLE SYSTEM (10)":                                                       "TABLESAMPLE",
		"SELECT * FROM ROWS FROM (generate_series(1,2)) AS t(a int)":                                            "not allowed",
		"WITH RECURSIVE t(n) AS (SELECT 1 UNION SELECT n FROM t) SEARCH DEPTH FIRST BY n SET o SELECT * FROM t": "SEARCH and CYCLE",
		// not queries at all
		"":        "write a query first",
		"SELEC 1": "syntax error",
	} {
		_, err := Check(sql, testViews)
		var r *Refused
		if !errors.As(err, &r) {
			t.Errorf("%s\nran, want refused (%q)", sql, want)
			continue
		}
		if !strings.Contains(r.Error(), want) {
			t.Errorf("%s\nrefused with %q, want it to say %q", sql, r.Error(), want)
		}
	}
}

func TestCheckSaysWhere(t *testing.T) {
	_, err := Check("SELECT title,\n  set_config('a', 'b', true)\nFROM incidents", testViews)
	var r *Refused
	if !errors.As(err, &r) || r.Line != 2 || r.Column != 3 {
		t.Fatalf("got %#v, want line 2 column 3", err)
	}
	_, err = Check("SELECT title\nFROM incidents\nWHERE (", testViews)
	if !errors.As(err, &r) || r.Line != 3 {
		t.Fatalf("got %#v, want a syntax error on line 3", err)
	}
}

func TestCheckCountsParametersAndTrims(t *testing.T) {
	c, err := Check("  SELECT * FROM incidents WHERE priority = $2 AND title = $1 ;; \n", testViews)
	if err != nil {
		t.Fatal(err)
	}
	if c.Params != 2 || c.SQL != "SELECT * FROM incidents WHERE priority = $2 AND title = $1" {
		t.Fatalf("got %+v", c)
	}
	if _, err := Check("SELECT $51", testViews); err == nil {
		t.Fatal("$51 was allowed")
	}
}

func TestCheckRefusesOversizedQueries(t *testing.T) {
	if _, err := Check("SELECT "+strings.Repeat("1+", MaxLength)+"1", testViews); err == nil {
		t.Fatal("an oversized query was allowed")
	}
}
