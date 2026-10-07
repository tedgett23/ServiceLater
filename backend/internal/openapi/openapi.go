// Package openapi writes the OpenAPI description of the HTTP API by reading the api package's source, so it cannot
// drift from the code: there is no second copy of the routes or the types to keep in step.
//
// It reads, from internal/api:
//   - the routes, from the chi calls in NewRouter (Route, Group, Use, With and the method calls), and who may call
//     them, from the Authenticate, auth.RequirePermission and auth.RequireRecordPermission middleware around them;
//   - each handler's doc comment, as the operation's description;
//   - the request body, from the value passed to decode(w, r, &req);
//   - query parameters, from url.Values.Get calls, and path parameters, from the route pattern;
//   - responses, from writeJSON(w, status, value) and w.WriteHeader(status), with the value's Go type (a struct, or
//     a map[string]any literal) turned into a JSON schema through its json tags and field comments;
//   - error statuses, from writeError and the apiError helpers, followed into the package's own functions.
//
// A handler the reader cannot make sense of (no response it can see, a route registered some other way) is an
// error, not a gap in the document. TestOpenAPISpecIsCurrent regenerates the file and compares; the route walk is
// checked against chi.Walk by TestOpenAPICoversEveryRoute.
package openapi

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/tools/go/packages"
)

type schema = map[string]any

// Generate reads the api package in dir and returns the OpenAPI document, indented, ending in a newline.
func Generate(dir string) ([]byte, error) {
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo |
			packages.NeedImports,
		Dir:  dir,
		Fset: fset,
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		return nil, err
	}
	if len(pkgs) != 1 {
		return nil, fmt.Errorf("expected one package in %s, found %d", dir, len(pkgs))
	}
	pkg := pkgs[0]
	for _, e := range pkg.Errors {
		// The document is embedded in the package it describes, so a missing one must not stop it being written.
		if !strings.Contains(e.Msg, "openapi.json") {
			return nil, fmt.Errorf("loading %s: %w", pkg.PkgPath, e)
		}
	}
	g := &gen{fset: fset, pkg: pkg, components: map[string]schema{}, names: map[*types.TypeName]string{},
		taken: map[string]*types.TypeName{}, parsed: map[string]*ast.File{}, fsets: map[string]*token.FileSet{}, codes: map[*types.Func][]int{}}
	return g.document()
}

type gen struct {
	fset       *token.FileSet
	pkg        *packages.Package
	components map[string]schema
	names      map[*types.TypeName]string // the component name given to each named struct
	taken      map[string]*types.TypeName
	parsed     map[string]*ast.File // source files read for doc comments, by name
	fsets      map[string]*token.FileSet
	request    bool                  // building a request body's schema
	codes      map[*types.Func][]int // error statuses a function can write, memoised
	visiting   map[*types.Func]bool
}

// Route is one registered endpoint, as the router's source declares it.
type Route struct {
	Method, Path string
	Auth         bool
	Permissions  []string
	handler      ast.Expr
	stmtDoc      *ast.CommentGroup
}

type scope struct {
	auth        bool
	permissions []string
}

func (g *gen) document() ([]byte, error) {
	routes, err := g.Routes()
	if err != nil {
		return nil, err
	}
	paths := map[string]map[string]any{}
	tags := map[string]bool{}
	for _, rt := range routes {
		op, err := g.operation(rt)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", rt.Method, rt.Path, err)
		}
		if paths[rt.Path] == nil {
			paths[rt.Path] = map[string]any{}
		}
		paths[rt.Path][strings.ToLower(rt.Method)] = op
		tags[op["tags"].([]string)[0]] = true
	}
	var tagList []map[string]string
	for _, t := range sortedKeys(tags) {
		tagList = append(tagList, map[string]string{"name": t})
	}
	g.components["Error"] = schema{
		"type":        "object",
		"description": "Every failure has this shape. The message is meant for a person.",
		"properties":  schema{"error": schema{"type": "string"}},
		"required":    []string{"error"},
	}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "ServiceLater API",
			"version": "v1",
			"description": "Generated from the Go source of internal/api by `make openapi`; do not edit by hand.\n\n" +
				"Sign in with `POST /api/v1/auth/login`. It sets an `sl_access` cookie (a short-lived token) and an " +
				"`sl_refresh` cookie; `POST /api/v1/auth/refresh` renews both. Unsafe requests from a browser must come " +
				"from the same origin. Permissions are checked twice: by role in the API, and by row-level security in " +
				"the database, so a record you may not see answers 404, exactly like one that does not exist.\n\n" +
				"Live updates are a WebSocket at `/ws/v1/subscribe`, which is not described here: it sends hints with " +
				"ids only, and clients refetch through this API.",
		},
		"tags":  tagList,
		"paths": paths,
		"components": map[string]any{
			"schemas": g.components,
			"securitySchemes": map[string]any{
				"cookieAuth": map[string]any{"type": "apiKey", "in": "cookie", "name": "sl_access"},
			},
		},
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// ---- routes ----

