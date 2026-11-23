package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Queries are for query:run holders (admins, core-itsm's analyst). What one sees is the view's to decide, as them; a
// query that is not allowed or fails says why and where. Every attempt is kept, and each person sees their own.
func TestRunningAQuery(t *testing.T) {
	h, pool := setup(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP VIEW IF EXISTS query.test_people; CREATE VIEW query.test_people WITH (security_barrier) AS
		SELECT u.name FROM app_query_identity() me, users u
		WHERE u.id = me.user_id OR position(',users:read,' IN me.perms) > 0`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP VIEW IF EXISTS query.test_people") })
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")

	expect(t, agent.do(h, "QUERY", "/api/v1/query", `{"sql":"SELECT 1"}`), 403, "an agent running a query")
	expect(t, agent.do(h, "POST", "/api/v1/query", `{"sql":"SELECT 1"}`), 403, "an agent running a query by POST")
	expect(t, agent.do(h, "GET", "/api/v1/query/schema", ""), 403, "an agent reading the views")

	type result struct {
		Columns []struct{ Name, Type string }
		Rows    [][]any
	}
	w := admin.do(h, "QUERY", "/api/v1/query", `{"sql":"SELECT name FROM test_people WHERE name ILIKE $1 ORDER BY 1","params":["%x.test%"]}`)
	expect(t, w, 200, "an admin's query")
	// POST is the same query, for clients and networks that cannot send QUERY.
	expect(t, admin.do(h, "POST", "/api/v1/query", `{"sql":"SELECT 1"}`), 200, "the same by POST")
	if r := decodeBody[result](t, w); len(r.Columns) != 1 || r.Columns[0].Type != "text" || len(r.Rows) == 0 {
		t.Fatalf("result: %+v", r)
	}

	// An analyst may query, and sees what the role lets them.
	expect(t, admin.do(h, "POST", "/api/v1/roles/analyst/grants", `{"user_id":"`+lena.id+`"}`), 201, "granting analyst")
	w = lena.do(h, "QUERY", "/api/v1/query", `{"sql":"SELECT count(*) AS n FROM test_people"}`)
	expect(t, w, 200, "an analyst's query")
	if r := decodeBody[result](t, w); r.Rows[0][0].(float64) < 3 {
		t.Fatalf("analyst saw %v people", r.Rows[0][0])
	}

	type problem struct {
		Error        string
		Line, Column int
	}
	w = admin.do(h, "QUERY", "/api/v1/query", `{"sql":"SELECT name,\n  set_config('app.permissions', '', true)\nFROM test_people"}`)
	expect(t, w, 422, "a forbidden function")
	if p := decodeBody[problem](t, w); p.Line != 2 || p.Column != 3 || p.Error == "" {
		t.Fatalf("problem: %+v", p)
	}
	w = admin.do(h, "QUERY", "/api/v1/query", `{"sql":"SELECT nosuch FROM test_people"}`)
	expect(t, w, 422, "an unknown column")
	if p := decodeBody[problem](t, w); p.Line != 1 || p.Column != 8 {
		t.Fatalf("problem: %+v", p)
	}

	schema := decodeBody[struct {
		Views []struct {
			Name    string
			Columns []struct{ Name, Type string }
		}
	}](t, admin.do(h, "GET", "/api/v1/query/schema", ""))
	found := false
	for _, v := range schema.Views {
		found = found || (v.Name == "test_people" && len(v.Columns) == 1 && v.Columns[0].Name == "name")
	}
	if !found {
		t.Fatalf("schema: %+v", schema)
	}

	runs := decodeBody[struct {
		Items []struct{ SQL, Outcome string }
	}](t, admin.do(h, "GET", "/api/v1/query/runs", ""))
	if len(runs.Items) != 4 || runs.Items[0].Outcome != "failed" || runs.Items[1].Outcome != "refused" || runs.Items[2].Outcome != "ok" || runs.Items[3].Outcome != "ok" {
		t.Fatalf("admin's runs: %+v", runs.Items)
	}
	if mine := decodeBody[struct{ Items []any }](t, lena.do(h, "GET", "/api/v1/query/runs", "")); len(mine.Items) != 1 {
		t.Fatalf("lena's runs: %+v", mine.Items)
	}
}

// The shipped views over what people did through the API: an analyst (core-itsm's role) sees every record and its
// public comments, never the internal notes (they do not hold comments:internal), and references come out as the
// API writes them.
func TestQueryViewsMatchTheAPI(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	ann := newActor(t, h, pool, "ann@x.test", "end_user")
	expect(t, admin.do(h, "POST", "/api/v1/roles/analyst/grants", `{"user_id":"`+ann.id+`"}`), 201, "granting analyst")

	w := lena.do(h, "POST", "/api/v1/tickets", `{"title":"Lena's laptop"}`)
	expect(t, w, 201, "filing")
	tk := decodeBody[struct{ ID, Reference string }](t, w)
	expect(t, agent.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/comments", `{"body":"On it"}`), 201, "a comment")
	expect(t, agent.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/comments", `{"body":"Lena again","internal":true}`), 201, "a note")

	type result struct{ Rows [][]any }
	run := func(who actor, sql string) [][]any {
		t.Helper()
		w := who.do(h, "QUERY", "/api/v1/query", `{"sql":"`+sql+`","params":["`+tk.ID+`"]}`)
		expect(t, w, 200, sql)
		return decodeBody[result](t, w).Rows
	}
	if rows := run(ann, "SELECT reference, title FROM records WHERE id = $1::uuid"); len(rows) != 1 || rows[0][0] != tk.Reference || rows[0][1] != "Lena's laptop" {
		t.Fatalf("analyst's record: %v (API reference %s)", rows, tk.Reference)
	}
	if rows := run(ann, "SELECT body FROM comments WHERE record_id = $1::uuid"); len(rows) != 1 || rows[0][0] != "On it" {
		t.Fatalf("analyst's comments: %v", rows)
	}
	if rows := run(admin, "SELECT body FROM comments WHERE record_id = $1::uuid ORDER BY created_at"); len(rows) != 2 {
		t.Fatalf("admin's comments: %v", rows)
	}
	// The analyst holds users:read, so the directory: everyone but app principals.
	if rows := run(ann, "SELECT count(*) FROM people WHERE id <> $1::uuid"); rows[0][0].(float64) < 4 {
		t.Fatalf("analyst's people: %v", rows)
	}

	schema := decodeBody[struct {
		Views []struct{ Name, Description string }
	}](t, ann.do(h, "GET", "/api/v1/query/schema", ""))
	names := map[string]bool{}
	for _, v := range schema.Views {
		names[v.Name] = v.Description != ""
	}
	for _, want := range []string{"records", "incidents", "changes", "comments", "slas", "people", "groups", "cis", "articles", "history"} {
		if !names[want] {
			t.Errorf("schema has no described view %s: %v", want, names)
		}
	}
}

// A query is a read: it stops when whoever sent it goes away (a closed tab, a cancelled request), by QUERY and by its
// POST alias alike, instead of running on to its 30 seconds and holding one of the person's two places. Changes are
// the opposite (TestAChangeFinishesWhenTheClientGoesAway).
func TestAQueryStopsWhenTheClientGoesAway(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	for _, method := range []string{"QUERY", "POST"} {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequestWithContext(ctx, method, "/api/v1/query",
			strings.NewReader(`{"sql":"SELECT count(*) FROM generate_series(1, 5000000000)"}`))
		req.Host = "sl.test"
		for _, c := range admin.cookies {
			req.AddCookie(c)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.ServeHTTP(httptest.NewRecorder(), req)
		}()
		time.Sleep(300 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the query was still running 5 s after its client went away", method)
		}
		var outcome, message string
		if err := pool.QueryRow(context.Background(), `SELECT outcome, error FROM query_runs ORDER BY started_at DESC LIMIT 1`).
			Scan(&outcome, &message); err != nil {
			t.Fatal(err)
		}
		if outcome != "failed" || !strings.Contains(message, "went away") {
			t.Errorf("%s: recorded %s %q", method, outcome, message)
		}
	}
	// And the method really is QUERY on the wire: an unknown method is a 405, not routed.
	if w := admin.do(h, "SEARCH", "/api/v1/query", `{"sql":"SELECT 1"}`); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("SEARCH: %d", w.Code)
	}
}
