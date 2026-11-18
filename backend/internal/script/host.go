package script

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	sltime "go.starlark.net/lib/time"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

// host is one run's built-ins: what the script may read and ask for.
type host struct {
	ctx  context.Context
	env  Env
	res  *Result
	prog *Program
	// err is a fault on our side (the reader failed), reported instead of the script's error it caused.
	err error
}

func (h *host) predeclared() starlark.StringDict {
	d := starlark.StringDict{
		"record":   starlark.None,
		"vars":     starlark.NewDict(0),
		"user":     starlark.None,
		"now":      sltime.Time(h.env.Now),
		"children": starlark.NewBuiltin("children", h.children),
		"get":      starlark.NewBuiltin("get", h.get),
		"count":    starlark.NewBuiltin("count", h.count),
		"update":   starlark.NewBuiltin("update", h.update),
		"comment":  starlark.NewBuiltin("comment", h.comment),
		"abort":    starlark.NewBuiltin("abort", h.abort),
	}
	for k, v := range modules {
		d[k] = v
	}
	if h.env.Record != nil {
		d["record"] = recordValue(h.env.Record)
	}
	if h.env.User != nil {
		d["user"] = structOf(reflect.ValueOf(*h.env.User))
	}
	if h.env.Vars != nil {
		v, _ := toStarlark(h.env.Vars)
		d["vars"] = v
	}
	if h.env.Now.IsZero() {
		d["now"] = sltime.Time(time.Now())
	}
	for _, v := range d {
		v.Freeze()
	}
	return d
}

// recordValue is a record as a script reads it: record.title, record.fields.category, record.cis[0].name.
func recordValue(r *expr.Record) starlark.Value { return structOf(reflect.ValueOf(*r)) }

// structOf turns a struct with cel tags into a read-only Starlark struct of the same names.
func structOf(v reflect.Value) starlark.Value {
	t := v.Type()
	d := starlark.StringDict{}
	for i := range t.NumField() {
		name := t.Field(i).Tag.Get("cel")
		if name == "" || !t.Field(i).IsExported() {
			continue
		}
		d[name] = valueOf(v.Field(i))
	}
	return starlarkstruct.FromStringDict(starlarkstruct.Default, d)
}

func valueOf(v reflect.Value) starlark.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return starlark.None
		}
		return valueOf(v.Elem())
	case reflect.Struct:
		if t, ok := v.Interface().(time.Time); ok {
			if t.IsZero() {
				return starlark.None
			}
			return sltime.Time(t)
		}
		return structOf(v)
	case reflect.Slice:
		out := make([]starlark.Value, v.Len())
		for i := range v.Len() {
			out[i] = valueOf(v.Index(i))
		}
		return starlark.NewList(out)
	case reflect.Map:
		x, _ := toStarlark(v.Interface())
		return x
	}
	x, err := toStarlark(v.Interface())
	if err != nil {
		return starlark.None
	}
	return x
}

