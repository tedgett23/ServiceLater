package query_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/dbtest"
	"github.com/servicelater/servicelater/backend/internal/query"
)

// A view in the style of the real ones: rows filtered by the checked identity, never by session settings.
const testView = `CREATE VIEW query.test_people WITH (security_barrier) AS
	SELECT u.name, u.role FROM app_query_identity() me, users u
	WHERE u.id = me.user_id OR position(',users:read,' IN me.perms) > 0`

func setup(t *testing.T) (*pgxpool.Pool, *query.Runner, uuid.UUID, uuid.UUID) {
	t.Helper()
	pool := dbtest.Pool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "TRUNCATE query_runs; DROP VIEW IF EXISTS query.test_people; "+testView); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP VIEW IF EXISTS query.test_people") })
	person := func(name, role string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO users (email, name, role) VALUES ($1, $2, $3) RETURNING id`,
			strings.ToLower(name)+"@example.com", name, role).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	admin := person("Ada", "admin")
	agent := person("Bob", "agent")
	r := query.NewRunner(pool)
	t.Cleanup(r.Close)
	return pool, r, admin, agent
}

func TestAQueryRunsAsThePerson(t *testing.T) {
	pool, r, admin, agent := setup(t)
	ctx := context.Background()

	res, err := r.Run(ctx, query.Request{User: admin, SQL: "SELECT name, role FROM test_people ORDER BY name"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 || res.Rows[0][0] != "Ada" || res.Columns[0].Name != "name" || res.Columns[0].Type != "text" {
		t.Fatalf("got %+v", res)
	}

	// Narrowed by an API key to what does not include the directory: they see only themselves.
	res, err = r.Run(ctx, query.Request{User: admin, Scopes: []string{"query:run"}, SQL: "SELECT name FROM test_people"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != "Ada" {
		t.Fatalf("narrowed: got %+v", res.Rows)
	}

	// An agent does not hold query:run: the views refuse them even if the server asked.
	_, err = r.Run(ctx, query.Request{User: agent, SQL: "SELECT name FROM test_people"})
	var refused *query.Refused
	if !errors.As(err, &refused) || !strings.Contains(refused.Message, "may not run queries") {
		t.Fatalf("agent: got %v", err)
	}
	// Nor does a key narrowed away from it.
	_, err = r.Run(ctx, query.Request{User: admin, Scopes: []string{"users:read"}, SQL: "SELECT name FROM test_people"})
	if !errors.As(err, &refused) {
		t.Fatalf("narrowed away: got %v", err)
	}

	// Every attempt is recorded, refusals included.
	var ok, refusals int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE outcome = 'ok'), count(*) FILTER (WHERE outcome = 'refused') FROM query_runs`).
		Scan(&ok, &refusals); err != nil {
		t.Fatal(err)
	}
	if ok != 2 || refusals != 2 {
		t.Fatalf("audit: %d ok, %d refused", ok, refusals)
	}
}

func TestAQueryGivesTypedValuesParametersAndPlaces(t *testing.T) {
	_, r, admin, _ := setup(t)
	ctx := context.Background()
	res, err := r.Run(ctx, query.Request{User: admin,
		SQL:    "SELECT $1::int + 1 AS n, 2.50::numeric AS d, timestamptz '2026-01-02 03:04:05Z' AS t, '{\"a\": 1}'::jsonb AS j, interval '1 day' AS i, NULL AS z",
		Params: []string{"41"}})
	if err != nil {
		t.Fatal(err)
	}
	row := res.Rows[0]
	if row[0] != int32(42) || row[2] != "2026-01-02T03:04:05Z" || row[5] != nil {
		t.Fatalf("got %#v", row)
	}
	if res.Columns[2].Type != "timestamptz" || res.Columns[3].Type != "jsonb" {
		t.Fatalf("columns %+v", res.Columns)
	}

	_, err = r.Run(ctx, query.Request{User: admin, SQL: "SELECT name\nFROM test_people\nWHERE nosuch = 1"})
	var failed *query.Failed
	if !errors.As(err, &failed) || failed.Line != 3 || failed.Column != 7 {
		t.Fatalf("got %#v", err)
	}
	_, err = r.Run(ctx, query.Request{User: admin, SQL: "SELECT $1::int"})
	var refused *query.Refused
	if !errors.As(err, &refused) {
		t.Fatalf("missing parameter: got %v", err)
	}
}

func TestAQueryStopsAtItsLimits(t *testing.T) {
	pool, r, admin, _ := setup(t)
	ctx := context.Background()
	r.MaxRows = 3
	res, err := r.Run(ctx, query.Request{User: admin, SQL: "SELECT x FROM generate_series(1, 10) x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 3 || !res.Truncated {
		t.Fatalf("got %d rows, truncated %v", len(res.Rows), res.Truncated)
	}

	r.Timeout = 300 * time.Millisecond
	started := time.Now()
	_, err = r.Run(ctx, query.Request{User: admin, SQL: "SELECT count(*) FROM generate_series(1, 1000000000)"})
	var failed *query.Failed
	if !errors.As(err, &failed) || !failed.Timeout {
		t.Fatalf("got %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatalf("stopped only after %s", time.Since(started))
	}

	// The hourly limit is a setting (Admin → Rate limits).
	if _, err := pool.Exec(ctx, `INSERT INTO settings (name, value) VALUES ('limits.queries_per_hour', '5')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM settings WHERE name LIKE 'limits.%'`) })
	if _, err := pool.Exec(ctx, `INSERT INTO query_runs (user_id, sql, outcome) SELECT $1, 'SELECT 1', 'ok' FROM generate_series(1, 5)`, admin); err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(ctx, query.Request{User: admin, SQL: "SELECT 1"})
	var busy *query.Busy
	if !errors.As(err, &busy) || !strings.Contains(busy.Message, "5 queries") {
		t.Fatalf("over the hourly limit: got %v", err)
	}
}

