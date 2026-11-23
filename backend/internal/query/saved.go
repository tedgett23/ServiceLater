package query

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	pgquery "github.com/wasilibs/go-pgquery"
)

// Saved views (docs/query.md "Saved views"): a query someone saved under a name, which other queries read like a
// table. Nothing is created in the database: when a query names one, its SQL goes in front of the query as a WITH
// clause, checked again like the rest, so a saved view can never show more than the query views do, and each person
// who reads it sees their own rows, whoever wrote it.

// savedView is a saved view as the runner needs it.
type savedView struct {
	SQL    string
	Shared bool
}

// plan is a query ready to run: the WITH clause of the saved views it reads (and the views they read), in an order
// where each comes after those it uses, then the query itself.
type plan struct {
	checked   *Checked
	views     map[string]bool // the query views, for the final check
	head      string          // "WITH ... SELECT * FROM (\n", the text before the query
	headRunes int
	segments  []segment // where each saved view's SQL is in head, in characters
	order     []string  // the saved views it reads, directly or not
}

type segment struct {
	name       string
	start, end int
}

const plainHead = "SELECT * FROM (\n"

// plan checks sql for someone and works out the saved views it reads. self is the name of a view being saved with
// this SQL, which may not read itself, through other views or directly ("" when it is a plain query).
func (r *Runner) plan(ctx context.Context, user uuid.UUID, sql, self string) (*plan, error) {
	views, err := r.viewNames(ctx)
	if err != nil {
		return nil, err
	}
	saved, err := r.savedViews(ctx, user)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(saved))
	for n := range saved {
		names[n] = true
	}
	checked, err := CheckWith(sql, views, names)
	if err != nil {
		return nil, err
	}
	pl := &plan{checked: checked, views: views}
	state := map[string]int{} // 1 visiting, 2 done
	var path []string
	if self != "" {
		state[self] = 1
		path = []string{self}
	}
	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case 2:
			return nil
		case 1:
			return &Refused{Message: "a view cannot read itself, directly or through other views: " + strings.Join(append(path, name), " → ")}
		}
		sv, ok := saved[name]
		if !ok {
			return &Refused{Message: fmt.Sprintf("there is no view %s that you can read", name)}
		}
		c, err := CheckWith(sv.SQL, views, names)
		if err != nil {
			return &Refused{Message: fmt.Sprintf("the view %s no longer works: %v", name, err)}
		}
		if c.Params > 0 {
			return &Refused{Message: fmt.Sprintf("the view %s takes parameters, which views may not", name)}
		}
		state[name] = 1
		path = append(path, name)
		for _, u := range c.Uses {
			if err := visit(u); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		state[name] = 2
		pl.order = append(pl.order, name)
		saved[name] = savedView{SQL: c.SQL, Shared: sv.Shared}
		return nil
	}
	for _, u := range checked.Uses {
		if err := visit(u); err != nil {
			return nil, err
		}
	}

	if len(pl.order) == 0 {
		pl.head = plainHead
	} else {
		var b strings.Builder
		b.WriteString("WITH ")
		for i, n := range pl.order {
			if i > 0 {
				b.WriteString(",\n")
			}
			b.WriteString(pgx.Identifier{n}.Sanitize() + " AS (\n")
			start := len([]rune(b.String()))
			b.WriteString(saved[n].SQL)
			pl.segments = append(pl.segments, segment{name: n, start: start, end: len([]rune(b.String()))})
			b.WriteString("\n)")
		}
		b.WriteString("\n" + plainHead)
		pl.head = b.String()
	}
	pl.headRunes = len([]rune(pl.head))
	return pl, nil
}

// savedViews is every saved view the person may read: their own and the shared ones. It is read as the system, by the
// same rule as the saved_views read policy; the query views still decide every row.
func (r *Runner) savedViews(ctx context.Context, user uuid.UUID) (map[string]savedView, error) {
	rows, err := r.owner.Query(ctx, `SELECT name, sql, shared FROM saved_views WHERE owner_id = $1 OR shared`, user)
	if err != nil {
		return nil, fmt.Errorf("query: saved views: %w", err)
	}
	defer rows.Close()
	out := map[string]savedView{}
	for rows.Next() {
		var name string
		var sv savedView
		if err := rows.Scan(&name, &sv.SQL, &sv.Shared); err != nil {
			return nil, fmt.Errorf("query: saved views: %w", err)
		}
		out[name] = sv
	}
	return out, rows.Err()
}

// ViewRequest is a view someone wants to save: its name, its SQL, and whether it is shared.
type ViewRequest struct {
	User   uuid.UUID
	Scopes []string
	Name   string
	SQL    string
	Shared bool
}

// ViewCheck is what saving a view needs to know: the columns it gives (from running it with no rows, as the person),
// the saved views it reads directly, and the SQL to store (the statement alone).
type ViewCheck struct {
	SQL     string
	Columns []Column
	Uses    []string
}

var viewName = regexp.MustCompile(`^[a-z][a-z0-9_]{1,62}$`)

// CheckView decides whether a view may be saved as asked, and what it gives: a valid name that no query view has and
// that is not an SQL keyword, one SELECT without parameters that the person may run, that reads no view through
// itself, and, when shared, reads only shared views (or everyone else would find it broken).
func (r *Runner) CheckView(ctx context.Context, req ViewRequest) (*ViewCheck, error) {
	if err := r.checkName(ctx, req.Name); err != nil {
		return nil, err
	}
	pl, err := r.plan(ctx, req.User, req.SQL, req.Name)
	if err != nil {
		return nil, err
	}
	if pl.checked.Params > 0 {
		return nil, &Refused{Message: "a view may not take parameters ($1, $2...): filter it when you read it instead"}
	}
	if req.Shared {
		saved, err := r.savedViews(ctx, req.User)
		if err != nil {
			return nil, err
		}
		var private []string
		for _, n := range pl.order {
			if !saved[n].Shared {
				private = append(private, n)
			}
		}
		if len(private) > 0 {
			return nil, &Refused{Message: "a shared view may read only shared views; these are private: " + strings.Join(private, ", ")}
		}
	}
	res, err := r.execute(ctx, Request{User: req.User, Scopes: req.Scopes}, pl, 0, nil)
	if err != nil {
		return nil, err
	}
	return &ViewCheck{SQL: pl.checked.SQL, Columns: res.Columns, Uses: pl.checked.Uses}, nil
}

func (r *Runner) checkName(ctx context.Context, name string) error {
	if !viewName.MatchString(name) {
		return &Refused{Message: "a view's name is 2 to 63 lower-case letters, digits and underscores, starting with a letter"}
	}
	views, err := r.viewNames(ctx)
	if err != nil {
		return err
	}
	if views[name] {
		return &Refused{Message: fmt.Sprintf("%s is one of the built-in views: choose another name", name)}
	}
	// A keyword (order, user, table...) would have to be quoted every time it is used.
	tree, err := pgquery.Parse("SELECT * FROM " + name)
	if err != nil || len(tree.Stmts) != 1 {
		return &Refused{Message: fmt.Sprintf("%s is an SQL keyword: choose another name", name)}
	}
	from := tree.Stmts[0].Stmt.GetSelectStmt().GetFromClause()
	if len(from) != 1 || from[0].GetRangeVar().GetRelname() != name {
		return &Refused{Message: fmt.Sprintf("%s is an SQL keyword: choose another name", name)}
	}
	return nil
}