// Routes walks NewRouter and lists every /api route it registers, sorted by path and method.
func (g *gen) Routes() ([]Route, error) {
	fn := g.funcDecl("NewRouter")
	if fn == nil {
		return nil, fmt.Errorf("no NewRouter in %s", g.pkg.PkgPath)
	}
	var out []Route
	if err := g.walk(fn.Body.List, "", &scope{}, g.commentMap(fn), &out); err != nil {
		return nil, err
	}
	var api []Route
	for _, r := range out {
		if strings.HasPrefix(r.Path, "/api/") {
			api = append(api, r)
		}
	}
	sort.Slice(api, func(i, j int) bool {
		if api[i].Path != api[j].Path {
			return api[i].Path < api[j].Path
		}
		return api[i].Method < api[j].Method
	})
	return api, nil
}

func (g *gen) commentMap(fn *ast.FuncDecl) ast.CommentMap {
	for _, f := range g.pkg.Syntax {
		if f.Pos() <= fn.Pos() && fn.End() <= f.End() {
			return ast.NewCommentMap(g.fset, f, f.Comments)
		}
	}
	return ast.CommentMap{}
}

func (g *gen) walk(stmts []ast.Stmt, prefix string, sc *scope, cm ast.CommentMap, out *[]Route) error {
	for _, st := range stmts {
		switch s := st.(type) {
		case *ast.ExprStmt:
			call, ok := s.X.(*ast.CallExpr)
			if !ok {
				continue
			}
			var doc *ast.CommentGroup
			if cs := cm[st]; len(cs) > 0 {
				doc = cs[0]
			}
			if err := g.call(call, prefix, sc, cm, doc, out); err != nil {
				return err
			}
		case *ast.IfStmt:
			inner := *sc
			if err := g.walk(s.Body.List, prefix, &inner, cm, out); err != nil {
				return err
			}
		case *ast.BlockStmt:
			inner := *sc
			if err := g.walk(s.List, prefix, &inner, cm, out); err != nil {
				return err
			}
		}
	}
	return nil
}

var methods = map[string]string{"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH", "Delete": "DELETE"}

// call reads one router call: r.Route, r.Group, r.Use, or r[.With(...)].Get(...) and the like.
func (g *gen) call(c *ast.CallExpr, prefix string, sc *scope, cm ast.CommentMap, doc *ast.CommentGroup, out *[]Route) error {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || !g.isRouter(sel.X) {
		return nil
	}
	// r.With(a, b).Get(...): the middleware applies to this one route.
	local := *sc
	local.permissions = append([]string(nil), sc.permissions...)
	recv := sel.X
	for {
		inner, ok := recv.(*ast.CallExpr)
		if !ok {
			break
		}
		isel, ok := inner.Fun.(*ast.SelectorExpr)
		if !ok || isel.Sel.Name != "With" {
			return fmt.Errorf("%s: unsupported router call", g.fset.Position(c.Pos()))
		}
		for _, mw := range inner.Args {
			g.middleware(mw, &local)
		}
		recv = isel.X
	}
	name := sel.Sel.Name
	switch name {
	case "Use":
		for _, mw := range c.Args {
			g.middleware(mw, sc)
		}
		return nil
	case "Route":
		p, err := g.stringLit(c.Args[0])
		if err != nil {
			return err
		}
		body, ok := c.Args[1].(*ast.FuncLit)
		if !ok {
			return fmt.Errorf("%s: Route needs an inline function", g.fset.Position(c.Pos()))
		}
		return g.walk(body.Body.List, joinPath(prefix, p), &local, cm, out)
	case "Group":
		body, ok := c.Args[0].(*ast.FuncLit)
		if !ok {
			return fmt.Errorf("%s: Group needs an inline function", g.fset.Position(c.Pos()))
		}
		return g.walk(body.Body.List, prefix, &local, cm, out)
	}
	method, ok := methods[name]
	if !ok {
		return fmt.Errorf("%s: the OpenAPI generator does not understand router.%s; register routes with Get, Post, Put, Patch or Delete",
			g.fset.Position(c.Pos()), name)
	}
	p, err := g.stringLit(c.Args[0])
	if err != nil {
		return err
	}
	*out = append(*out, Route{Method: method, Path: joinPath(prefix, p), Auth: local.auth, Permissions: local.permissions,
		handler: c.Args[1], stmtDoc: doc})
	return nil
}

