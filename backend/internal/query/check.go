// Package query runs the SQL people write themselves (docs/query.md). Three things stand between a query and the
// data, and each would hold alone:
//
//  1. Check parses the SQL with Postgres's own parser (libpg_query) and allows only what is on its lists: one SELECT,
//     the views in the `query` schema, and the node types, functions, operators and casts below. Everything else is
//     refused, so nothing new in Postgres is let through by default.
//  2. The query runs as a login role of its own (app_query_role) in a read-only transaction. That role may use only
//     the `query` schema. It is not a member of the owner, so there is no way back to it.
//  3. The views filter their rows by an identity they check themselves (app_query_identity, migration 00017): signed
//     by the server, never read from settings the session could change.
package query

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
	"github.com/wasilibs/go-pgquery/parser"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// MaxLength is the longest query accepted, in bytes.
const MaxLength = 20000

// maxNodes caps how much parse tree a query may have, so that checking it stays cheap.
const maxNodes = 20000

// Refused is why a query was not run: the first thing in it that is not allowed, and where (Line and Column count from
// 1; both are 0 when the problem is the query as a whole).
type Refused struct {
	Message string
	Line    int
	Column  int
}

func (e *Refused) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.Message)
	}
	return e.Message
}

// Checked is a query that passed: the SQL to run (the statement alone, trailing semicolons and space removed) and the
// number of parameters ($1, $2, ...) it takes.
type Checked struct {
	SQL    string
	Params int
	// Original is the text as written and Offset where SQL starts in it, to point at a place Postgres names.
	Original string
	Offset   int
	// Uses is the saved views it reads, by name, each once, in the order they first appear.
	Uses []string
}

// Check decides whether sql may run, given the views that exist (lower-case names in the query schema).
func Check(sql string, views map[string]bool) (*Checked, error) {
	return CheckWith(sql, views, nil)
}

// CheckWith is Check for a query that may also read saved views (saved: their names), written bare, never as
// query.name. A saved view's name wins over a query view's, so that a view added to the query schema later never
// changes what an existing saved view means; a WITH name in scope wins over both.
func CheckWith(sql string, views, saved map[string]bool) (*Checked, error) {
	if strings.TrimSpace(sql) == "" {
		return nil, &Refused{Message: "write a query first"}
	}
	if len(sql) > MaxLength {
		return nil, &Refused{Message: fmt.Sprintf("a query may be at most %d characters", MaxLength)}
	}
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return nil, parseError(sql, err)
	}
	if len(tree.Stmts) != 1 {
		return nil, &Refused{Message: "write one statement: several separated by semicolons are not run"}
	}
	raw := tree.Stmts[0]
	sel := raw.Stmt.GetSelectStmt()
	if sel == nil {
		return nil, &Refused{Message: "only SELECT queries are run: nothing here changes data"}
	}
	c := &checker{sql: sql, views: views, saved: saved}
	c.selectStmt(sel, nil)
	if c.err != nil {
		return nil, c.err
	}
	end := int(raw.StmtLocation + raw.StmtLen)
	if raw.StmtLen == 0 {
		end = len(sql)
	}
	part := sql[raw.StmtLocation:end]
	stmt := strings.TrimSpace(strings.TrimRight(part, " \t\r\n;"))
	offset := int(raw.StmtLocation) + len(part) - len(strings.TrimLeft(part, " \t\r\n"))
	return &Checked{SQL: stmt, Params: c.params, Original: sql, Offset: offset, Uses: c.uses}, nil
}

// parseError turns the parser's error into a Refused with the place it names.
func parseError(sql string, err error) error {
	r := &Refused{Message: err.Error()}
	var pe *parser.Error
	if errors.As(err, &pe) {
		r.Message = pe.Message
		if pe.Cursorpos > 0 {
			r.Line, r.Column = lineCol(sql, pe.Cursorpos-1)
		}
	}
	return r
}

// lineCol is the 1-based line and column of a byte offset.
func lineCol(sql string, offset int) (int, int) {
	if offset < 0 || offset > len(sql) {
		return 0, 0
	}
	before := sql[:offset]
	line := strings.Count(before, "\n") + 1
	col := len([]rune(before[strings.LastIndex(before, "\n")+1:])) + 1
	return line, col
}

type checker struct {
	sql    string
	views  map[string]bool
	saved  map[string]bool
	uses   []string
	err    *Refused
	nodes  int
	params int
}

