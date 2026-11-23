package query

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Reading a saved view like a table (GET or QUERY /api/v1/views/{name}/rows): a filter, an order, some columns and a
// page, turned into one SELECT over the view. Column names are checked against the view's columns and quoted; every
// value is a parameter, sent as text for Postgres to convert to the column's type. The SQL then runs like any other
// query, checked and as the reader, so a filter can never reach more than a query could.

// Read is how to read a view's rows.
type Read struct {
	// Where is the filter, as JSON (see Where); empty or null keeps every row.
	Where json.RawMessage
	// Equal are column = value conditions (the GET form), ANDed with Where.
	Equal []Condition
	// Order is the columns to sort by, in turn; Desc reverses one.
	Order []OrderBy
	// Columns are the columns to return; none means all of them.
	Columns []string
	Limit   int
	Offset  int
}

// Condition is one column equal to a value.
type Condition struct {
	Column, Value string
}

// OrderBy is one column to sort by.
type OrderBy struct {
	Column string
	Desc   bool
}

// FilterError is a filter that cannot be read: where in it (where.and[1].title), and why.
type FilterError struct {
	Path    string
	Message string
}

func (e *FilterError) Error() string {
	if e.Path == "" {
		return e.Message
	}
	return e.Path + ": " + e.Message
}

// Limits on one filter, so that building and running it stays cheap.
const (
	maxFilterDepth      = 10
	maxFilterConditions = 200
	maxFilterValues     = 1000
)

// ReadSQL is the SELECT that reads view as asked, with its parameters ($1...). It asks for one row more than Limit,
// so that the caller can tell whether there are more.
func ReadSQL(view string, columns []Column, r Read) (string, []string, error) {
	types := make(map[string]string, len(columns))
	for _, c := range columns {
		types[c.Name] = c.Type
	}
	b := &filterBuilder{types: types}
	var conds []string
	for _, e := range r.Equal {
		if _, ok := types[e.Column]; !ok {
			return "", nil, &FilterError{Message: fmt.Sprintf("%s is not a column of %s", e.Column, view)}
		}
		conds = append(conds, b.column(e.Column)+" = "+b.param(e.Value))
	}
	if w := bytes.TrimSpace(r.Where); len(w) > 0 && !bytes.Equal(w, []byte("null")) {
		cond, err := b.node(w, "where", 0)
		if err != nil {
			return "", nil, err
		}
		conds = append(conds, cond)
	}
	if len(b.params) > maxFilterValues {
		return "", nil, &FilterError{Message: fmt.Sprintf("a filter may hold at most %d values", maxFilterValues)}
	}

	sel := "*"
	if len(r.Columns) > 0 {
		var cols []string
		for i, c := range r.Columns {
			if _, ok := types[c]; !ok {
				return "", nil, &FilterError{Path: fmt.Sprintf("columns[%d]", i), Message: fmt.Sprintf("%s is not a column of %s", c, view)}
			}
			cols = append(cols, b.column(c))
		}
		sel = strings.Join(cols, ", ")
	}
	sql := "SELECT " + sel + " FROM " + pgx.Identifier{view}.Sanitize() + " q"
	if len(conds) > 0 {
		sql += "\nWHERE " + strings.Join(conds, "\n  AND ")
	}
	if len(r.Order) > 0 {
		var parts []string
		for i, o := range r.Order {
			if _, ok := types[o.Column]; !ok {
				return "", nil, &FilterError{Path: fmt.Sprintf("order[%d]", i), Message: fmt.Sprintf("%s is not a column of %s", o.Column, view)}
			}
			p := b.column(o.Column)
			if o.Desc {
				p += " DESC"
			}
			parts = append(parts, p)
		}
		sql += "\nORDER BY " + strings.Join(parts, ", ")
	}
	sql += "\nLIMIT " + strconv.Itoa(r.Limit+1) + " OFFSET " + strconv.Itoa(r.Offset)
	return sql, b.params, nil
}

type filterBuilder struct {
	types  map[string]string
	params []string
	count  int
}

func (b *filterBuilder) column(name string) string { return "q." + pgx.Identifier{name}.Sanitize() }

func (b *filterBuilder) param(v string) string {
	b.params = append(b.params, v)
	return "$" + strconv.Itoa(len(b.params))
}

// node reads one filter node: {"and": [...]}, {"or": [...]}, {"not": {...}}, or {"column": {"op": value, ...}}.
func (b *filterBuilder) node(raw json.RawMessage, path string, depth int) (string, error) {
	if depth > maxFilterDepth {
		return "", &FilterError{Path: path, Message: fmt.Sprintf("a filter may nest at most %d deep", maxFilterDepth)}
	}
	obj, err := object(raw, path)
	if err != nil {
		return "", err
	}
	if len(obj) != 1 {
		return "", &FilterError{Path: path, Message: `give one of "and", "or", "not" or a column, with an object per condition`}
	}
	for key, val := range obj {
		switch key {
		case "and", "or":
			var items []json.RawMessage
			if err := json.Unmarshal(val, &items); err != nil || len(items) == 0 {
				return "", &FilterError{Path: path + "." + key, Message: "give a list of conditions"}
			}
			parts := make([]string, len(items))
			for i, it := range items {
				p, err := b.node(it, fmt.Sprintf("%s.%s[%d]", path, key, i), depth+1)
				if err != nil {
					return "", err
				}
				parts[i] = p
			}
			return "(" + strings.Join(parts, " "+strings.ToUpper(key)+" ") + ")", nil
		case "not":
			p, err := b.node(val, path+".not", depth+1)
			if err != nil {
				return "", err
			}
			return "NOT " + p, nil
		default:
			return b.conditions(key, val, path+"."+key)
		}
	}
	return "", nil // unreachable: one key
}