// The layers under the check hold on their own: SQL sent straight to the query role, as if the check had a hole.
func TestTheQueryRoleHoldsWithoutTheCheck(t *testing.T) {
	pool, r, admin, agent := setup(t)
	ctx := context.Background()
	if _, err := r.Run(ctx, query.Request{User: admin, SQL: "SELECT 1"}); err != nil { // opens the role's pool
		t.Fatal(err)
	}
	var key []byte
	var role string
	if err := pool.QueryRow(ctx, `SELECT key, app_query_role() FROM query_keys`).Scan(&key, &role); err != nil {
		t.Fatal(err)
	}
	raw := query.RolePool(r)

	// as runs sql in a read-only transaction carrying token, the way the runner does, and gives the error.
	// Statements separated by "; " run one after another in the transaction; the last one's rows are returned.
	as := func(token, sql string) ([][]any, error) {
		var out [][]any
		err := pgx.BeginTxFunc(ctx, raw, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT set_config('query.token', $1, true)", token); err != nil {
				return err
			}
			stmts := strings.Split(sql, "; ")
			for _, s := range stmts[:len(stmts)-1] {
				if _, err := tx.Exec(ctx, s); err != nil {
					return err
				}
			}
			rows, err := tx.Query(ctx, stmts[len(stmts)-1])
			if err != nil {
				return err
			}
			out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) ([]any, error) { return r.Values() })
			return err
		})
		return out, err
	}
	agentToken := query.Sign(key, agent, time.Now().Add(time.Minute), nil)
	adminToken := query.Sign(key, admin, time.Now().Add(time.Minute), nil)

	for name, sql := range map[string]string{ //nolint:gosec // SQL that names credentials tables, not credentials
		"a table":                      "SELECT * FROM public.users",
		"the signing key":              "SELECT * FROM public.query_keys",
		"a public function":            "SELECT public.app_permissions_of('" + admin.String() + "')",
		"the identity function's key":  "SELECT key FROM query_keys",
		"credentials after RESET ROLE": "RESET ROLE; SELECT * FROM public.user_credentials",
		"the owner role":               "SET ROLE servicelater",
		"the app role":                 "SET ROLE servicelater_app",
		"a write":                      "CREATE TEMP TABLE x (a int)",
	} {
		_, err := as(adminToken, sql)
		if err == nil {
			t.Errorf("%s: %s was allowed", name, sql)
			continue
		}
		t.Logf("%s: %v", name, err)
	}

	// Setting the permissions RLS trusts changes nothing for the views: the agent still may not query.
	_, err := as(agentToken, "SELECT set_config('app.permissions', ',query:run,users:read,', true), set_config('app.current_user_id', '"+admin.String()+"', true); SET app.hidden_groups = ','; SELECT name FROM test_people")
	if err == nil || !strings.Contains(err.Error(), "may not run queries") {
		t.Errorf("an agent with forged settings: %v", err)
	}
	// And the role does not become anyone else's on the way back.
	if rows, err := as(adminToken, "RESET ROLE; SELECT current_user::text"); err != nil || rows[0][0] != role {
		t.Errorf("after RESET ROLE: %v %v", rows, err)
	}
	if _, err := as(agentToken, "SELECT name FROM test_people WHERE set_config('app.permissions', ',query:run,users:read,', true) <> ''"); err == nil || !strings.Contains(err.Error(), "may not run queries") {
		t.Errorf("an agent with forged settings: %v", err)
	}

	// A token that is forged, edited, signed with another key or out of date is refused.
	edited := strings.Replace(agentToken, agent.String(), admin.String(), 1)
	for name, token := range map[string]string{
		"none":        "",
		"edited":      edited,
		"another key": query.Sign([]byte("0123456789abcdef0123456789abcdef"), admin, time.Now().Add(time.Minute), nil),
		"expired":     query.Sign(key, admin, time.Now().Add(-time.Second), nil),
		"widened":     strings.Replace(query.Sign(key, admin, time.Now().Add(time.Minute), []string{"query:run"}), "|query:run|", "|*|", 1),
		"garbage":     "a|b|c|d",
	} {
		_, err := as(token, "SELECT name FROM test_people")
		if err == nil {
			t.Errorf("token %s: allowed", name)
			continue
		}
		t.Logf("token %s: %v", name, err)
	}
	rows, err := as(adminToken, "SELECT name FROM test_people")
	if err != nil || len(rows) != 2 {
		t.Fatalf("a good token: %v %v", rows, err)
	}
}