// isRouter reports whether e is a chi router (or a With chain on one).
func (g *gen) isRouter(e ast.Expr) bool {
	t := g.pkg.TypesInfo.TypeOf(e)
	if t == nil {
		return false
	}
	n, ok := types.Unalias(t).(*types.Named)
	if !ok {
		if p, ok := t.(*types.Pointer); ok {
			n, ok = p.Elem().(*types.Named)
			if !ok {
				return false
			}
		} else {
			return false
		}
	}
	return n.Obj().Pkg() != nil && strings.HasPrefix(n.Obj().Pkg().Path(), "github.com/go-chi/chi")
}

// middleware records what a middleware means for the API's callers: sign-in, or a permission.
func (g *gen) middleware(e ast.Expr, sc *scope) {
	switch m := e.(type) {
	case *ast.SelectorExpr:
		if m.Sel.Name == "Authenticate" {
			sc.auth = true
		}
	case *ast.CallExpr:
		if sel, ok := m.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RequirePermission" {
			sc.permissions = nil
			for _, a := range m.Args {
				if s, err := g.stringLit(a); err == nil {
					sc.permissions = append(sc.permissions, s)
				}
			}
		}
		// A record permission depends on the record's type, which is known only once it is loaded.
		if sel, ok := m.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RequireRecordPermission" && len(m.Args) == 1 {
			if verb, err := g.stringLit(m.Args[0]); err == nil {
				sc.permissions = []string{"records:" + verb + ":<type>"}
			}
		}
	}
}

func joinPath(prefix, p string) string {
	full := strings.ReplaceAll(prefix+p, "//", "/")
	if len(full) > 1 {
		full = strings.TrimSuffix(full, "/")
	}
	return full
}

// ---- operations ----

var pathParam = regexp.MustCompile(`\{([^}]+)\}`)

func (g *gen) operation(rt Route) (map[string]any, error) {
	body, doc, name, err := g.handler(rt)
	if err != nil {
		return nil, err
	}
	a := g.analyse(body)
	op := map[string]any{
		"operationId": name,
		"summary":     humanize(name),
		"tags":        []string{tagOf(rt.Path)},
	}
	desc := describe(rt.Method, doc)
	if len(rt.Permissions) > 0 {
		if desc != "" {
			desc += "\n\n"
		}
		desc += "Requires the `" + strings.Join(rt.Permissions, "` or `") + "` permission."
	}
	if desc != "" {
		op["description"] = desc
	}
	if len(rt.Permissions) > 0 {
		op["x-permissions"] = rt.Permissions
	}
	if rt.Auth {
		op["security"] = []map[string][]string{{"cookieAuth": {}}}
	} else {
		op["security"] = []map[string][]string{}
	}

	var params []map[string]any
	for _, m := range pathParam.FindAllStringSubmatch(rt.Path, -1) {
		s := schema{"type": "string"}
		if a.uuidParams[m[1]] {
			s["format"] = "uuid"
		}
		params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": s})
	}
	for _, q := range a.query {
		s := schema{"type": "string"}
		if a.intQuery[q] {
			s = schema{"type": "integer"}
		}
		params = append(params, map[string]any{"name": q, "in": "query", "schema": s})
	}
	if len(params) > 0 {
		op["parameters"] = params
	}
	if a.request != nil {
		op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": a.request}}}
	}

	if len(a.success) == 0 {
		return nil, fmt.Errorf("handler %s writes no response the generator can see (writeJSON or w.WriteHeader with a constant status)", name)
	}
	responses := map[string]any{}
	for code, s := range a.success {
		r := map[string]any{"description": http.StatusText(code)}
		if s != nil {
			r["content"] = map[string]any{"application/json": map[string]any{"schema": s}}
		}
		responses[strconv.Itoa(code)] = r
	}
	errs := map[int]bool{}
	for _, c := range a.errors {
		errs[c] = true
	}
	if rt.Auth {
		errs[http.StatusUnauthorized] = true
	}
	if len(rt.Permissions) > 0 {
		errs[http.StatusForbidden] = true
	}
	for c := range errs {
		if _, ok := a.success[c]; ok {
			continue
		}
		responses[strconv.Itoa(c)] = map[string]any{"description": http.StatusText(c),
			"content": map[string]any{"application/json": map[string]any{"schema": schema{"$ref": "#/components/schemas/Error"}}}}
	}
	op["responses"] = responses
	return op, nil
}