func (c *checker) refuse(location int32, format string, args ...any) {
	if c.err != nil {
		return
	}
	c.err = &Refused{Message: fmt.Sprintf(format, args...)}
	if location >= 0 {
		c.err.Line, c.err.Column = lineCol(c.sql, int(location))
	}
}

// plain are the node types allowed as they are: their children are checked in turn.
var plain = map[protoreflect.Name]bool{
	"Node": true, "List": true, "String": true, "Integer": true, "Float": true, "Boolean": true, "BitString": true,
	"A_Const": true, "ResTarget": true, "ColumnRef": true, "A_Star": true, "BoolExpr": true, "NullTest": true,
	"BooleanTest": true, "CaseExpr": true, "CaseWhen": true, "CoalesceExpr": true, "MinMaxExpr": true, "Alias": true,
	"JoinExpr": true, "SortBy": true, "WindowDef": true, "SubLink": true, "RangeSubselect": true, "A_Indices": true,
	"A_Indirection": true, "A_ArrayExpr": true, "RowExpr": true, "GroupingSet": true, "RangeFunction": true,
	"TypeCast": true, "NamedArgExpr": true,
}

// node checks one parse tree node and everything under it. scope is the WITH names visible here.
func (c *checker) node(m protoreflect.Message, scope []string) {
	if c.err != nil {
		return
	}
	c.nodes++
	if c.nodes > maxNodes {
		c.refuse(-1, "this query is too large to check")
		return
	}
	switch v := m.Interface().(type) {
	case *pg.SelectStmt:
		c.selectStmt(v, scope)
		return
	case *pg.RangeVar:
		c.rangeVar(v, scope)
		return
	case *pg.FuncCall:
		c.funcCall(v)
	case *pg.A_Expr:
		c.aExpr(v)
	case *pg.SortBy:
		if len(v.UseOp) > 0 {
			c.refuse(v.Location, "ORDER BY ... USING is not allowed in queries: use ASC or DESC")
			return
		}
	case *pg.TypeName:
		c.typeName(v)
		return
	case *pg.SQLValueFunction:
		if !allowedValueFunctions[v.Op.String()] {
			c.refuse(v.Location, "%s is not allowed in queries", strings.TrimPrefix(v.Op.String(), "SVFOP_"))
		}
		return
	case *pg.ParamRef:
		if v.Number < 1 || v.Number > 50 {
			c.refuse(v.Location, "parameters go from $1 to $50")
			return
		}
		c.params = max(c.params, int(v.Number))
		return
	default:
		if !plain[m.Descriptor().Name()] {
			c.refuse(location(m), "%s is not allowed in queries", describe(m.Descriptor().Name()))
			return
		}
	}
	c.children(m, scope)
}

// children checks every node under m.
func (c *checker) children(m protoreflect.Message, scope []string) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Message() != nil:
			l := v.List()
			for i := range l.Len() {
				c.node(l.Get(i).Message(), scope)
			}
		case fd.Message() != nil && !fd.IsMap():
			c.node(v.Message(), scope)
		}
		return c.err == nil
	})
}

// selectStmt checks a SELECT (or VALUES, or a set operation): no INTO, no FOR UPDATE, and the names its WITH brings into
// scope, each visible to the ones after it (and to itself, when recursive) and to the rest of the statement.
func (c *checker) selectStmt(s *pg.SelectStmt, scope []string) {
	if c.err != nil {
		return
	}
	if s.IntoClause != nil {
		c.refuse(location(s.IntoClause.ProtoReflect()), "SELECT INTO makes a table: queries only read")
		return
	}
	if len(s.LockingClause) > 0 {
		c.refuse(-1, "FOR UPDATE and FOR SHARE lock rows: queries only read")
		return
	}
	inner := slices.Clone(scope)
	if w := s.WithClause; w != nil {
		for _, n := range w.Ctes {
			cte := n.GetCommonTableExpr()
			if cte == nil {
				c.refuse(w.Location, "this WITH is not allowed in queries")
				return
			}
			if cte.SearchClause != nil || cte.CycleClause != nil {
				c.refuse(cte.Location, "SEARCH and CYCLE are not allowed in queries")
				return
			}
			if w.Recursive {
				inner = append(inner, strings.ToLower(cte.Ctename))
			}
			q := cte.Ctequery.GetSelectStmt()
			if q == nil {
				c.refuse(cte.Location, "a WITH query may only SELECT")
				return
			}
			c.selectStmt(q, inner)
			if !w.Recursive {
				inner = append(inner, strings.ToLower(cte.Ctename))
			}
		}
	}
	m := s.ProtoReflect()
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Name() == "with_clause" {
			return true
		}
		switch {
		case fd.IsList() && fd.Message() != nil:
			l := v.List()
			for i := range l.Len() {
				c.node(l.Get(i).Message(), inner)
			}
		case fd.Message() != nil:
			c.node(v.Message(), inner)
		}
		return c.err == nil
	})
}

