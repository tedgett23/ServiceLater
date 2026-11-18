package apps

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

// Limits of the steps that run other workflows, reach out, and find records (docs/apps-platform.md §7).
const (
	// MaxSubflowDepth is how deep subflows may nest: a run started by a step of a run started by a step... .
	MaxSubflowDepth = 5
	// MaxForeachItems is the most items one foreach runs a workflow for.
	MaxForeachItems = 1000
	// MaxParallel is the most runs a foreach has going at once.
	MaxParallel = 10
	// MaxFind is the most records a query step or a schedule's for_each finds.
	MaxFind = 1000
	// MaxHTTPTimeout is the longest an http.request waits for an answer.
	MaxHTTPTimeout = 30 * time.Second
	// MaxRetries is the most times an http.request tries again.
	MaxRetries = 5
)

// HTTPMethods are the methods an http.request may use.
var HTTPMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

// HTTPAuths are how an http.request may send its secret.
var HTTPAuths = []string{"bearer", "basic", "header"}

// RecordQuery is what a query step or a schedule finds: records of one type matching every filter, written like a
// lookup's (field: {op: value}), as whoever it runs as.
//
//	find:
//	  type: incident
//	  where:
//	    state_category: { eq: open }
//	    updated_at: { lt: =now - duration('168h') }
//	  limit: 200
type RecordQuery struct {
	Type  string                       `json:"type" yaml:"type"`
	Where map[string]map[string]*Value `json:"where,omitempty" yaml:"where"`
	Limit int                          `json:"limit,omitempty" yaml:"limit"`

	lookup *Lookup
}

// queryExtraFields are fields a query may filter by besides a lookup's.
var queryExtraFields = map[string]string{"parent_id": lookupID, "created_at": lookupTime, "updated_at": lookupTime}

// QueryFields are the fields records of a type can be found by. The SQL function app_find_records (migrations/00008_workflows.sql) maps the
// same names to columns; TestQueryFieldsMatchTheDatabase keeps them in step.
func QueryFields(recordType string) (map[string]string, bool) {
	if recordType == "freeze_window" || recordType == "task" {
		return nil, false
	}
	base, ok := lookupFields(recordType)
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(base)+len(queryExtraFields))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range queryExtraFields {
		out[k] = v
	}
	return out, true
}

// compile checks the query and compiles its values, returning what is wrong, prefixed by at.
func (rq *RecordQuery) compile(at string, env *expr.Env, envErr error) []string {
	if rq == nil {
		return nil
	}
	var out []string
	switch {
	case rq.Type == "":
		out = append(out, at+": needs type: the record type to find")
	case rq.Limit < 0 || rq.Limit > MaxFind:
		out = append(out, fmt.Sprintf("%s: limit must be 1 to %d", at, MaxFind))
	}
	where := rq.Where
	if where == nil {
		where = map[string]map[string]*Value{}
	}
	rq.lookup = &Lookup{Count: rq.Type, Where: where, query: true}
	for _, msg := range (Lookups{"find": rq.lookup}).validate(env, envErr) {
		msg = strings.Replace(msg, "lookups.find: ", "", 1)
		msg = strings.Replace(msg, "count "+fmt.Sprintf("%q", rq.Type)+" is not something a lookup can count", fmt.Sprintf("%q is not a record type that can be found", rq.Type), 1)
		if strings.Contains(msg, "needs where") {
			continue // finding every record of a type is allowed: limit bounds it
		}
		out = append(out, at+": "+msg)
	}
	if len(out) > 0 && rq.lookup.err == nil {
		rq.lookup.err = fmt.Errorf("%s", out[0])
	}
	return out
}

// Filters works out the query's filters for in.
func (rq *RecordQuery) Filters(ctx context.Context, in expr.Input) ([]expr.Filter, error) {
	if rq.lookup == nil {
		return nil, fmt.Errorf("the query was not compiled")
	}
	if rq.lookup.err != nil {
		return nil, rq.lookup.err
	}
	out := make([]expr.Filter, 0, len(rq.lookup.filters))
	for _, f := range rq.lookup.filters {
		v, err := filterValue(ctx, f.value, f.op, in)
		if err != nil {
			return nil, fmt.Errorf("where.%s.%s: %w", f.field, f.op, err)
		}
		out = append(out, expr.Filter{Field: f.field, Op: f.op, Value: v})
	}
	return out, nil
}

// Max is how many records the query finds at most.
func (rq *RecordQuery) Max() int {
	if rq.Limit > 0 {
		return rq.Limit
	}
	return 100
}