// handler finds the body and doc comment of a route's handler: a method or function of the package, or an inline
// function (whose doc is the comment on the statement registering it).
func (g *gen) handler(rt Route) (*ast.BlockStmt, string, string, error) {
	switch h := rt.handler.(type) {
	case *ast.FuncLit:
		segs := strings.Split(strings.Trim(pathParam.ReplaceAllString(rt.Path, ""), "/"), "/")
		return h.Body, rt.stmtDoc.Text(), segs[len(segs)-1], nil
	case *ast.SelectorExpr, *ast.Ident:
		var obj types.Object
		if sel, ok := h.(*ast.SelectorExpr); ok {
			if s := g.pkg.TypesInfo.Selections[sel]; s != nil {
				obj = s.Obj()
			}
		} else {
			obj = g.pkg.TypesInfo.Uses[h.(*ast.Ident)]
		}
		fn, ok := obj.(*types.Func)
		if !ok {
			break
		}
		decl := g.declOf(fn)
		if decl == nil {
			break
		}
		return decl.Body, decl.Doc.Text(), fn.Name(), nil
	}
	return nil, "", "", fmt.Errorf("%s: cannot find the handler's source", g.fset.Position(rt.handler.Pos()))
}

// analysis is what a handler's body says about its request and responses.
type analysis struct {
	request    schema
	success    map[int]schema // status -> body schema (nil for none)
	errors     []int
	query      []string
	intQuery   map[string]bool
	uuidParams map[string]bool
}

func (g *gen) analyse(body *ast.BlockStmt) analysis {
	a := analysis{success: map[int]schema{}, intQuery: map[string]bool{}, uuidParams: map[string]bool{}}
	info := g.pkg.TypesInfo

	// Keys of `for name := range map[string]T{"a": ..., "b": ...}`, so qs.Get(name) names both.
	rangeKeys := map[types.Object][]string{}
	// Variables holding a query value that is parsed as a number.
	numeric := map[types.Object]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.RangeStmt:
			if id, ok := s.Key.(*ast.Ident); ok {
				if lit, ok := s.X.(*ast.CompositeLit); ok {
					for _, el := range lit.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, err := g.stringLit(kv.Key); err == nil {
								rangeKeys[info.Defs[id]] = append(rangeKeys[info.Defs[id]], k)
							}
						}
					}
				}
			}
		case *ast.CallExpr:
			if g.isFunc(s, "strconv", "ParseInt", "Atoi", "ParseUint") && len(s.Args) > 0 {
				if id, ok := s.Args[0].(*ast.Ident); ok {
					numeric[info.Uses[id]] = true
				}
			}
		}
		return true
	})
	// `s := qs.Get("x")` followed by strconv on s: x is a number.
	queryVar := map[types.Object]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if c, ok := as.Rhs[0].(*ast.CallExpr); ok {
			if name, ok := g.queryGet(c); ok {
				if id, ok := as.Lhs[0].(*ast.Ident); ok {
					if obj := info.Defs[id]; obj != nil {
						queryVar[obj] = name
					}
				}
			}
		}
		return true
	})
	for obj, name := range queryVar {
		if numeric[obj] {
			a.intQuery[name] = true
		}
	}

	addQuery := func(name string) {
		for _, q := range a.query {
			if q == name {
				return
			}
		}
		a.query = append(a.query, name)
	}
	ast.Inspect(body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch {
		case g.isLocal(c, "decode") && len(c.Args) == 3:
			if u, ok := c.Args[2].(*ast.UnaryExpr); ok && u.Op == token.AND {
				g.request = true
				a.request = g.schemaOf(info.TypeOf(u.X))
				g.request = false
			}
		case g.isLocal(c, "writeJSON") && len(c.Args) == 3:
			if code, ok := g.intConst(c.Args[1]); ok {
				if _, seen := a.success[code]; !seen {
					a.success[code] = g.valueSchema(c.Args[2])
				}
			}
		case g.isLocal(c, "intParam") && len(c.Args) >= 2:
			if inner, ok := c.Args[1].(*ast.CallExpr); ok {
				if name, ok := g.queryGet(inner); ok {
					a.intQuery[name] = true
				}
			}
		case g.isLocal(c, "urlUUID") && len(c.Args) == 3:
			if s, err := g.stringLit(c.Args[2]); err == nil {
				a.uuidParams[s] = true
			}
		case g.isFunc(c, "github.com/google/uuid", "Parse") && len(c.Args) == 1:
			if inner, ok := c.Args[0].(*ast.CallExpr); ok && g.isFunc(inner, "github.com/go-chi/chi/v5", "URLParam") && len(inner.Args) == 2 {
				if s, err := g.stringLit(inner.Args[1]); err == nil {
					a.uuidParams[s] = true
				}
			}
		}
		if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "WriteHeader" && len(c.Args) == 1 {
			if code, ok := g.intConst(c.Args[0]); ok && code < 400 {
				a.success[code] = nil
			}
		}
		if name, ok := g.queryGet(c); ok {
			addQuery(name)
		} else if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Get" && g.isQueryValues(sel.X) && len(c.Args) == 1 {
			if id, ok := c.Args[0].(*ast.Ident); ok {
				for _, k := range rangeKeys[info.Uses[id]] {
					addQuery(k)
				}
			}
		}
		return true
	})
	a.errors = g.errorCodes(body)
	return a
}