// rangeVar checks a table named in FROM: a WITH name in scope, or a view in the query schema.
func (c *checker) rangeVar(r *pg.RangeVar, scope []string) {
	name := strings.ToLower(r.Relname)
	if r.Catalogname == "" && r.Schemaname == "" && slices.Contains(scope, name) {
		return
	}
	if r.Catalogname == "" && r.Schemaname == "" && c.saved[name] {
		if !slices.Contains(c.uses, name) {
			c.uses = append(c.uses, name)
		}
		if r.Alias != nil {
			c.node(r.Alias.ProtoReflect(), scope)
		}
		return
	}
	if r.Catalogname == "" && (r.Schemaname == "" || r.Schemaname == "query") && c.views[name] {
		if r.Alias != nil {
			c.node(r.Alias.ProtoReflect(), scope)
		}
		return
	}
	full := r.Relname
	if r.Schemaname != "" {
		full = r.Schemaname + "." + full
	}
	c.refuse(r.Location, "%s is not something queries can read: use the views listed beside the editor", full)
}

func (c *checker) funcCall(f *pg.FuncCall) {
	name, ok := qualified(f.Funcname)
	if !ok || !allowedFunctions[name] {
		c.refuse(f.Location, "%s() is not a function queries may call", strings.Join(names(f.Funcname), "."))
	}
}

func (c *checker) aExpr(e *pg.A_Expr) {
	op, ok := qualified(e.Name)
	switch e.Kind {
	case pg.A_Expr_Kind_AEXPR_BETWEEN, pg.A_Expr_Kind_AEXPR_NOT_BETWEEN, pg.A_Expr_Kind_AEXPR_BETWEEN_SYM,
		pg.A_Expr_Kind_AEXPR_NOT_BETWEEN_SYM:
		return // the name is the keyword (BETWEEN); the comparisons are the types' own
	}
	if !ok || !allowedOperators[op] {
		c.refuse(e.Location, "the operator %s is not allowed in queries", strings.Join(names(e.Name), "."))
	}
}

func (c *checker) typeName(t *pg.TypeName) {
	name, ok := qualified(t.Names)
	if !ok || !allowedTypes[name] || t.PctType || t.Setof {
		c.refuse(t.Location, "queries may not cast to %s", strings.Join(names(t.Names), "."))
		return
	}
	for _, n := range t.Typmods {
		c.node(n.ProtoReflect(), nil)
	}
}

// qualified is a function, operator or type name without the pg_catalog schema the parser adds to some of them (EXTRACT
// becomes pg_catalog.extract). Any other schema is refused (ok false).
func qualified(list []*pg.Node) (string, bool) {
	n := names(list)
	switch {
	case len(n) == 1:
		return n[0], true
	case len(n) == 2 && n[0] == "pg_catalog":
		return n[1], true
	}
	return "", false
}

func names(list []*pg.Node) []string {
	out := make([]string, 0, len(list))
	for _, n := range list {
		out = append(out, n.GetString_().GetSval())
	}
	return out
}

// location is a node's place in the query, or -1 when it has none.
func location(m protoreflect.Message) int32 {
	if fd := m.Descriptor().Fields().ByName("location"); fd != nil {
		if v := m.Get(fd).Int(); v >= 0 && v <= MaxLength {
			return int32(v)
		}
	}
	return -1
}

// describe names a node type the way the SQL says it.
func describe(name protoreflect.Name) string {
	if d, ok := described[string(name)]; ok {
		return d
	}
	return string(name)
}

var described = map[string]string{
	"InsertStmt": "INSERT", "UpdateStmt": "UPDATE", "DeleteStmt": "DELETE", "MergeStmt": "MERGE",
	"CollateClause": "COLLATE", "XmlExpr": "XML", "XmlSerialize": "XML", "RangeTableSample": "TABLESAMPLE",
	"RangeTableFunc": "XMLTABLE", "JsonTable": "JSON_TABLE", "CommonTableExpr": "WITH",
}