var textTypes = []string{"text", "varchar", "bpchar", "name", "unknown"}

// conditions reads {"op": value, ...} on one column; several operators are ANDed.
func (b *filterBuilder) conditions(column string, raw json.RawMessage, path string) (string, error) {
	typ, ok := b.types[column]
	if !ok {
		return "", &FilterError{Path: path, Message: fmt.Sprintf("%s is not a column", column)}
	}
	ops, err := object(raw, path)
	if err != nil {
		return "", err
	}
	if len(ops) == 0 {
		return "", &FilterError{Path: path, Message: `give an operator, such as {"eq": "value"}`}
	}
	names := make([]string, 0, len(ops))
	for op := range ops {
		names = append(names, op)
	}
	slices.Sort(names)
	col := b.column(column)
	var parts []string
	for _, op := range names {
		b.count++
		if b.count > maxFilterConditions {
			return "", &FilterError{Path: path, Message: fmt.Sprintf("a filter may have at most %d conditions", maxFilterConditions)}
		}
		val, at := ops[op], path+"."+op
		switch op {
		case "eq", "ne", "lt", "lte", "gt", "gte":
			v, err := scalar(val, at)
			if err != nil {
				return "", err
			}
			sym := map[string]string{"eq": "=", "ne": "<>", "lt": "<", "lte": "<=", "gt": ">", "gte": ">="}[op]
			parts = append(parts, col+" "+sym+" "+b.param(v))
		case "like", "ilike":
			if !slices.Contains(textTypes, typ) {
				return "", &FilterError{Path: at, Message: fmt.Sprintf("%s compares text, and %s is %s", op, column, typ)}
			}
			v, err := scalar(val, at)
			if err != nil {
				return "", err
			}
			parts = append(parts, col+" "+strings.ToUpper(op)+" "+b.param(v))
		case "in", "not_in":
			var items []json.RawMessage
			if err := json.Unmarshal(val, &items); err != nil || len(items) == 0 {
				return "", &FilterError{Path: at, Message: "give a list of values"}
			}
			ps := make([]string, len(items))
			for i, it := range items {
				v, err := scalar(it, fmt.Sprintf("%s[%d]", at, i))
				if err != nil {
					return "", err
				}
				ps[i] = b.param(v)
			}
			kw := " IN ("
			if op == "not_in" {
				kw = " NOT IN ("
			}
			parts = append(parts, col+kw+strings.Join(ps, ", ")+")")
		case "between":
			var pair []json.RawMessage
			if err := json.Unmarshal(val, &pair); err != nil || len(pair) != 2 {
				return "", &FilterError{Path: at, Message: "give two values, the lowest and the highest"}
			}
			lo, err := scalar(pair[0], at+"[0]")
			if err != nil {
				return "", err
			}
			hi, err := scalar(pair[1], at+"[1]")
			if err != nil {
				return "", err
			}
			parts = append(parts, col+" BETWEEN "+b.param(lo)+" AND "+b.param(hi))
		case "is_null":
			var yes bool
			if err := json.Unmarshal(val, &yes); err != nil {
				return "", &FilterError{Path: at, Message: "give true or false"}
			}
			if yes {
				parts = append(parts, col+" IS NULL")
			} else {
				parts = append(parts, col+" IS NOT NULL")
			}
		case "contains":
			if typ != "jsonb" {
				return "", &FilterError{Path: at, Message: fmt.Sprintf("contains looks inside JSON, and %s is %s", column, typ)}
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, val); err != nil {
				return "", &FilterError{Path: at, Message: "give a JSON value"}
			}
			parts = append(parts, col+" @> "+b.param(compact.String())+"::jsonb")
		default:
			return "", &FilterError{Path: at, Message: "not an operator: use eq, ne, lt, lte, gt, gte, in, not_in, between, like, ilike, is_null or contains"}
		}
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return "(" + strings.Join(parts, " AND ") + ")", nil
}

func object(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, &FilterError{Path: path, Message: "give an object"}
	}
	return obj, nil
}

// scalar is a value as the text Postgres reads it: a string as it is, a number exactly as written, true or false.
func scalar(raw json.RawMessage, path string) (string, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return "", &FilterError{Path: path, Message: "give a value"}
	}
	switch x := v.(type) {
	case string:
		return x, nil
	case json.Number:
		return x.String(), nil
	case bool:
		return strconv.FormatBool(x), nil
	case nil:
		return "", &FilterError{Path: path, Message: `null is never equal to anything: use {"is_null": true}`}
	}
	return "", &FilterError{Path: path, Message: "give a string, a number or true or false"}
}