// queryGet matches qs.Get("name") on a url.Values.
func (g *gen) queryGet(c *ast.CallExpr) (string, bool) {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Get" || len(c.Args) != 1 || !g.isQueryValues(sel.X) {
		return "", false
	}
	s, err := g.stringLit(c.Args[0])
	return s, err == nil
}

func (g *gen) isQueryValues(e ast.Expr) bool {
	t := g.pkg.TypesInfo.TypeOf(e)
	return t != nil && t.String() == "net/url.Values"
}

// errorCodes is every constant error status the code can write, following calls into the package's own functions.
func (g *gen) errorCodes(body ast.Node) []int {
	set := map[int]bool{}
	info := g.pkg.TypesInfo
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if g.isLocal(x, "writeError") && len(x.Args) >= 2 {
				for _, c := range g.intConsts(x.Args[1]) {
					set[c] = true
				}
			}
			if fn := g.calledLocalFunc(x); fn != nil {
				for _, c := range g.funcCodes(fn) {
					set[c] = true
				}
			}
		case *ast.CompositeLit:
			// &apiError{status, message}
			if t := info.TypeOf(x); t != nil && strings.HasSuffix(t.String(), ".apiError") && len(x.Elts) > 0 {
				for _, c := range g.intConsts(x.Elts[0]) {
					set[c] = true
				}
			}
		}
		return true
	})
	var out []int
	for c := range set {
		if c >= 400 {
			out = append(out, c)
		}
	}
	sort.Ints(out)
	return out
}

func (g *gen) funcCodes(fn *types.Func) []int {
	if c, ok := g.codes[fn]; ok {
		return c
	}
	if g.visiting == nil {
		g.visiting = map[*types.Func]bool{}
	}
	if g.visiting[fn] {
		return nil
	}
	g.visiting[fn] = true
	defer delete(g.visiting, fn)
	decl := g.declOf(fn)
	if decl == nil || decl.Body == nil {
		return nil
	}
	c := g.errorCodes(decl.Body)
	g.codes[fn] = c
	return c
}

// calledLocalFunc is the function or method of this package that c calls, if it is one.
func (g *gen) calledLocalFunc(c *ast.CallExpr) *types.Func {
	var obj types.Object
	switch f := c.Fun.(type) {
	case *ast.Ident:
		obj = g.pkg.TypesInfo.Uses[f]
	case *ast.SelectorExpr:
		if s := g.pkg.TypesInfo.Selections[f]; s != nil {
			obj = s.Obj()
		} else {
			obj = g.pkg.TypesInfo.Uses[f.Sel]
		}
	}
	fn, ok := obj.(*types.Func)
	if !ok || fn.Pkg() != g.pkg.Types {
		return nil
	}
	return fn
}

func (g *gen) isLocal(c *ast.CallExpr, name string) bool {
	id, ok := c.Fun.(*ast.Ident)
	if !ok || id.Name != name {
		return false
	}
	fn, ok := g.pkg.TypesInfo.Uses[id].(*types.Func)
	return ok && fn.Pkg() == g.pkg.Types
}

