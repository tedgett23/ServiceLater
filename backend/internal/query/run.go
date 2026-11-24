package query

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/ratelimit"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// Limits on what one query may take. How many one person may start in an hour (counted from the audit log, so across
// servers) and have running at once (on one server) are settings: queries_per_hour and queries_at_once (Admin → Rate
// limits).
const (
	DefaultTimeout = 30 * time.Second
	DefaultMaxRows = 5000
)

// Runner runs checked queries as the query role (app_query_role) and records every attempt in query_runs.
type Runner struct {
	owner   *pgxpool.Pool
	Timeout time.Duration
	MaxRows int

	mu      sync.Mutex
	pool    *pgxpool.Pool
	key     []byte
	running map[uuid.UUID]int
}

// NewRunner makes a Runner on the server's own (owner) pool. The query role's pool is opened at the first query.
func NewRunner(owner *pgxpool.Pool) *Runner {
	return &Runner{owner: owner, Timeout: DefaultTimeout, MaxRows: DefaultMaxRows, running: map[uuid.UUID]int{}}
}

// Close closes the query role's pool, if it was opened.
func (r *Runner) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pool != nil {
		r.pool.Close()
		r.pool = nil
	}
}

// Request is one query to run, for a person (and the API key they came with, if any: its scopes narrow what the
// query sees, as everywhere else).
type Request struct {
	User   uuid.UUID
	KeyID  uuid.UUID
	Scopes []string // nil: everything the person may do
	SQL    string
	Params []string
}

// Column is one column of a result: its name and its Postgres type (int8, text, timestamptz...).
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Result is what a query gave: at most MaxRows rows, Truncated when there were more.
type Result struct {
	Columns    []Column `json:"columns"`
	Rows       [][]any  `json:"rows"`
	Truncated  bool     `json:"truncated"`
	DurationMS int      `json:"duration_ms"`
}

// Failed is a query that was allowed but did not work: Postgres's message, where in the query it points (Line and
// Column from 1, or 0), and whether it ran out of time.
type Failed struct {
	Message string
	Line    int
	Column  int
	Timeout bool
}

func (e *Failed) Error() string { return e.Message }

// Busy is a person over their limit of queries at once or in an hour.
type Busy struct{ Message string }

func (e *Busy) Error() string { return e.Message }

// Run checks and runs a query. A refusal is a *Refused, a query that went wrong a *Failed, too many a *Busy; any other
// error is the server's own.
func (r *Runner) Run(ctx context.Context, req Request) (*Result, error) {
	if err := r.start(ctx, req.User); err != nil {
		return nil, err
	}
	defer r.finish(req.User)

	started := time.Now()
	res, err := r.run(ctx, req)
	r.audit(req, res, err, time.Since(started))
	return res, err
}