// Schedule starts a workflow by the clock: a cron line (minute hour day month weekday) in a time zone, once with no
// record, or once per record ForEach finds. Scheduled runs act as the app, for nobody.
type Schedule struct {
	Cron     string       `json:"cron" yaml:"cron"`
	Timezone string       `json:"timezone,omitempty" yaml:"timezone"`
	ForEach  *RecordQuery `json:"for_each,omitempty" yaml:"for_each"`

	spec cron.Schedule
	loc  *time.Location
}

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func (s *Schedule) compile(w *Workflow, env *expr.Env, envErr error) []string {
	var out []string
	spec, err := cronParser.Parse(s.Cron)
	if err != nil {
		out = append(out, fmt.Sprintf("cron %q: %v (five fields: minute hour day month weekday, such as \"0 8 * * 1-5\")", s.Cron, err))
	}
	s.spec = spec
	s.loc = time.UTC
	if s.Timezone != "" {
		loc, err := time.LoadLocation(s.Timezone)
		if err != nil {
			out = append(out, fmt.Sprintf("timezone %q is not a time zone (such as Europe/London)", s.Timezone))
		} else {
			s.loc = loc
		}
	}
	if s.ForEach != nil {
		out = append(out, s.ForEach.compile("for_each", env, envErr)...)
		if s.ForEach.Type != w.RecordType && w.RecordType != "task" {
			out = append(out, fmt.Sprintf("for_each finds %s records, but the workflow is for %s", s.ForEach.Type, w.RecordType))
		}
	}
	return out
}

// Next is when the schedule fires next after t (zero when it cannot run).
func (s *Schedule) Next(t time.Time) time.Time {
	if s.spec == nil {
		return time.Time{}
	}
	return s.spec.Next(t.In(s.loc))
}

// Prev is the latest time the schedule fired at or before t, looking back at most a day (zero if none).
func (s *Schedule) Prev(t time.Time) time.Time {
	if s.spec == nil {
		return time.Time{}
	}
	var last time.Time
	for at := s.spec.Next(t.Add(-24 * time.Hour).In(s.loc)); !at.IsZero() && !at.After(t); at = s.spec.Next(at) {
		last = at
	}
	return last
}

// Items is a foreach's list expression, compiled.
func (s *Step) ItemsValue() *Value { return s.items }

// RecipientPrograms are a notify step's =expression recipients, compiled (nil for a named one).
func (s *Step) RecipientPrograms() []*expr.Program { return s.recipients }

// HTTPTimeout is how long an http.request waits (Timeout, or 10 seconds).
func (s *Step) HTTPTimeout() time.Duration {
	if d, err := ParseDuration(s.Timeout); err == nil && d > 0 {
		return d
	}
	return 10 * time.Second
}

