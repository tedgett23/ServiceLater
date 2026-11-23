package query_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/query"
)

// save checks a view as its owner and stores it the way the API does.
func (w *viewWorld) save(t *testing.T, owner uuid.UUID, name, sql string, shared bool) error {
	t.Helper()
	c, err := w.r.CheckView(context.Background(), query.ViewRequest{User: owner, Name: name, SQL: sql, Shared: shared})
	if err != nil {
		return err
	}
	cols, _ := json.Marshal(c.Columns)
	uses := c.Uses
	if uses == nil {
		uses = []string{}
	}
	if _, err := w.pool.Exec(context.Background(), `INSERT INTO saved_views (name, sql, shared, columns, depends_on, owner_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (name) DO UPDATE SET sql = EXCLUDED.sql, shared = EXCLUDED.shared, columns = EXCLUDED.columns, depends_on = EXCLUDED.depends_on`,
		name, c.SQL, shared, cols, uses, owner); err != nil {
		t.Fatal(err)
	}
	return nil
}

func mustRefuse(t *testing.T, err error, want string) {
	t.Helper()
	var refused *query.Refused
	if !errors.As(err, &refused) || !strings.Contains(refused.Message, want) {
		t.Errorf("want a refusal saying %q, got %v", want, err)
	}
}

// A saved view is SQL, not data: whoever reads it sees their own rows, not its author's. Views read views, in the
// right order, and the checks that keep that sane hold.
func TestSavedViewsAreReadAsTheReader(t *testing.T) {
	w := newViewWorld(t)
	ctx := context.Background()
	if err := w.save(t, w.ada, "open_incidents", "SELECT id, title, assignment_group_id FROM records WHERE type = 'incident' AND state_category IN ('open', 'waiting');", true); err != nil {
		t.Fatal(err)
	}
	if err := w.save(t, w.ada, "incident_titles", "SELECT title FROM open_incidents", true); err != nil {
		t.Fatal(err)
	}
	for who, want := range map[uuid.UUID][]string{
		w.ada: {"A grievance", "Ron's printer"}, // she sees the restricted one
		w.ann: {"Ron's printer"},                // the analyst does not
		w.ron: {"Ron's printer"},                // the requester, his own
		w.hal: {"A grievance", "Ron's printer"}, // HR's member
	} {
		if got := w.rows(t, who, "SELECT t.title FROM incident_titles t JOIN open_incidents o USING (title)"); !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", w.nameOf(who), got, want)
		}
	}
	// The columns are what the view gives.
	c, err := w.r.CheckView(ctx, query.ViewRequest{User: w.ada, Name: "probe", SQL: "SELECT * FROM open_incidents"})
	if err != nil || len(c.Columns) != 3 || c.Columns[0].Name != "id" || c.Columns[0].Type != "uuid" || !slices.Equal(c.Uses, []string{"open_incidents"}) {
		t.Fatalf("check: %+v %v", c, err)
	}

	// A private view is its owner's alone.
	if err := w.save(t, w.ada, "ada_only", "SELECT 1 AS one", false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.r.Run(ctx, query.Request{User: w.ann, SQL: "SELECT * FROM ada_only"}); err == nil {
		t.Error("Ann read Ada's private view")
	}
	if got := w.rows(t, w.ada, "SELECT one FROM ada_only"); !slices.Equal(got, []string{"1"}) {
		t.Errorf("Ada's own: %v", got)
	}

	// What may not be saved.
	mustRefuse(t, w.save(t, w.ada, "open_incidents", "SELECT * FROM incident_titles", true), "cannot read itself")
	mustRefuse(t, w.save(t, w.ada, "loop", "SELECT * FROM loop", true), "not something queries can read") // not a view yet
	mustRefuse(t, w.save(t, w.ada, "with_param", "SELECT * FROM records WHERE priority = $1", true), "parameters")
	mustRefuse(t, w.save(t, w.ada, "records", "SELECT 1 AS x", true), "built-in")
	mustRefuse(t, w.save(t, w.ada, "order", "SELECT 1 AS x", true), "keyword")
	mustRefuse(t, w.save(t, w.ada, "Bad Name", "SELECT 1 AS x", true), "lower-case")
	mustRefuse(t, w.save(t, w.ada, "leaks", "SELECT * FROM ada_only", true), "these are private: ada_only")
	mustRefuse(t, w.save(t, w.ada, "writes", "DELETE FROM records", true), "only SELECT")
	if err := w.save(t, w.ada, "reads_private", "SELECT * FROM ada_only", false); err != nil {
		t.Errorf("a private view reading a private view: %v", err)
	}

	// A view that fails as it runs says so by name; a mistake in the query itself still points into the query.
	if _, err := w.pool.Exec(ctx, `INSERT INTO saved_views (name, sql, shared, owner_id) VALUES ('divides', 'SELECT 1 / 0 AS x', true, $1)`, w.ada); err != nil {
		t.Fatal(err)
	}
	_, err = w.r.Run(ctx, query.Request{User: w.ann, SQL: "SELECT x FROM divides"})
	var failed *query.Failed
	if !errors.As(err, &failed) || !strings.Contains(failed.Message, "division by zero") {
		t.Errorf("a view failing: %v", err)
	}
	_, err = w.r.Run(ctx, query.Request{User: w.ann, SQL: "SELECT title\nFROM open_incidents\nWHERE nosuch = 1"})
	if !errors.As(err, &failed) || failed.Line != 3 || failed.Column != 7 {
		t.Errorf("a mistake after a view: %#v", err)
	}
	_, err = w.r.Run(ctx, query.Request{User: w.ann, SQL: "SELECT nosuch FROM open_incidents"})
	if !errors.As(err, &failed) || failed.Line != 1 || failed.Column != 8 {
		t.Errorf("a mistake before a view: %#v", err)
	}

	// The person's own WITH may use a view's name for something else.
	if got := w.rows(t, w.ann, "WITH open_incidents AS (SELECT 'mine' AS title) SELECT title FROM open_incidents"); !slices.Equal(got, []string{"mine"}) {
		t.Errorf("a WITH named like a view: %v", got)
	}
}