func (g *gen) isFunc(c *ast.CallExpr, pkgPath string, names ...string) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	fn, ok := g.pkg.TypesInfo.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != pkgPath {
		return false
	}
	for _, n := range names {
		if fn.Name() == n {
			return true
		}
	}
	return false
}

func (g *gen) intConst(e ast.Expr) (int, bool) {
	tv, ok := g.pkg.TypesInfo.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.Int {
		return 0, false
	}
	n, ok := constant.Int64Val(tv.Value)
	return int(n), ok
}

// intConsts is a constant status, or every value of a map literal indexed to pick one.
func (g *gen) intConsts(e ast.Expr) []int {
	if n, ok := g.intConst(e); ok {
		return []int{n}
	}
	if ix, ok := e.(*ast.IndexExpr); ok {
		if lit, ok := ix.X.(*ast.CompositeLit); ok {
			var out []int
			for _, el := range lit.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if n, ok := g.intConst(kv.Value); ok {
						out = append(out, n)
					}
				}
			}
			return out
		}
	}
	return nil
}

func (g *gen) stringLit(e ast.Expr) (string, error) {
	tv, ok := g.pkg.TypesInfo.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", fmt.Errorf("%s: expected a constant string", g.fset.Position(e.Pos()))
	}
	return constant.StringVal(tv.Value), nil
}

func (g *gen) funcDecl(name string) *ast.FuncDecl {
	for _, f := range g.pkg.Syntax {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == name {
				return fd
			}
		}
	}
	return nil
}

func (g *gen) declOf(fn *types.Func) *ast.FuncDecl {
	for _, f := range g.pkg.Syntax {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Pos() == fn.Pos() {
				return fd
			}
		}
	}
	return nil
}

// ---- schemas ----

// valueSchema is the schema of a response value: a map[string]any literal is an object with exactly its keys.
func (g *gen) valueSchema(e ast.Expr) schema {
	if lit, ok := e.(*ast.CompositeLit); ok {
		if m, ok := g.pkg.TypesInfo.TypeOf(lit).Underlying().(*types.Map); ok && isString(m.Key()) {
			props := schema{}
			var required []string
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				k, err := g.stringLit(kv.Key)
				if err != nil {
					continue
				}
				props[k] = g.schemaOf(g.pkg.TypesInfo.TypeOf(kv.Value))
				required = append(required, k)
			}
			sort.Strings(required)
			return schema{"type": "object", "properties": props, "required": required}
		}
	}
	return g.schemaOf(g.pkg.TypesInfo.TypeOf(e))
}