// moreProblems checks the steps that call workflows, reach out, find and file records, and notify.
func (s *Step) moreProblems(w *Workflow, unused func(...string), target func(string, string, bool), add func(string, ...any), forbid func(string, bool)) {
	waitLimit := func() {
		if s.Timeout == "" {
			return
		}
		if d, err := ParseDuration(s.Timeout); err != nil {
			add("timeout: %v", err)
		} else if d < minWait || d > maxWait {
			add("timeout must be between %s and 30d", minWait)
		}
	}
	inputs := func() {
		for _, msg := range assignProblems("inputs", s.Inputs, func(name string) []string {
			if !varNameRE.MatchString(name) || name == "item" || name == "index" {
				return []string{fmt.Sprintf("inputs: %q is not a variable name for the run (lowercase letters, digits and underscores; not item or index)", name)}
			}
			return nil
		}) {
			add("%s", msg)
		}
	}
	text := func(name string, v *Value, required bool) {
		switch {
		case v == nil && required:
			add("%s needs %s", s.Type, name)
		case v != nil && v.Expr == "":
			if t, ok := v.Literal.(string); !ok || strings.TrimSpace(t) == "" {
				add("%s must be text, or =an expression", name)
			}
		}
	}
	switch s.Type {
	case StepSubflow:
		unused("next", "workflow", "inputs", "record", "timeout", "on_timeout", "on_error")
		target("next", s.Next, true)
		target("on_timeout", s.OnTimeout, false)
		target("on_error", s.OnError, false)
		if s.Workflow == "" {
			add("subflow needs workflow: the name of a workflow of this app to run")
		}
		text("record", s.Record, false)
		inputs()
		waitLimit()
	case StepForeach:
		unused("next", "workflow", "items", "each", "parallel", "keep_going", "inputs", "timeout", "on_timeout", "on_error")
		target("next", s.Next, true)
		target("on_timeout", s.OnTimeout, false)
		target("on_error", s.OnError, false)
		if s.Workflow == "" {
			add("foreach needs workflow: the name of a workflow of this app to run for each item")
		}
		if strings.TrimSpace(s.Items) == "" {
			add("foreach needs items: an expression giving the list (such as vars.find_ids)")
		}
		if s.Each != "" && s.Each != "value" && s.Each != "record" {
			add("each must be value or record, got %q", s.Each)
		}
		if s.Parallel < 0 || s.Parallel > MaxParallel {
			add("parallel must be 1 to %d", MaxParallel)
		}
		inputs()
		waitLimit()
	case StepHTTP:
		unused("next", "method", "url", "headers", "body", "secret", "auth", "auth_header", "retries", "timeout", "on_error")
		target("next", s.Next, true)
		target("on_error", s.OnError, false)
		if !slices.Contains(HTTPMethods, s.Method) {
			add("method must be one of %s, got %q", strings.Join(HTTPMethods, ", "), s.Method)
		}
		text("url", s.URL, true)
		if s.URL != nil && s.URL.Expr == "" {
			if u, ok := s.URL.Literal.(string); ok {
				if p, err := url.Parse(u); err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
					add("url must be an http(s) address, such as https://api.example.com/v1/items")
				}
			}
		}
		for _, msg := range assignProblems("headers", s.Headers, func(name string) []string {
			if !headerNameRE.MatchString(name) {
				return []string{fmt.Sprintf("headers: %q is not a header name", name)}
			}
			return nil
		}) {
			add("%s", msg)
		}
		switch {
		case s.Secret != "" && !slices.Contains(HTTPAuths, s.Auth):
			add("auth must be one of %s when a secret is sent", strings.Join(HTTPAuths, ", "))
		case s.Secret == "" && s.Auth != "":
			add("auth needs secret: the name of a secret the app declares in its manifest's config")
		case s.Auth == "header" && !headerNameRE.MatchString(s.AuthHeader):
			add("auth: header needs auth_header: the header to send the secret in (such as X-Api-Key)")
		}
		forbid("auth_header", s.Auth != "header" && s.AuthHeader != "")
		if s.Retries < 0 || s.Retries > MaxRetries {
			add("retries must be 0 to %d", MaxRetries)
		}
		if s.Timeout != "" {
			if d, err := ParseDuration(s.Timeout); err != nil || d < time.Second || d > MaxHTTPTimeout {
				add("timeout must be between 1s and %s", MaxHTTPTimeout)
			}
		}
		if s.Method == "GET" || s.Method == "DELETE" {
			forbid("body", s.Body != nil)
		}
	case StepQuery:
		unused("next", "find")
		target("next", s.Next, true)
		if s.Find == nil {
			add("query needs find: the record type and filters")
		}
	case StepRecordCreate:
		unused("next", "record_type", "title", "description", "group", "fields", "parent")
		target("next", s.Next, true)
		if !validTypeName(s.RecordType) || s.RecordType == "task" {
			add("record.create needs record_type: the type of record to file")
		}
		text("title", s.Title, true)
		text("group", s.Group, false)
		text("parent", s.Parent, false)
		for _, msg := range assignProblems("fields", s.Fields, func(name string) []string {
			if !varNameRE.MatchString(name) {
				return []string{fmt.Sprintf("fields: %q is not a field name", name)}
			}
			return nil
		}) {
			add("%s", msg)
		}
	case StepNotify:
		unused("next", "template", "recipients")
		target("next", s.Next, true)
		if s.Template == "" {
			add("notify needs template: one of this app's notification templates")
		}
		if len(s.Recipients) == 0 {
			add("notify needs recipients: assignee, requester, assignment_group or =an expression giving user ids")
		}
		for i, to := range s.Recipients {
			if !strings.HasPrefix(to, "=") && !slices.Contains(Recipients, to) {
				add("recipients[%d]: %q is not a recipient (%s, or =an expression)", i, to, strings.Join(Recipients, ", "))
			}
		}
	}
}

var headerNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// compileMore compiles the new steps' expressions, reporting each problem through fail.
func (s *Step) compileMore(env *expr.Env, envErr error, fail func(string, error)) {
	s.items, s.recipients = nil, nil
	for _, nv := range s.Inputs {
		nv.Value.compileAny(env, envErr)
		fail("inputs."+nv.Name, nv.Value.err)
	}
	for _, nv := range s.Fields {
		nv.Value.compileAny(env, envErr)
		fail("fields."+nv.Name, nv.Value.err)
	}
	for _, nv := range s.Headers {
		nv.Value.compile(env, envErr, expr.String)
		fail("headers."+nv.Name, nv.Value.err)
	}
	for _, v := range []struct {
		name string
		val  *Value
	}{{"record", s.Record}, {"url", s.URL}, {"parent", s.Parent}} {
		v.val.compile(env, envErr, expr.String)
		if v.val != nil {
			fail(v.name, v.val.err)
		}
	}
	if s.Items != "" {
		s.items = &Value{Expr: strings.TrimPrefix(strings.TrimSpace(s.Items), "=")}
		s.items.compileAny(env, envErr)
		fail("items", s.items.err)
	}
	if s.Find != nil {
		for _, msg := range s.Find.compile("find", env, envErr) {
			fail("", fmt.Errorf("%s", msg))
		}
	}
	s.recipients = make([]*expr.Program, len(s.Recipients))
	for i, to := range s.Recipients {
		if src, ok := strings.CutPrefix(to, "="); ok && envErr == nil {
			p, err := env.CompileAs(strings.TrimSpace(src), expr.StringList)
			s.recipients[i] = p
			fail(fmt.Sprintf("recipients[%d]", i), err)
		}
	}
}