// toStarlark converts a value as Go holds it (JSON-like) into Starlark.
func toStarlark(x any) (starlark.Value, error) {
	switch v := x.(type) {
	case nil:
		return starlark.None, nil
	case string:
		return starlark.String(v), nil
	case bool:
		return starlark.Bool(v), nil
	case int:
		return starlark.MakeInt(v), nil
	case int64:
		return starlark.MakeInt64(v), nil
	case float64:
		if v == float64(int64(v)) && v < 1<<53 && v > -(1<<53) {
			return starlark.MakeInt64(int64(v)), nil
		}
		return starlark.Float(v), nil
	case interface{ Int64() (int64, error) }: // json.Number
		if n, err := v.Int64(); err == nil {
			return starlark.MakeInt64(n), nil
		}
		return starlark.String(fmt.Sprint(v)), nil
	case time.Time:
		return sltime.Time(v), nil
	case []any:
		out := make([]starlark.Value, len(v))
		for i, e := range v {
			s, err := toStarlark(e)
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return starlark.NewList(out), nil
	case []string:
		out := make([]starlark.Value, len(v))
		for i, e := range v {
			out[i] = starlark.String(e)
		}
		return starlark.NewList(out), nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		d := starlark.NewDict(len(v))
		for _, k := range keys {
			s, err := toStarlark(v[k])
			if err != nil {
				return nil, err
			}
			_ = d.SetKey(starlark.String(k), s)
		}
		return d, nil
	}
	return nil, fmt.Errorf("cannot hand a %T to a script", x)
}

// toGo converts what a script gives back into JSON-like Go: text, int64, float64, bool, nil, lists and maps.
func toGo(v starlark.Value) (any, error) {
	switch x := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.String:
		return string(x), nil
	case starlark.Bool:
		return bool(x), nil
	case starlark.Int:
		if n, ok := x.Int64(); ok {
			return n, nil
		}
		return nil, fmt.Errorf("the number %s is too big", x.BigInt().Text(10))
	case starlark.Float:
		return float64(x), nil
	case sltime.Time:
		return time.Time(x).UTC().Format(time.RFC3339), nil
	case *starlark.List:
		out := make([]any, x.Len())
		for i := range x.Len() {
			g, err := toGo(x.Index(i))
			if err != nil {
				return nil, err
			}
			out[i] = g
		}
		return out, nil
	case starlark.Tuple:
		out := make([]any, len(x))
		for i, e := range x {
			g, err := toGo(e)
			if err != nil {
				return nil, err
			}
			out[i] = g
		}
		return out, nil
	case *starlark.Dict:
		out := map[string]any{}
		for _, item := range x.Items() {
			k, ok := starlark.AsString(item[0])
			if !ok {
				return nil, fmt.Errorf("dict keys must be text, got %s", item[0].Type())
			}
			g, err := toGo(item[1])
			if err != nil {
				return nil, err
			}
			out[k] = g
		}
		return out, nil
	}
	return nil, fmt.Errorf("a %s cannot be kept", v.Type())
}

// recordID is the id of a record a built-in was given: a record from children()/get()/record, or its id or reference.
func (h *host) recordID(v starlark.Value) (string, error) {
	if s, ok := v.(*starlarkstruct.Struct); ok {
		id, err := s.Attr("id")
		if err == nil {
			if str, ok := starlark.AsString(id); ok && (str != "" || h.own(str)) {
				return str, nil // "" is the record being created, before it has an id
			}
		}
		return "", fmt.Errorf("that is not a record")
	}
	if s, ok := starlark.AsString(v); ok && s != "" {
		if h.env.Record != nil && (s == h.env.Record.ID || s == h.env.Record.Reference) {
			return h.env.Record.ID, nil
		}
		if h.env.Reader == nil {
			return "", fmt.Errorf("other records cannot be found here")
		}
		r, err := h.env.Reader.Get(h.ctx, s)
		if err != nil {
			h.err = err
			return "", err
		}
		if r == nil {
			return "", fmt.Errorf("there is no record %s, or it cannot be seen", s)
		}
		return r.ID, nil
	}
	return "", fmt.Errorf("give a record, or its reference")
}

func (h *host) own(id string) bool { return h.env.Record != nil && id == h.env.Record.ID }

func (h *host) effects() int { return len(h.res.Updates) + len(h.res.Comments) }

// children(record, open=False): the records under one, as the script's runner may see them.
func (h *host) children(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var rec starlark.Value
	open := false
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "record", &rec, "open?", &open); err != nil {
		return nil, err
	}
	if h.env.Reader == nil {
		return nil, fmt.Errorf("children() is not available here")
	}
	id, err := h.recordID(rec)
	if err != nil {
		return nil, err
	}
	list, err := h.env.Reader.Children(h.ctx, id, open)
	if err != nil {
		h.err = err
		return nil, err
	}
	out := make([]starlark.Value, len(list))
	for i, r := range list {
		out[i] = recordValue(r)
	}
	return starlark.NewList(out), nil
}

// get(reference): a record by reference or id, or None.
func (h *host) get(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var ref string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "reference", &ref); err != nil {
		return nil, err
	}
	if h.env.Reader == nil {
		return nil, fmt.Errorf("get() is not available here")
	}
	r, err := h.env.Reader.Get(h.ctx, ref)
	if err != nil {
		h.err = err
		return nil, err
	}
	if r == nil {
		return starlark.None, nil
	}
	return recordValue(r), nil
}

