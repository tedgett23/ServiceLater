package api_test

import (
	"strings"
	"testing"
)

// Saved views through the API: saved by one person, shared, read by another as themselves (by query and as rows with
// filters, order and pages), and kept from breaking the views that read them.
func TestSavedViews(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	ann := newActor(t, h, pool, "ann@x.test", "end_user")
	expect(t, admin.do(h, "POST", "/api/v1/roles/analyst/grants", `{"user_id":"`+ann.id+`"}`), 201, "granting analyst")
	for _, title := range []string{"Printer jam", "VPN drops", "Disk full"} {
		expect(t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"`+title+`"}`), 201, "filing "+title)
	}

	expect(t, agent.do(h, "GET", "/api/v1/views", ""), 403, "an agent listing views")
	expect(t, agent.do(h, "PUT", "/api/v1/views/x", `{"sql":"SELECT 1 AS x"}`), 403, "an agent saving one")

	type view struct {
		Name, SQL string
		Shared    bool
		Mine      bool
		Columns   []struct{ Name, Type string }
		Uses      []string
	}
	w := ann.do(h, "PUT", "/api/v1/views/incidents_by_title", `{"sql":"SELECT reference, title, priority FROM records WHERE type = 'incident'","description":"Every incident","shared":true}`)
	expect(t, w, 201, "saving a view")
	v := decodeBody[view](t, w)
	if !v.Mine || !v.Shared || len(v.Columns) != 3 || v.Columns[2].Type != "int2" {
		t.Fatalf("saved: %+v", v)
	}
	expect(t, ann.do(h, "PUT", "/api/v1/views/incidents_by_title", `{"sql":"SELECT reference, title, priority FROM records WHERE type = 'incident' ORDER BY 1","shared":true}`), 200, "changing it")
	w = ann.do(h, "PUT", "/api/v1/views/broken", `{"sql":"SELECT nosuch\nFROM records"}`)
	expect(t, w, 422, "a view that does not run")
	if p := decodeBody[struct{ Line, Column int }](t, w); p.Line != 1 || p.Column != 8 {
		t.Fatalf("where: %+v", p)
	}

	// A chart is kept with it, checked against its columns.
	w = ann.do(h, "PUT", "/api/v1/views/by_priority", `{"sql":"SELECT priority, count(*) AS n FROM records GROUP BY 1","chart":{"type":"bars","x":"priority","y":["n"]}}`)
	expect(t, w, 201, "a view with a chart")
	if c := decodeBody[struct {
		Chart struct {
			Type, X string
			Y       []string
		}
	}](t, w).Chart; c.Type != "bars" || c.X != "priority" || len(c.Y) != 1 {
		t.Fatalf("chart: %+v", c)
	}
	expect(t, ann.do(h, "PUT", "/api/v1/views/by_priority", `{"sql":"SELECT priority, count(*) AS n FROM records GROUP BY 1","chart":{"type":"bars","x":"priority","y":["title"]}}`), 422, "a chart of a column it does not have")
	expect(t, ann.do(h, "DELETE", "/api/v1/views/by_priority", ""), 204, "removing it")

	// Another person: reads it as themselves, cannot change it, cannot take its name.
	expect(t, admin.do(h, "PUT", "/api/v1/views/incidents_by_title", `{"sql":"SELECT 1 AS x"}`), 403, "someone else's view")
	expect(t, ann.do(h, "PUT", "/api/v1/views/admins_private", `{"sql":"SELECT 1 AS x"}`), 201, "ann's private view")
	expect(t, admin.do(h, "PUT", "/api/v1/views/admins_private", `{"sql":"SELECT 2 AS x"}`), 409, "a name someone else has, privately")
	expect(t, admin.do(h, "GET", "/api/v1/views/admins_private", ""), 404, "someone else's private view")

	type rows struct {
		Rows          [][]any
		Truncated     bool
		Limit, Offset int
	}
	w = admin.do(h, "GET", "/api/v1/views/incidents_by_title/rows?order=-title&limit=2", "")
	expect(t, w, 200, "reading rows")
	if r := decodeBody[rows](t, w); len(r.Rows) != 2 || r.Rows[0][1] != "VPN drops" || !r.Truncated || r.Limit != 2 {
		t.Fatalf("page 1: %+v", r)
	}
	if r := decodeBody[rows](t, admin.do(h, "GET", "/api/v1/views/incidents_by_title/rows?order=-title&limit=2&offset=2", "")); len(r.Rows) != 1 || r.Truncated {
		t.Fatalf("page 2: %+v", r)
	}
	if r := decodeBody[rows](t, admin.do(h, "GET", "/api/v1/views/incidents_by_title/rows?title=Disk+full", "")); len(r.Rows) != 1 {
		t.Fatalf("filtered: %+v", r)
	}
	if r := decodeBody[rows](t, admin.do(h, "GET", "/api/v1/views/incidents_by_title/rows?priority=3", "")); len(r.Rows) != 3 {
		t.Fatalf("filtered by a number: %+v", r)
	}
	expect(t, admin.do(h, "GET", "/api/v1/views/incidents_by_title/rows?nosuch=1", ""), 400, "a filter that is not a column")
	expect(t, admin.do(h, "GET", "/api/v1/views/incidents_by_title/rows?order=nosuch", ""), 400, "an order that is not a column")
	// And by name in a query.
	w = admin.do(h, "QUERY", "/api/v1/query", `{"sql":"SELECT count(*) FROM incidents_by_title"}`)
	expect(t, w, 200, "a query reading it")
	if r := decodeBody[rows](t, w); r.Rows[0][0].(float64) != 3 {
		t.Fatalf("count: %+v", r)
	}

	// A view others read: its columns stay, it stays shared, it stays.
	expect(t, admin.do(h, "PUT", "/api/v1/views/titles", `{"sql":"SELECT title FROM incidents_by_title","shared":true}`), 201, "a view reading it")
	w = ann.do(h, "PUT", "/api/v1/views/incidents_by_title", `{"sql":"SELECT reference FROM records","shared":true}`)
	expect(t, w, 409, "changing its columns")
	if !strings.Contains(w.Body.String(), "titles") {
		t.Fatalf("names the view: %s", w.Body)
	}
	expect(t, ann.do(h, "PUT", "/api/v1/views/incidents_by_title", `{"sql":"SELECT reference, title, priority FROM records WHERE type = 'incident'","shared":false}`), 409, "making it private")
	expect(t, ann.do(h, "DELETE", "/api/v1/views/incidents_by_title", ""), 409, "removing it")

	// The schema lists the saved views beside the built-in ones.
	schema := decodeBody[struct {
		SavedViews []view `json:"saved_views"`
	}](t, admin.do(h, "GET", "/api/v1/query/schema", ""))
	if len(schema.SavedViews) != 2 || schema.SavedViews[0].Name != "incidents_by_title" || schema.SavedViews[1].Name != "titles" {
		t.Fatalf("schema: %+v", schema.SavedViews)
	}

	// Removing: the owner, in order; an admin (settings:manage) may remove anyone's.
	expect(t, ann.do(h, "DELETE", "/api/v1/views/titles", ""), 403, "someone else's view")
	expect(t, admin.do(h, "DELETE", "/api/v1/views/titles", ""), 204, "removing her own")
	expect(t, admin.do(h, "DELETE", "/api/v1/views/incidents_by_title", ""), 204, "an admin removing anyone's")
	expect(t, admin.do(h, "GET", "/api/v1/views/incidents_by_title/rows", ""), 404, "rows of a view that is gone")
}

// QUERY /views/{name}/rows: the whole filter language against real records, as the reader.
func TestReadingAViewWithAFilter(t *testing.T) {
	h, pool := setup(t)
	admin := newActor(t, h, pool, "admin@x.test", "admin")
	for _, tk := range []string{
		`{"title":"VPN drops at night","priority":1}`,
		`{"title":"Printer jam","priority":3}`,
		`{"title":"vpn client will not install","priority":2}`,
		`{"title":"Disk full","priority":4}`,
	} {
		expect(t, admin.do(h, "POST", "/api/v1/tickets", tk), 201, "filing")
	}
	expect(t, admin.do(h, "PUT", "/api/v1/views/work", `{"sql":"SELECT reference, title, priority, assignee_id, fields, created_at FROM records WHERE type = 'incident'"}`), 201, "saving")

	type rows struct {
		Columns   []struct{ Name string }
		Rows      [][]any
		Truncated bool
	}
	read := func(body string) rows {
		t.Helper()
		w := admin.do(h, "QUERY", "/api/v1/views/work/rows", body)
		expect(t, w, 200, body)
		return decodeBody[rows](t, w)
	}
	titles := func(r rows) []string {
		var out []string
		for _, row := range r.Rows {
			out = append(out, row[0].(string))
		}
		return out
	}

	r := read(`{"where": {"and": [{"title": {"ilike": "%vpn%"}}, {"priority": {"lte": 2}}]}, "order": [{"priority": "desc"}], "columns": ["title"]}`)
	if got := titles(r); len(got) != 2 || got[0] != "vpn client will not install" || len(r.Columns) != 1 {
		t.Fatalf("and + ilike + lte: %v %+v", got, r.Columns)
	}
	r = read(`{"where": {"or": [{"priority": {"in": [1, 4]}}, {"title": {"eq": "Printer jam"}}]}, "order": [{"priority": "asc"}], "columns": ["title"]}`)
	if got := titles(r); len(got) != 3 || got[0] != "VPN drops at night" || got[2] != "Disk full" {
		t.Fatalf("or + in: %v", got)
	}
	r = read(`{"where": {"not": {"priority": {"between": ["2", "3"]}}}, "columns": ["title"], "order": [{"title": "asc"}]}`)
	if got := titles(r); len(got) != 2 {
		t.Fatalf("not between: %v", got)
	}
	r = read(`{"where": {"assignee_id": {"is_null": true}}, "limit": 3, "columns": ["title"]}`)
	if len(r.Rows) != 3 || !r.Truncated {
		t.Fatalf("is_null + limit: %+v", r)
	}
	r = read(`{"where": {"fields": {"contains": {}}}, "columns": ["title"]}`)
	if len(r.Rows) != 4 {
		t.Fatalf("contains: %+v", r)
	}

	w := admin.do(h, "QUERY", "/api/v1/views/work/rows", `{"where": {"and": [{"title": {"eq": "x"}}, {"priority": {"like": "1%"}}]}}`)
	expect(t, w, 422, "a filter that cannot be read")
	if !strings.Contains(w.Body.String(), "where.and[1].priority.like") {
		t.Fatalf("says where: %s", w.Body)
	}
	expect(t, admin.do(h, "QUERY", "/api/v1/views/work/rows", `{"where": {"priority": {"eq": "high"}}}`), 422, "a value of the wrong type")
	expect(t, admin.do(h, "QUERY", "/api/v1/views/work/rows", `{"order": [{"title": "sideways"}]}`), 422, "an order that is not asc or desc")
	expect(t, admin.do(h, "QUERY", "/api/v1/views/work/rows", `{"limit": 6000}`), 422, "too many rows")
	expect(t, admin.do(h, "QUERY", "/api/v1/views/nosuch/rows", `{}`), 404, "a view that does not exist")
}