// checkWorkflowCalls checks what the app's workflows name: subflow and foreach run a workflow of this app (without a
// loop of workflows calling each other), notify sends one of its templates.
func checkWorkflowCalls(defs []Definition, p *Problems) {
	workflows := map[string]*Workflow{}
	templates := map[string]bool{}
	files := map[string]string{}
	for _, d := range defs {
		if d.Workflow != nil {
			workflows[d.Workflow.Name], files[d.Workflow.Name] = d.Workflow, d.File
		}
		if d.Notification != nil {
			templates[d.Notification.Name] = true
		}
	}
	calls := map[string][]string{}
	for name, w := range workflows {
		for _, st := range w.Steps {
			switch st.Type {
			case StepSubflow, StepForeach:
				callee := workflows[st.Workflow]
				switch {
				case st.Workflow == "":
				case callee == nil:
					p.add(files[name], "step %s: there is no workflow %q in this app (workflows: %s)", st.ID, st.Workflow, workflowNames(workflows))
				case st.Type == StepSubflow && st.Record == nil && callee.RecordType != w.RecordType && callee.RecordType != "task":
					p.add(files[name], "step %s: workflow %q is for %s records, but this one runs on %s records (give record: to run it on another)", st.ID, st.Workflow, callee.RecordType, w.RecordType)
				default:
					calls[name] = append(calls[name], st.Workflow)
				}
			case StepNotify:
				if st.Template != "" && !templates[st.Template] {
					p.add(files[name], "step %s: there is no notification template %q in this app", st.ID, st.Template)
				}
			}
		}
	}
	// A workflow that reaches itself through subflows would run until the depth limit stopped it.
	for name := range workflows {
		seen := map[string]bool{}
		var reach func(n string) bool
		reach = func(n string) bool {
			for _, c := range calls[n] {
				if c == name {
					return true
				}
				if !seen[c] {
					seen[c] = true
					if reach(c) {
						return true
					}
				}
			}
			return false
		}
		if reach(name) {
			p.add(files[name], "workflow %s runs itself through its subflows, which would never end", name)
		}
	}
}

// HTTPHosts are the hosts an app may call, from its manifest's http: permissions (http:api.example.com, or
// http:*.example.com for its subdomains).
func HTTPHosts(perms []string) []string {
	var out []string
	for _, p := range perms {
		if h, ok := strings.CutPrefix(p, "http:"); ok {
			out = append(out, strings.ToLower(h))
		}
	}
	return out
}

// HostAllowed reports whether host (no port) is one of hosts.
func HostAllowed(hosts []string, host string) bool {
	host = strings.ToLower(host)
	for _, h := range hosts {
		if h == host || (strings.HasPrefix(h, "*.") && strings.HasSuffix(host, h[1:])) {
			return true
		}
	}
	return false
}

var hostPermRE = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// checkHTTPSteps checks the app's http.request steps against its manifest: a literal URL's host is one it may call,
// and a secret is one it declares (config, type secret).
func checkHTTPSteps(m *Manifest, defs []Definition, p *Problems) {
	if m == nil {
		return
	}
	hosts := HTTPHosts(m.Permissions)
	secrets := map[string]bool{}
	for _, c := range m.Config {
		if c.Type == "secret" {
			secrets[c.Key] = true
		}
	}
	for _, d := range defs {
		if d.Workflow == nil {
			continue
		}
		for _, st := range d.Workflow.Steps {
			if st.Type != StepHTTP {
				continue
			}
			if st.URL != nil && st.URL.Expr == "" {
				if raw, ok := st.URL.Literal.(string); ok {
					if u, err := url.Parse(raw); err == nil && u.Host != "" && !HostAllowed(hosts, u.Hostname()) {
						p.add(d.File, "step %s: %s is not a host this app may call: add http:%s to its manifest's permissions", st.ID, u.Hostname(), u.Hostname())
					}
				}
			}
			if st.Secret != "" && !secrets[st.Secret] {
				p.add(d.File, "step %s: the app declares no secret %q: add it to its manifest's config with type: secret", st.ID, st.Secret)
			}
		}
	}
}

// AppSecretName is where an app's declared secret (its manifest's config, type secret) is kept in the secrets store:
// apps.<app>.<key>, with hyphens as underscores.
func AppSecretName(app, key string) string {
	norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "-", "_") }
	return "apps." + norm(app) + "." + norm(key)
}