func isString(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

var wellKnown = map[string]schema{
	"github.com/google/uuid.UUID":         {"type": "string", "format": "uuid"},
	"time.Time":                           {"type": "string", "format": "date-time"},
	"time.Duration":                       {"type": "integer", "description": "nanoseconds"},
	"encoding/json.RawMessage":            {},
	"github.com/google/uuid.NullUUID":     {"type": []string{"string", "null"}, "format": "uuid"},
	"github.com/jackc/pgx/v5/pgtype.Text": {"type": []string{"string", "null"}},
}

func (g *gen) schemaOf(t types.Type) schema {
	t = types.Unalias(t)
	switch x := t.(type) {
	case *types.Pointer:
		return nullable(g.schemaOf(x.Elem()))
	case *types.Named:
		full := x.Obj().Name()
		if x.Obj().Pkg() != nil {
			full = x.Obj().Pkg().Path() + "." + full
		}
		if s, ok := wellKnown[full]; ok {
			return copySchema(s)
		}
		// nullable[T] is a PATCH field: absent, null or a T.
		if x.Obj().Pkg() == g.pkg.Types && x.Obj().Name() == "nullable" && x.TypeArgs().Len() == 1 {
			return nullable(g.schemaOf(x.TypeArgs().At(0)))
		}
		if _, ok := x.Underlying().(*types.Struct); ok {
			if hasMarshaler(x) {
				return schema{}
			}
			return schema{"$ref": "#/components/schemas/" + g.component(x)}
		}
		return g.schemaOf(x.Underlying())
	case *types.Basic:
		switch {
		case x.Info()&types.IsBoolean != 0:
			return schema{"type": "boolean"}
		case x.Info()&types.IsString != 0:
			return schema{"type": "string"}
		case x.Info()&types.IsInteger != 0:
			s := schema{"type": "integer"}
			switch x.Kind() {
			case types.Int64, types.Uint64, types.Int, types.Uint:
				s["format"] = "int64"
			case types.Int32, types.Int16, types.Int8, types.Uint32, types.Uint16, types.Uint8:
				s["format"] = "int32"
			}
			return s
		case x.Info()&types.IsFloat != 0:
			return schema{"type": "number"}
		}
	case *types.Slice:
		if b, ok := x.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Byte {
			return schema{"type": "string", "format": "byte"}
		}
		return schema{"type": "array", "items": g.schemaOf(x.Elem())}
	case *types.Array:
		return schema{"type": "array", "items": g.schemaOf(x.Elem())}
	case *types.Map:
		return schema{"type": "object", "additionalProperties": g.schemaOf(x.Elem())}
	case *types.Interface:
		return schema{}
	case *types.Struct:
		return g.structSchema(x)
	}
	return schema{}
}

func hasMarshaler(n *types.Named) bool {
	for _, t := range []types.Type{n, types.NewPointer(n)} {
		ms := types.NewMethodSet(t)
		if ms.Lookup(nil, "MarshalJSON") != nil {
			return true
		}
		for i := 0; i < ms.Len(); i++ {
			if ms.At(i).Obj().Name() == "MarshalJSON" {
				return true
			}
		}
	}
	return false
}

func nullable(s schema) schema {
	switch t := s["type"].(type) {
	case string:
		out := copySchema(s)
		out["type"] = []string{t, "null"}
		return out
	case []string:
		return s
	}
	if len(s) == 0 {
		return s
	}
	return schema{"anyOf": []schema{s, {"type": "null"}}}
}

func copySchema(s schema) schema {
	out := schema{}
	for k, v := range s {
		out[k] = v
	}
	return out
}

// component names a struct type and adds its schema to components once. An unexported name loses a DTO suffix and
// gains a capital (ticketDTO is Ticket); a clash between packages is settled with the package's name.
func (g *gen) component(n *types.Named) string {
	obj := n.Obj()
	if name, ok := g.names[obj]; ok {
		return name
	}
	name := obj.Name()
	if !obj.Exported() {
		name = strings.TrimSuffix(name, "DTO")
		name = string(unicode.ToUpper(rune(name[0]))) + name[1:]
	}
	if other, ok := g.taken[name]; ok && other != obj {
		name = strings.ToUpper(obj.Pkg().Name()[:1]) + obj.Pkg().Name()[1:] + name
	}
	g.names[obj], g.taken[name] = name, obj
	g.components[name] = schema{} // reserve, for types that refer to themselves
	s := g.structSchema(n.Underlying().(*types.Struct))
	if d := stripName(g.docAt(obj.Pos(), false), obj.Name()); d != "" {
		s["description"] = d
	}
	g.components[name] = s
	return name
}

func (g *gen) structSchema(st *types.Struct) schema {
	props := schema{}
	var required []string
	g.fields(st, props, &required)
	sort.Strings(required)
	out := schema{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func (g *gen) fields(st *types.Struct, props schema, required *[]string) {
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		tag := reflectTag(st.Tag(i))
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if f.Embedded() && name == "" {
			t := types.Unalias(f.Type())
			if p, ok := t.(*types.Pointer); ok {
				t = p.Elem()
			}
			if inner, ok := t.Underlying().(*types.Struct); ok {
				g.fields(inner, props, required)
				continue
			}
		}
		if !f.Exported() {
			continue
		}
		if name == "" {
			name = f.Name()
		}
		s := g.schemaOf(f.Type())
		if d := stripName(g.docAt(f.Pos(), true), f.Name()); d != "" {
			if _, isRef := s["$ref"]; isRef {
				s = schema{"allOf": []schema{s}, "description": d}
			} else {
				s = copySchema(s)
				s["description"] = d
			}
		}
		props[name] = s
		// A request body is decoded leniently: a missing field is its zero value, so only validation (which the
		// description states) makes one required.
		if !g.request && !strings.Contains(opts, "omitempty") && !strings.Contains(opts, "omitzero") {
			*required = append(*required, name)
		}
	}
}

func reflectTag(tag string) string {
	const key = `json:"`
	i := strings.Index(tag, key)
	if i < 0 {
		return ""
	}
	rest := tag[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// docAt finds the comment on the declaration at pos: a struct field (its doc or line comment) or a type. Types from
// other packages carry positions from export data, so the source file is read again and matched by line and column.
func (g *gen) docAt(pos token.Pos, field bool) string {
	if !pos.IsValid() {
		return ""
	}
	p := g.fset.Position(pos)
	if p.Filename == "" {
		return ""
	}
	f, ok := g.parsed[p.Filename]
	if !ok {
		fs := token.NewFileSet()
		parsedFile, err := parser.ParseFile(fs, p.Filename, nil, parser.ParseComments)
		if err != nil {
			g.parsed[p.Filename] = nil
			return ""
		}
		f = parsedFile
		g.parsed[p.Filename] = f
		g.fsets[p.Filename] = fs
	}
	if f == nil {
		return ""
	}
	fs := g.fsets[p.Filename]
	at := func(id *ast.Ident) bool {
		q := fs.Position(id.Pos())
		return q.Line == p.Line && q.Column == p.Column
	}
	var doc string
	ast.Inspect(f, func(n ast.Node) bool {
		if doc != "" {
			return false
		}
		switch x := n.(type) {
		case *ast.Field:
			if !field {
				return true
			}
			for _, id := range x.Names {
				if at(id) {
					doc = strings.TrimSpace(x.Doc.Text())
					if doc == "" {
						doc = strings.TrimSpace(x.Comment.Text())
					}
				}
			}
		case *ast.GenDecl:
			if field {
				return true
			}
			for _, sp := range x.Specs {
				if ts, ok := sp.(*ast.TypeSpec); ok && at(ts.Name) {
					doc = strings.TrimSpace(ts.Doc.Text())
					if doc == "" && len(x.Specs) == 1 {
						doc = strings.TrimSpace(x.Doc.Text())
					}
				}
			}
		}
		return true
	})
	return unwrap(doc)
}

// ---- text ----

var routeLine = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE) /\S*( \((staff|admin)\))?[:.]?\s*`)

// describe turns a handler's doc comment into a description: the leading "GET /path (staff):" restates what the
// document already shows, so it goes.
func describe(method string, doc string) string {
	doc = strings.TrimSpace(doc)
	if m := routeLine.FindString(doc); m != "" && strings.HasPrefix(m, method) {
		doc = strings.TrimSpace(doc[len(m):])
	}
	if doc != "" {
		r := []rune(doc)
		r[0] = unicode.ToUpper(r[0])
		doc = string(r)
	}
	return unwrap(doc)
}

// unwrap joins the hard-wrapped lines of a Go comment into paragraphs.
func unwrap(s string) string {
	paras := strings.Split(strings.TrimSpace(s), "\n\n")
	for i, p := range paras {
		paras[i] = strings.Join(strings.Fields(p), " ")
	}
	return strings.Join(paras, "\n\n")
}

// stripName turns a Go doc comment into a description: "ticketDTO is a ticket ..." becomes "A ticket ...".
func stripName(doc, name string) string {
	rest, ok := strings.CutPrefix(doc, name+" ")
	if !ok {
		return doc
	}
	for _, verb := range []string{"is ", "are "} {
		if r, ok := strings.CutPrefix(rest, verb); ok {
			rest = r
			break
		}
	}
	if rest == "" {
		return doc
	}
	r := []rune(rest)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// humanize turns a handler's name into a summary: listMyNotifications is "List my notifications", and an acronym
// stays one word, so getOpenAPI is "Get open API".
func humanize(name string) string {
	r := []rune(name)
	var words []string
	start := 0
	for i := 1; i < len(r); i++ {
		lowerToUpper := unicode.IsLower(r[i-1]) && unicode.IsUpper(r[i])
		acronymEnds := unicode.IsUpper(r[i-1]) && unicode.IsUpper(r[i]) && i+1 < len(r) && unicode.IsLower(r[i+1])
		if lowerToUpper || acronymEnds {
			words = append(words, string(r[start:i]))
			start = i
		}
	}
	words = append(words, string(r[start:]))
	for i, w := range words {
		if strings.ToUpper(w) != w || len(w) == 1 { // leave acronyms as they are
			words[i] = strings.ToLower(w)
		}
	}
	s := strings.Join(words, " ")
	return strings.ToUpper(s[:1]) + s[1:]
}

func tagOf(path string) string {
	segs := strings.Split(strings.TrimPrefix(path, "/api/v1/"), "/")
	return strings.TrimSuffix(segs[0], ".json")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
