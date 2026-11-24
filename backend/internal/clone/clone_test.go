package clone

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/dbtest"
	"github.com/servicelater/servicelater/backend/internal/expr"
)

// A new table must be given a clone policy (or be listed as never copied): otherwise the export refuses to run,
// rather than leak it or leave it behind by accident.
func TestEveryTableHasAClonePolicy(t *testing.T) {
	pool := dbtest.Pool(t)
	rows, err := pool.Query(context.Background(), `SELECT relname FROM pg_class
		WHERE relnamespace = 'public'::regnamespace AND relkind = 'r' ORDER BY relname`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range tables {
		_, has := policies[name]
		if has == slices.Contains(never, name) {
			t.Errorf("%s: give it a policy in internal/clone/plan.go or list it in never (exactly one)", name)
		}
	}
	for name := range policies {
		if !slices.Contains(tables, name) {
			t.Errorf("the policy for %s names no table", name)
		}
	}
	for _, name := range never {
		if !slices.Contains(tables, name) {
			t.Errorf("never lists %s, which is no table", name)
		}
	}
}

type world struct {
	pat, mo, sam, una         string // an agent, their manager, a requester of a closed record, someone with none
	open, closed, problem, p1 string
}

func seed(t *testing.T, pool *pgxpool.Pool) world {
	t.Helper()
	ctx := context.Background()
	var w world
	for _, u := range []struct {
		id                *string
		email, name, role string
	}{{&w.mo, "mo@x.test", "Mo", "agent"}, {&w.pat, "pat@x.test", "Pat", "agent"}, {&w.sam, "sam@x.test", "Sam", "end_user"},
		{&w.una, "una@x.test", "Una", "end_user"}} {
		if err := pool.QueryRow(ctx, `INSERT INTO users (email, name, role) VALUES ($1, $2, $3) RETURNING id`, u.email, u.name, u.role).Scan(u.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET manager_id = $1 WHERE id = $2`, w.mo, w.pat); err != nil {
		t.Fatal(err)
	}
	file := func(id *string, typ, title, category string, priority int, requester string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `INSERT INTO tasks (type, title, description, status, state_category, priority, requester_id, assignee_id)
			VALUES ($1, $2, 'The details', $3, $3, $4, $5, $6) RETURNING id`, typ, title, category, priority, requester, w.pat).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	file(&w.problem, "problem", "Disks fill up", "closed", 3, w.pat)
	file(&w.open, "incident", "Disk full", "open", 3, w.pat)
	file(&w.closed, "incident", "Printer jam", "closed", 3, w.sam)
	file(&w.p1, "incident", "Everything is down", "open", 1, w.pat)
	if _, err := pool.Exec(ctx, `INSERT INTO incidents (task_id, type, problem_id) VALUES ($1, 'incident', $2)
		ON CONFLICT (task_id) DO UPDATE SET problem_id = EXCLUDED.problem_id`, w.open, w.problem); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		task, author, body string
		internal           bool
	}{{w.open, w.pat, "Looking", false}, {w.open, w.pat, "Between us", true}, {w.closed, w.sam, "Thanks", false}} {
		if _, err := pool.Exec(ctx, `INSERT INTO task_comments (task_id, author_id, body, internal) VALUES ($1, $2, $3, $4)`,
			c.task, c.author, c.body, c.internal); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func rule(recordType, when string) (*expr.Program, error) {
	env, err := expr.Rule()
	if err != nil {
		return nil, err
	}
	return env.Compile(when)
}

func strings1(t *testing.T, tx pgx.Tx, q string) []string {
	t.Helper()
	rows, err := tx.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var s *string
		err := r.Scan(&s)
		if s == nil {
			return "<null>", err
		}
		return *s, err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// A profile taking open records only, without internal notes, people referenced and masked: the closed record and
// its requester stay behind, the open record's link to a closed problem is cleared and reported, a kept agent's
// manager comes along, and names, addresses and text are replaced.
func TestAProfileChoosesRecordsPeopleAndMasking(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	w := seed(t, pool)
	p := DefaultProfile
	p.Records, p.InternalNotes, p.People = RecordsOpen, false, PeopleReferenced
	p.Mask = Mask{Names: true, Emails: true, Text: true}
	p.Exclude = []Exclusion{{RecordType: "incident", When: "record.priority == 1"}}
	o := Options{Profile: p, Exclusion: rule}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rep, err := prepare(ctx, tx, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings1(t, tx, "SELECT id::text FROM clone_tasks"); !slices.Equal(got, []string{w.open}) {
		t.Errorf("kept records %v, want only the open incident %s", got, w.open)
	}
	if rep.Excluded != 1 {
		t.Errorf("excluded %d, want the P1", rep.Excluded)
	}
	if rep.ClearedTotal != 1 || len(rep.Cleared) != 1 || rep.Cleared[0].Link != "problem" || !strings.HasPrefix(rep.Cleared[0].To, "PRB") {
		t.Errorf("cleared %+v (%d), want the incident's problem", rep.Cleared, rep.ClearedTotal)
	}
	users := strings1(t, tx, "SELECT id::text FROM clone_users")
	for who, want := range map[string]bool{w.pat: true, w.mo: true, w.sam: false, w.una: false} {
		if slices.Contains(users, who) != want {
			t.Errorf("person %s kept = %v, want %v", who, !want, want)
		}
	}
	query := func(table string, columns ...string) []string {
		q, ok := selectFor(table, columns, p)
		if !ok {
			t.Fatalf("no query for %s", table)
		}
		outer := make([]string, len(columns))
		for i, c := range columns {
			outer[i] = "coalesce(x." + c + "::text, 'none')"
		}
		return strings1(t, tx, "SELECT concat_ws('|', "+strings.Join(outer, ", ")+") FROM ("+q+") x")
	}
	if got := query("users", "email", "name"); len(got) != 2 || strings.Contains(strings.Join(got, " "), "x.test") ||
		strings.Contains(strings.Join(got, " "), "Pat") || !strings.HasPrefix(got[0], "user-") {
		t.Errorf("people %v, want Pat and Mo masked", got)
	}
	if got := query("task_comments", "body"); !slices.Equal(got, []string{"Masked text."}) {
		t.Errorf("comments %v, want the public one, masked", got)
	}
	if got := query("tasks", "description", "parent_id"); !slices.Equal(got, []string{"Masked text.|none"}) {
		t.Errorf("tasks %v", got)
	}
	if got := query("incidents", "task_id", "problem_id"); !slices.Equal(got, []string{w.open + "|none"}) {
		t.Errorf("incidents %v, want the open one with its problem cleared", got)
	}
	if _, ok := selectFor("settings", []string{"name"}, p); ok {
		t.Errorf("settings are the target's own")
	}
}

// A package opens only with its passphrase, holds the rows its profile chose and never a table the target keeps,
// and making one changes nothing.
func TestAPackageOpensWithItsPassphrase(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	seed(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO settings (name, value) VALUES ('email.host', '"smtp.example.com"')`); err != nil {
		t.Fatal(err)
	}
	p := DefaultProfile
	p.Records, p.Mask.Emails = RecordsOpen, true
	if _, err := Export(ctx, pool, &bytes.Buffer{}, Options{Profile: p, Passphrase: "short"}); err == nil {
		t.Fatal("a short passphrase was taken")
	}
	var buf bytes.Buffer
	sum, err := Export(ctx, pool, &buf, Options{Profile: p, ProfileName: "uat", Passphrase: "correct horse battery", Instance: "Prod"})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Rows["tasks"] != 2 || sum.Rows["task_comments"] != 2 {
		t.Errorf("rows %v, want the two open records and their comments", sum.Rows)
	}
	for _, name := range never {
		if _, ok := sum.Rows[name]; ok {
			t.Errorf("the package holds %s", name)
		}
	}
	if _, err := Verify(bytes.NewReader(buf.Bytes()), "wrong horse battery"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	got, err := Verify(bytes.NewReader(buf.Bytes()), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != "uat" || got.Instance != "Prod" || got.Rows["tasks"] != 2 || got.Report.People == 0 || got.IdentitySalt == "" {
		t.Errorf("read back %+v", got)
	}
	cut := buf.Bytes()[:buf.Len()-200]
	if _, err := Verify(bytes.NewReader(cut), "correct horse battery"); err == nil {
		t.Error("a package cut short verified")
	}
	var temp int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_class WHERE relname LIKE 'clone_tasks%'").Scan(&temp); err != nil || temp != 0 {
		t.Errorf("temporary tables left behind: %d %v", temp, err)
	}
	if HashEmail("a", "pat@x.test") != HashEmail("a", "PAT@x.test ") || HashEmail("a", "pat@x.test") == HashEmail("b", "pat@x.test") {
		t.Error("addresses hash by their lowercased form")
	}
}