func (r *Runner) start(ctx context.Context, user uuid.UUID) error {
	var lastHour int
	var limits settings.Limits
	if err := db.WithSystem(ctx, r.owner, func(tx pgx.Tx) error {
		var err error
		if limits, err = ratelimit.Load(ctx, dbgen.New(tx)); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM query_runs WHERE user_id = $1 AND started_at > now() - interval '1 hour'`, user).Scan(&lastHour)
	}); err != nil {
		return fmt.Errorf("query: count runs: %w", err)
	}
	if lastHour >= limits.QueriesPerHour {
		return &Busy{Message: fmt.Sprintf("you have run %d queries in the last hour, the most allowed: try again later", limits.QueriesPerHour)}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running[user] >= limits.QueriesAtOnce {
		return &Busy{Message: fmt.Sprintf("you already have %d queries running: wait for one to finish", limits.QueriesAtOnce)}
	}
	r.running[user]++
	return nil
}

func (r *Runner) finish(user uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running[user]--; r.running[user] <= 0 {
		delete(r.running, user)
	}
}

func (r *Runner) run(ctx context.Context, req Request) (*Result, error) {
	pl, err := r.plan(ctx, req.User, req.SQL, "")
	if err != nil {
		return nil, err
	}
	if len(req.Params) != pl.checked.Params {
		return nil, &Refused{Message: fmt.Sprintf("this query takes %d parameters and was given %d", pl.checked.Params, len(req.Params))}
	}
	args := make([]any, len(req.Params))
	for i, p := range req.Params {
		args[i] = p
	}
	return r.execute(ctx, req, pl, r.MaxRows, args)
}

// execute runs a plan as the query role for the person, at most maxRows rows (Truncated when there were more).
func (r *Runner) execute(ctx context.Context, req Request, pl *plan, maxRows int, args []any) (*Result, error) {
	// The text actually run is checked again: whatever it turns out to be, it is something allowed. The saved views
	// in it are WITH names by now.
	wrapped := pl.head + pl.checked.SQL + "\n) q LIMIT " + strconv.Itoa(maxRows+1)
	if _, err := Check(wrapped, pl.views); err != nil {
		return nil, &Refused{Message: "this query cannot be run as written"}
	}

	pool, key, err := r.queryPool(ctx)
	if err != nil {
		return nil, err
	}
	timeout := r.Timeout
	ctx, cancel := context.WithTimeout(ctx, timeout+2*time.Second)
	defer cancel()
	started := time.Now()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("query: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	token := Sign(key, req.User, time.Now().Add(timeout+time.Minute), req.Scopes)
	if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true), set_config('search_path', 'query', true),
		set_config('query.token', $2, true)`, strconv.Itoa(int(timeout.Milliseconds())), token); err != nil {
		return nil, fmt.Errorf("query: settings: %w", err)
	}
	rows, err := tx.Query(ctx, wrapped, args...)
	if err != nil {
		return nil, r.failure(ctx, err, pl)
	}
	defer rows.Close()

	res := &Result{Rows: [][]any{}, Columns: []Column{}}
	tm := tx.Conn().TypeMap()
	for _, f := range rows.FieldDescriptions() {
		typ := "unknown"
		if t, ok := tm.TypeForOID(f.DataTypeOID); ok {
			typ = typeName(t.Name)
		}
		res.Columns = append(res.Columns, Column{Name: f.Name, Type: typ})
	}
	for rows.Next() {
		if len(res.Rows) == maxRows {
			res.Truncated = true
			break
		}
		vals, err := rows.Values()
		if err != nil {
			return nil, r.failure(ctx, err, pl)
		}
		for i, v := range vals {
			vals[i] = jsonValue(v)
		}
		res.Rows = append(res.Rows, vals)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, r.failure(ctx, err, pl)
	}
	res.DurationMS = int(time.Since(started).Milliseconds())
	return res, nil
}

// failure explains an error from Postgres in terms of the person's own query, or of the saved view it is in.
func (r *Runner) failure(ctx context.Context, err error, pl *plan) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return &Failed{Message: "the query was stopped because whoever sent it went away"}
	}
	if ctx.Err() != nil {
		return &Failed{Message: fmt.Sprintf("the query took longer than %s and was stopped", r.Timeout), Timeout: true}
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return &Failed{Message: err.Error()}
	}
	switch pe.Code {
	case "57014": // query_canceled: the statement timeout
		return &Failed{Message: fmt.Sprintf("the query took longer than %s and was stopped", r.Timeout), Timeout: true}
	case "42501": // the views' identity check
		return &Refused{Message: pe.Message}
	}
	f := &Failed{Message: pe.Message}
	if pe.Detail != "" {
		f.Message += ": " + pe.Detail
	}
	// Position counts characters from 1 in the text run: the saved views' WITH, then the person's query.
	pos := int(pe.Position) - 1
	if pe.Position == 0 {
		return f
	}
	if rel := pos - pl.headRunes; rel >= 0 {
		runes := []rune(pl.checked.SQL)
		if rel <= len(runes) {
			f.Line, f.Column = lineCol(pl.checked.Original, pl.checked.Offset+len(string(runes[:rel])))
		}
		return f
	}
	for _, sg := range pl.segments {
		if pos >= sg.start && pos < sg.end {
			f.Message = "in the view " + sg.name + ": " + f.Message
		}
	}
	return f
}

// typeName is a type as the workbench shows it, the same in results and in the schema: Postgres's short name (int2,
// timestamptz), and text[] for an array rather than its internal _text.
func typeName(name string) string {
	if strings.HasPrefix(name, "_") {
		return name[1:] + "[]"
	}
	return name
}

// jsonValue is a column's value as JSON shows it: times in RFC 3339, ids as text, numbers that JSON cannot hold
// (NaN, infinity, numerics past float precision) as text.
func jsonValue(v any) any {
	switch x := v.(type) {
	case nil, bool, string, int16, int32, int64, map[string]any, []any:
		return x
	case float32:
		return jsonValue(float64(x))
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return strconv.FormatFloat(x, 'g', -1, 64)
		}
		return x
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case [16]byte:
		return uuid.UUID(x).String()
	case pgtype.Numeric:
		b, err := x.MarshalJSON()
		if err != nil {
			return nil
		}
		return json.RawMessage(b)
	case pgtype.Interval:
		v, err := x.Value()
		if err != nil {
			return nil
		}
		return v
	case driver.Valuer:
		v, err := x.Value()
		if err != nil {
			return nil
		}
		return jsonValue(v)
	case []byte:
		return base64.StdEncoding.EncodeToString(x)
	}
	return fmt.Sprint(v)
}

// audit records the attempt, whatever happened, as the system: a refused or failed query is recorded too.
func (r *Runner) audit(req Request, res *Result, err error, took time.Duration) {
	outcome, message, rows, truncated := "ok", "", 0, false
	var refused *Refused
	var failed *Failed
	switch {
	case errors.As(err, &refused):
		outcome, message = "refused", refused.Error()
	case errors.As(err, &failed) && failed.Timeout:
		outcome, message = "timeout", failed.Message
	case err != nil:
		outcome, message = "failed", err.Error()
	default:
		rows, truncated = len(res.Rows), res.Truncated
	}
	params := req.Params
	if params == nil {
		params = []string{}
	}
	var key any
	if req.KeyID != uuid.Nil {
		key = req.KeyID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = db.WithSystem(ctx, r.owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO query_runs (user_id, api_key_id, sql, params, outcome, error, row_count, truncated, duration_ms)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			req.User, key, req.SQL, params, outcome, message, rows, truncated, int(took.Milliseconds()))
		return err
	})
}