// count(target, where): how many records match, as a rule's lookups count them. where maps a field to a value
// (equal) or to {"op": value}.
func (h *host) count(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var target string
	var where *starlark.Dict
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "target", &target, "where?", &where); err != nil {
		return nil, err
	}
	if h.env.Count == nil {
		return nil, fmt.Errorf("count() is not available here")
	}
	var filters []expr.Filter
	if where != nil {
		for _, item := range where.Items() {
			field, ok := starlark.AsString(item[0])
			if !ok {
				return nil, fmt.Errorf("count: fields are named by text")
			}
			if d, ok := item[1].(*starlark.Dict); ok {
				for _, op := range d.Items() {
					name, _ := starlark.AsString(op[0])
					val, err := toGo(op[1])
					if err != nil {
						return nil, err
					}
					filters = append(filters, expr.Filter{Field: field, Op: name, Value: val})
				}
				continue
			}
			val, err := toGo(item[1])
			if err != nil {
				return nil, err
			}
			filters = append(filters, expr.Filter{Field: field, Op: "eq", Value: val})
		}
	}
	n, err := h.env.Count(h.ctx, target, filters)
	if err != nil {
		return nil, fmt.Errorf("count: %w", err)
	}
	return starlark.MakeInt64(n), nil
}

// update(record, fields, comment=None, internal=False): change a record when the script ends. A "status" in fields
// moves it (through its lifecycle, guards and all); the rest are set as record.update sets them.
func (h *host) update(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var rec starlark.Value
	var fields *starlark.Dict
	var note starlark.Value = starlark.None
	internal := false
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "record", &rec, "fields", &fields, "comment?", &note, "internal?", &internal); err != nil {
		return nil, err
	}
	id, err := h.recordID(rec)
	if err != nil {
		return nil, err
	}
	switch {
	case h.own(id) && !h.env.Allow.Own:
		return nil, fmt.Errorf("update() cannot change this record here")
	case !h.own(id) && !h.env.Allow.Others:
		return nil, fmt.Errorf("update() can only change the record this runs for here")
	case h.effects() >= maxEffects:
		return nil, fmt.Errorf("a script may make at most %d changes", maxEffects)
	}
	g, err := toGo(fields)
	if err != nil {
		return nil, err
	}
	u := Update{Record: id, Fields: g.(map[string]any), Internal: internal}
	if s, ok := u.Fields["status"]; ok {
		status, ok := s.(string)
		if !ok {
			return nil, fmt.Errorf("status is the name of a status")
		}
		u.Status = status
		delete(u.Fields, "status")
	}
	if note != starlark.None {
		text, ok := starlark.AsString(note)
		if !ok {
			return nil, fmt.Errorf("comment is text")
		}
		if !h.own(id) || h.env.Allow.Comments {
			u.Comment = text
		} else {
			return nil, fmt.Errorf("a comment cannot be added here")
		}
	}
	h.res.Updates = append(h.res.Updates, u)
	return starlark.None, nil
}

// comment(record, text, internal=False): add a comment when the script ends.
func (h *host) comment(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var rec starlark.Value
	var text string
	internal := false
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "record", &rec, "text", &text, "internal?", &internal); err != nil {
		return nil, err
	}
	id, err := h.recordID(rec)
	if err != nil {
		return nil, err
	}
	switch {
	case h.own(id) && !h.env.Allow.Comments:
		return nil, fmt.Errorf("comment() is not available here")
	case !h.own(id) && !h.env.Allow.Others:
		return nil, fmt.Errorf("comment() can only comment on the record this runs for here")
	case h.effects() >= maxEffects:
		return nil, fmt.Errorf("a script may make at most %d changes", maxEffects)
	}
	h.res.Comments = append(h.res.Comments, Comment{Record: id, Body: text, Internal: internal})
	return starlark.None, nil
}

// abort(message): stop, refusing the save (a before rule) or failing the step (a workflow).
func (h *host) abort(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var msg string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "message", &msg); err != nil {
		return nil, err
	}
	if !h.env.Allow.Abort {
		return nil, fmt.Errorf("abort() is not available here")
	}
	return nil, &Abort{Message: msg}
}