// What queries may use. Only reading, computing and formatting: nothing that changes a setting or the database, reads
// files or other sessions, waits, takes locks, runs SQL from text (query_to_xml) or reports on the server.
var (
	allowedFunctions = set(
		// aggregates
		"count", "sum", "avg", "min", "max", "bool_and", "bool_or", "every", "string_agg", "array_agg", "json_agg",
		"jsonb_agg", "json_object_agg", "jsonb_object_agg", "percentile_cont", "percentile_disc", "mode", "stddev",
		"stddev_pop", "stddev_samp", "variance", "var_pop", "var_samp", "corr", "covar_pop", "covar_samp",
		"regr_slope", "regr_intercept", "regr_r2", "regr_count", "any_value",
		// window functions
		"row_number", "rank", "dense_rank", "percent_rank", "cume_dist", "ntile", "lag", "lead", "first_value",
		"last_value", "nth_value",
		// numbers
		"abs", "ceil", "ceiling", "floor", "round", "trunc", "sign", "mod", "power", "pow", "sqrt", "cbrt", "exp", "ln",
		"log", "log10", "div", "width_bucket", "random", "greatest", "least", "numeric", "to_number",
		// text
		"lower", "upper", "initcap", "length", "char_length", "character_length", "octet_length", "btrim", "ltrim",
		"rtrim", "substring", "substr", "left", "right", "position", "strpos", "replace", "translate", "split_part",
		"concat", "concat_ws", "format", "lpad", "rpad", "repeat", "reverse", "starts_with", "regexp_replace",
		"regexp_match", "regexp_matches", "regexp_like", "regexp_count", "regexp_substr", "regexp_split_to_array",
		"regexp_split_to_table", "string_to_array", "string_to_table", "array_to_string", "to_char", "md5", "quote_literal",
		"quote_ident", "chr", "ascii", "normalize", "is_normalized", "overlay", "similar_to_escape", "like_escape",
		// dates and times
		"now", "date_trunc", "date_part", "extract", "date_bin", "age", "make_date", "make_time", "make_timestamp",
		"make_timestamptz", "make_interval", "to_timestamp", "to_date", "justify_days", "justify_hours",
		"justify_interval", "isfinite", "timezone", "overlaps", "date_add", "date_subtract", "generate_series",
		"statement_timestamp", "transaction_timestamp",
		// JSON
		"to_json", "to_jsonb", "json_build_object", "jsonb_build_object", "json_build_array", "jsonb_build_array",
		"jsonb_array_length", "json_array_length", "jsonb_typeof", "json_typeof", "jsonb_extract_path",
		"jsonb_extract_path_text", "json_extract_path", "json_extract_path_text", "jsonb_array_elements",
		"jsonb_array_elements_text", "json_array_elements", "json_array_elements_text", "jsonb_object_keys",
		"json_object_keys", "jsonb_each", "jsonb_each_text", "json_each", "json_each_text", "jsonb_strip_nulls",
		"jsonb_pretty", "jsonb_path_query", "jsonb_path_query_array", "jsonb_path_query_first", "jsonb_path_exists",
		"jsonb_path_match",
		// arrays
		"array_length", "cardinality", "unnest", "array_position", "array_positions", "array_append", "array_prepend",
		"array_cat", "array_remove", "array_replace", "array_lower", "array_upper", "array_ndims", "array_dims",
		"trim_array",
		// other
		"nullif", "coalesce", "num_nulls", "num_nonnulls", "gen_random_uuid",
	)
	allowedOperators = set(
		"=", "<>", "!=", "<", ">", "<=", ">=", "+", "-", "*", "/", "%", "^", "||", "~~", "!~~", "~~*", "!~~*", "~",
		"!~", "~*", "!~*", "->", "->>", "#>", "#>>", "@>", "<@", "?", "?|", "?&", "&&", "-|-", "@@", "#-", "@",
	)
	allowedTypes = set(
		"text", "varchar", "bpchar", "char", "name", "int2", "int4", "int8", "smallint", "integer", "int", "bigint",
		"numeric", "decimal", "float4", "float8", "real", "double precision", "bool", "boolean", "date", "time",
		"timetz", "timestamp", "timestamptz", "interval", "uuid", "json", "jsonb", "jsonpath",
	)
	allowedValueFunctions = set(
		"SVFOP_CURRENT_DATE", "SVFOP_CURRENT_TIME", "SVFOP_CURRENT_TIME_N", "SVFOP_CURRENT_TIMESTAMP",
		"SVFOP_CURRENT_TIMESTAMP_N", "SVFOP_LOCALTIME", "SVFOP_LOCALTIME_N", "SVFOP_LOCALTIMESTAMP",
		"SVFOP_LOCALTIMESTAMP_N",
	)
)

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}