// viewNames is the views in the query schema, by lower-case name.
func (r *Runner) viewNames(ctx context.Context) (map[string]bool, error) {
	rows, err := r.owner.Query(ctx, `SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'query' AND c.relkind IN ('v', 'm')`)
	if err != nil {
		return nil, fmt.Errorf("query: views: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("query: views: %w", err)
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[strings.ToLower(n)] = true
	}
	return out, nil
}

// queryPool is the pool that connects as the query role, opened at the first query. The role's password comes from
// the signing key, so every server derives the same one; it is set as a SCRAM verifier, so the password itself is
// never sent to the database or written in its logs.
func (r *Runner) queryPool(ctx context.Context) (*pgxpool.Pool, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pool != nil {
		return r.pool, r.key, nil
	}
	var key []byte
	var role string
	if err := r.owner.QueryRow(ctx, `SELECT key, app_query_role() FROM query_keys`).Scan(&key, &role); err != nil {
		return nil, nil, fmt.Errorf("query: key: %w", err)
	}
	password := hex.EncodeToString(derive(key, "login:"+role))
	verifier, err := scramVerifier(password, derive(key, "salt:"+role)[:16])
	if err != nil {
		return nil, nil, err
	}
	if _, err := r.owner.Exec(ctx, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" PASSWORD '"+verifier+"'"); err != nil {
		return nil, nil, fmt.Errorf("query: set the query role's password: %w", err)
	}
	cfg, err := pgxpool.ParseConfig(r.owner.Config().ConnString())
	if err != nil {
		return nil, nil, fmt.Errorf("query: pool config: %w", err)
	}
	cfg.ConnConfig.User = role
	cfg.ConnConfig.Password = password
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	cfg.MaxConns = 8
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 5 * time.Minute
	// Nothing a query did to its session outlives it.
	cfg.AfterRelease = func(c *pgx.Conn) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := c.Exec(ctx, "DISCARD ALL")
		return err == nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("query: pool: %w", err)
	}
	r.pool, r.key = pool, key
	return pool, key, nil
}

// Sign makes the identity a query runs with (app_query_identity checks it): who, until when, narrowed to which scopes.
func Sign(key []byte, user uuid.UUID, expires time.Time, scopes []string) string {
	s := "*"
	if scopes != nil {
		s = strings.Join(scopes, ",")
	}
	msg := user.String() + "|" + strconv.FormatInt(expires.Unix(), 10) + "|" + s
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return msg + "|" + hex.EncodeToString(mac.Sum(nil))
}

func derive(key []byte, purpose string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(purpose))
	return mac.Sum(nil)
}

// scramVerifier is what Postgres stores for a SCRAM-SHA-256 password (RFC 5802, 7677).
func scramVerifier(password string, salt []byte) (string, error) {
	const iterations = 4096
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("query: scram: %w", err)
	}
	client := hmacSum(salted, "Client Key")
	stored := sha256.Sum256(client)
	server := hmacSum(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(stored[:]), b64(server)), nil
}

func hmacSum(key []byte, msg string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return mac.Sum(nil)
}

// View is one view queries can read, with what its comment in the migration says about it and its columns.
type View struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Columns     []ViewColumn `json:"columns"`
}

// ViewColumn is one column of a view.
type ViewColumn struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

// Schema is every view queries can read, by name, for the editor's list and its completion.
func (r *Runner) Schema(ctx context.Context) ([]View, error) {
	rows, err := r.owner.Query(ctx, `SELECT c.relname::text, coalesce(obj_description(c.oid, 'pg_class'), ''),
			a.attname::text, CASE WHEN t.typcategory = 'A' THEN e.typname || '[]' ELSE t.typname::text END,
			coalesce(col_description(c.oid, a.attnum), '')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'query'
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		JOIN pg_type t ON t.oid = a.atttypid
		LEFT JOIN pg_type e ON e.oid = t.typelem
		WHERE c.relkind IN ('v', 'm')
		ORDER BY c.relname, a.attnum`)
	if err != nil {
		return nil, fmt.Errorf("query: schema: %w", err)
	}
	defer rows.Close()
	out := []View{}
	for rows.Next() {
		var view, desc string
		var col ViewColumn
		if err := rows.Scan(&view, &desc, &col.Name, &col.Type, &col.Description); err != nil {
			return nil, fmt.Errorf("query: schema: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].Name != view {
			out = append(out, View{Name: view, Description: desc})
		}
		out[len(out)-1].Columns = append(out[len(out)-1].Columns, col)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: schema: %w", err)
	}
	return out, nil
}
