package query_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/query"
)

var cols = []query.Column{{Name: "reference", Type: "text"}, {Name: "title", Type: "text"}, {Name: "priority", Type: "int2"},
	{Name: "assignee_id", Type: "uuid"}, {Name: "fields", Type: "jsonb"}, {Name: "created_at", Type: "timestamptz"}}

// A filter becomes a WHERE with quoted columns and every value a parameter, exactly as written (numbers too).
func TestAFilterBecomesSQL(t *testing.T) {
	sql, params, err := query.ReadSQL("open_work", cols, query.Read{
		Where: []byte(`{"and": [
			{"priority": {"lte": 2, "gt": 0}},
			{"title": {"ilike": "%vpn%"}},
			{"or": [{"assignee_id": {"is_null": true}}, {"reference": {"in": ["INC1", "INC2"]}}]},
			{"not": {"fields": {"contains": {"category": "network"}}}},
			{"created_at": {"between": ["2026-01-01", "2026-12-31"]}}
		]}`),
		Equal:   []query.Condition{{Column: "reference", Value: "INC9"}},
		Order:   []query.OrderBy{{Column: "priority"}, {Column: "created_at", Desc: true}},
		Columns: []string{"reference", "title"},
		Limit:   50, Offset: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{
		`SELECT q."reference", q."title" FROM "open_work" q`,
		`q."reference" = $1`,
		`(q."priority" > $2 AND q."priority" <= $3)`,
		`q."title" ILIKE $4`,
		`(q."assignee_id" IS NULL OR q."reference" IN ($5, $6))`,
		`NOT q."fields" @> $7::jsonb`,
		`q."created_at" BETWEEN $8 AND $9`,
		`ORDER BY q."priority", q."created_at" DESC`,
		`LIMIT 51 OFFSET 100`,
	} {
		if !strings.Contains(sql, part) {
			t.Errorf("missing %s in\n%s", part, sql)
		}
	}
	if !slices.Equal(params, []string{"INC9", "0", "2", "%vpn%", "INC1", "INC2", `{"category":"network"}`, "2026-01-01", "2026-12-31"}) {
		t.Errorf("params %q", params)
	}
}

func TestAFilterSaysWhereItIsWrong(t *testing.T) {
	for filter, want := range map[string]string{
		`{"nosuch": {"eq": 1}}`: "where.nosuch: nosuch is not a column",
		`{"and": [{"title": {"eq": 1}}, {"priority": {"like": "1%"}}]}`: "where.and[1].priority.like: like compares text, and priority is int2",
		`{"title": {"approx": "x"}}`:                                    "where.title.approx: not an operator",
		`{"title": {"eq": null}}`:                                       `use {"is_null": true}`,
		`{"title": {"in": []}}`:                                         "where.title.in: give a list of values",
		`{"title": {"between": [1]}}`:                                   "two values",
		`{"priority": {"contains": 1}}`:                                 "contains looks inside JSON",
		`{"title": {"eq": "a"}, "priority": {"eq": 1}}`:                 `give one of "and", "or", "not" or a column`,
		`{"and": []}`:                                                   "where.and: give a list of conditions",
		`[1]`:                                                           "where: give an object",
		strings.Repeat(`{"not": `, 12) + `{"title": {"eq": "x"}}` + strings.Repeat("}", 12): "nest at most 10",
	} {
		_, _, err := query.ReadSQL("v", cols, query.Read{Where: []byte(filter), Limit: 10})
		var fe *query.FilterError
		if !errors.As(err, &fe) || !strings.Contains(fe.Error(), want) {
			t.Errorf("%s: got %v, want %q", filter, err, want)
		}
	}
	if _, _, err := query.ReadSQL("v", cols, query.Read{Columns: []string{"title", "nosuch"}, Limit: 10}); err == nil || !strings.Contains(err.Error(), "columns[1]") {
		t.Errorf("columns: %v", err)
	}
	if _, _, err := query.ReadSQL("v", cols, query.Read{Order: []query.OrderBy{{Column: "nosuch"}}, Limit: 10}); err == nil || !strings.Contains(err.Error(), "order[0]") {
		t.Errorf("order: %v", err)
	}
}
