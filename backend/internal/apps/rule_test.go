package apps

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const goodRule = `kind: rule
name: critical-needs-assignee
record_type: incident
timing: before
on: [update]
when: 'record.priority == 1 && !has(record.assignee_id)'
run:
  - action: abort
    message: Critical incidents need an assignee.
`

func ruleApp(files map[string]string) map[string]string {
	out := map[string]string{"app.yaml": goodManifest}
	for name, content := range files {
		out["default/rules/"+name] = content
	}
	return out
}

func TestAGoodRuleCompilesToRunnablePieces(t *testing.T) {
	src := `kind: rule
name: triage
record_type: incident
timing: before
on: [create, update]
order: 20
on_error: skip
when: 'event == "create" || record.priority != old.priority'
run:
  - action: set
    field: priority
    value: '=record.title.contains("urgent") ? 1 : 3'
  - action: set
    field: title
    value: "Untitled"
    when: 'size(record.title) == 0'
  - action: set
    field: assignee_id
    value: =user.id
  - action: abort
    message: =record.title + " cannot be saved"
    when: 'record.priority == 4'
`
	c, p := compile(t, ruleApp(map[string]string{"triage.yaml": src}))
	if p != nil {
		t.Fatal(p)
	}
	if len(c.Definitions) != 1 || c.Definitions[0].Kind != "rule" || c.Definitions[0].Name != "triage" {
		t.Fatalf("definitions: %+v", c.Definitions)
	}
	r := c.Definitions[0].Rule
	if r.Order != 20 || r.OnError != OnErrorSkip || !r.Applies(TimingBefore, EventCreate) || !r.Applies(TimingBefore, EventUpdate) || r.Applies(TimingAfter, EventCreate) {
		t.Errorf("rule: %+v", r)
	}
	if prog, err := r.Condition(); prog == nil || err != nil {
		t.Errorf("the rule's filter should be compiled: %v", err)
	}
	if prog, err := r.Run[0].Value.Compiled(); prog == nil || err != nil {
		t.Errorf("an =expression value should be compiled: %v", err)
	}
	if prog, _ := r.Run[1].Value.Compiled(); prog != nil || r.Run[1].Value.Literal != "Untitled" {
		t.Errorf("a literal has no program and keeps its value: %+v", r.Run[1].Value)
	}
	if prog, err := r.Run[3].Condition(); prog == nil || err != nil {
		t.Errorf("the per-action filter should be compiled: %v", err)
	}
	if r.Run[0].Value.Literal != nil || r.Run[0].Value.Expr == "" {
		t.Errorf("an =expression is an expression, not a literal: %+v", r.Run[0].Value)
	}
}

func TestDefaultsForARule(t *testing.T) {
	c, p := compile(t, ruleApp(map[string]string{"r.yaml": goodRule}))
	if p != nil {
		t.Fatal(p)
	}
	r := c.Definitions[0].Rule
	if r.Order != 100 || r.OnError != OnErrorBlock {
		t.Errorf("order %d, on_error %q: want 100 and block (fail closed)", r.Order, r.OnError)
	}
}

const asyncRule = `kind: rule
name: escalation-note
record_type: incident
timing: async
on: [update]
when: 'record.priority == 1'
run:
  - action: comment.add
    body: =user.name + " raised this to critical."
    internal: true
`

// ruleWith is a minimal rule around the given actions, at the given timing and record type.
func ruleWith(timing, recordType, run string) map[string]string {
	return ruleApp(map[string]string{"r.yaml": fmt.Sprintf(
		"kind: rule\nname: rr\nrecord_type: %s\ntiming: %s\non: [update]\nrun:\n%s", recordType, timing, run)})
}

func TestRulesAreRejectedWhenTheyAreWrong(t *testing.T) {
	with := func(replace, with string) map[string]string {
		return ruleApp(map[string]string{"r.yaml": strings.Replace(goodRule, replace, with, 1)})
	}
	before := func(run string) map[string]string { return ruleWith("before", "incident", run) }
	after := func(run string) map[string]string { return ruleWith("after", "incident", run) }
	many := strings.Repeat("  - action: abort\n    message: no\n", 21)

	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		// the rule as a whole
		{"a bad name", with("name: critical-needs-assignee", "name: Bad_Name"), "name \"Bad_Name\""},
		{"an unknown record type", with("record_type: incident", "record_type: widget"), "record_type \"widget\""},
		{"an unknown timing", with("timing: before", "timing: sometime"), "timing must be before, after or async"},
		{"no events", with("on: [update]", "on: []"), "on is required"},
		{"delete is not here yet", with("on: [update]", "on: [delete]"), "on: delete is not supported yet"},
		{"a made-up event", with("on: [update]", "on: [nightly]"), "\"nightly\" is not an event"},
		{"an event twice", with("on: [update]", "on: [update, update]"), "listed twice"},
		{"a comment event before the save", with("on: [update]", "on: [comment]"), "on: comment is only for after rules"},
		{"mentioned outside a comment rule", after("  - action: notify\n    template: t\n    to: [mentioned]\n"), "the rule must be on: [comment]"},
		{"order out of range", with("on: [update]", "on: [update]\norder: 5000"), "order must be from 0 to 1000"},
		{"a bad on_error", with("on: [update]", "on: [update]\non_error: shrug"), "on_error must be block or skip"},
		{"an unknown field", with("timing: before", "timing: before\ntimimg: after"), "field timimg not found"},
		{"the wrong kind", with("kind: rule", "kind: lifecycle"), "kind must be rule"},
		{"no actions", before(""), "at least one action"},
		{"too many actions", before(many), "at most 20 actions"},

		// the filter
		{"a typo in the filter", with("record.priority == 1", "record.prioity == 1"), "when: column"},
		{"a filter that is not true or false", with("record.priority == 1 && !has(record.assignee_id)", "record.priority"), "must produce true or false"},
		{"a filter comparing with null", with("!has(record.assignee_id)", "record.assignee_id == null"), "do not compare with null"},
		{"a typo in an action's filter", before("  - action: abort\n    message: no\n    when: 'record.prioity == 1'\n"), "run[0]: when: column"},

		// actions
		{"an unknown action", before("  - action: explode\n"), "unknown action \"explode\""},
		{"an action with no name", before("  - message: hi\n"), "an action needs a name"},
		{"abort without a message", before("  - action: abort\n"), "abort needs a message"},
		{"abort with a stray field", before("  - action: abort\n    message: no\n    field: title\n"), "abort does not take field"},
		{"abort with a message that is not text", before("  - action: abort\n    message: =record.priority\n"), "message: the expression must produce text"},
		{"set with no field", before("  - action: set\n    value: 1\n"), "set needs a field"},
		{"set of a field that cannot be set", before("  - action: set\n    field: status\n    value: closed\n"), "set cannot change \"status\""},
		{"set with no value", before("  - action: set\n    field: priority\n"), "set needs a value"},
		{"set in an after rule", after("  - action: set\n    field: priority\n    value: 1\n"), "only allowed in a before rule"},
		{"a priority out of range", before("  - action: set\n    field: priority\n    value: 9\n"), "priority must be 1 (critical) to 4 (low)"},
		{"a priority that is text", before("  - action: set\n    field: priority\n    value: high\n"), "priority takes a whole number"},
		{"a title that is a number", before("  - action: set\n    field: title\n    value: 5\n"), "title takes text"},
		{"an empty title", before("  - action: set\n    field: title\n    value: \"\"\n"), "title must be 1 to 200"},
		{"a priority expression that is text", before("  - action: set\n    field: priority\n    value: =record.title\n"), "must produce a whole number, not string"},
		{"a title expression that is a number", before("  - action: set\n    field: title\n    value: =record.priority\n"), "must produce text, not int"},
		{"a value with a typo", before("  - action: set\n    field: priority\n    value: =record.prioity\n"), "value: column"},
		{"a value that is a list", before("  - action: set\n    field: priority\n    value: [1]\n"), "a value is text"},
		{"a value that is a decimal", before("  - action: set\n    field: priority\n    value: 1.5\n"), "a value is text"},
		{"resolution on another type", ruleWith("before", "task", "  - action: set\n    field: resolution\n    value: done\n"), "resolution belongs to incident records"},
		{"risk on an incident", ruleWith("before", "incident", "  - action: set\n    field: risk\n    value: high\n"), "risk belongs to change records"},
		{"a risk that is not a level", ruleWith("before", "change", "  - action: set\n    field: risk\n    value: extreme\n"), `risk must be one of "", "low", "medium", "high"`},
		{"comment.add in a before rule", before("  - action: comment.add\n    body: hi\n"), "only allowed in an after or async rule"},
		{"abort in an async rule", with("timing: before", "timing: async"), "abort is not allowed in an async rule"},
		{"set in an async rule", ruleWith("async", "incident", "  - action: set\n    field: priority\n    value: 1\n"), "only allowed in a before rule"},
		{"skipping errors in an async rule", ruleApp(map[string]string{"r.yaml": strings.Replace(asyncRule, "on: [update]", "on: [update]\non_error: skip", 1)}), "on_error: skip does not apply to an async rule"},
		{"too many async actions", ruleWith("async", "incident", strings.Repeat("  - action: comment.add\n    body: x\n", 101)), "async rule may have at most 100 actions"},
		{"record.update in a before rule", before("  - action: record.update\n    field: priority\n    value: 1\n"), "record.update saves a change of its own, so it is only allowed in an async rule"},
		{"record.update in an after rule", after("  - action: record.update\n    field: priority\n    value: 1\n"), "only allowed in an async rule"},
		{"record.update with no field", ruleWith("async", "incident", "  - action: record.update\n    value: 1\n"), "record.update needs a field"},
		{"record.update of a field that cannot be set", ruleWith("async", "incident", "  - action: record.update\n    field: status\n    value: closed\n"), "record.update cannot change \"status\""},
		{"record.update with a priority out of range", ruleWith("async", "incident", "  - action: record.update\n    field: priority\n    value: 9\n"), "priority must be 1 (critical) to 4 (low)"},
		{"record.update with a stray field", ruleWith("async", "incident", "  - action: record.update\n    field: priority\n    value: 1\n    body: x\n"), "record.update does not take body"},
		{"set in an async rule points at record.update", ruleWith("async", "incident", "  - action: set\n    field: priority\n    value: 1\n"), "an async rule uses record.update"},
		{"comment.add without a body", after("  - action: comment.add\n"), "comment.add needs a body"},
		{"comment.add with a body that is not text", after("  - action: comment.add\n    body: =record.priority\n"), "body: the expression must produce text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, p := compile(t, c.files)
			if p == nil {
				t.Fatal("was accepted")
			}
			if !strings.Contains(p.Error(), c.want) {
				t.Errorf("error %q does not mention %q", p.Error(), c.want)
			}
		})
	}
}

func TestAnAfterRuleMayAddCommentsAndAbort(t *testing.T) {
	src := "kind: rule\nname: ack\nrecord_type: task\ntiming: after\non: [create]\nrun:\n  - action: comment.add\n    body: =\"Thanks, \" + user.name\n    internal: true\n  - action: abort\n    message: stop\n    when: 'record.priority == 4'\n"
	c, p := compile(t, ruleApp(map[string]string{"ack.yaml": src}))
	if p != nil {
		t.Fatal(p)
	}
	if r := c.Definitions[0].Rule; !r.Applies(TimingAfter, EventCreate) || !r.Run[0].Internal || r.Run[1].Action != "abort" {
		t.Errorf("rule: %+v", r)
	}
}

// A comment rule reads the comment, and may tell the people it mentions.
func TestARuleMayRunWhenSomeoneComments(t *testing.T) {
	files := ruleApp(map[string]string{"c.yaml": "kind: rule\nname: on-comment\nrecord_type: task\ntiming: after\non: [comment]\n" +
		"when: '!comment.internal && size(comment.mentions) > 0 && comment.body.contains(\"urgent\")'\nrun:\n  - action: notify\n    template: heads-up\n    to: [mentioned, requester]\n"})
	files["default/notifications/heads-up.yaml"] = "kind: notification\nname: heads-up\ntitle: \"{{actor.name}} on {{record.reference}}\"\nbody: \"{{comment.body}}\"\n"
	c, p := compile(t, files)
	if p != nil {
		t.Fatal(p)
	}
	for _, d := range c.Definitions {
		if r := d.Rule; r != nil && (!r.Applies(TimingAfter, EventComment) || r.Applies(TimingAfter, EventUpdate)) {
			t.Errorf("rule: %+v", r)
		}
	}
}

// Rules are layered like everything else: a local file changes one value of a shipped rule and keeps the rest.
func TestALocalFileCanAdjustAShippedRule(t *testing.T) {
	b := layered(map[string]string{
		"app.yaml": goodManifest, "default/rules/r.yaml": goodRule,
		"local/rules/r.yaml": "order: 5\non_error: skip\n",
	}, nil)
	c, err := b.Compile()
	if err != nil {
		t.Fatal(err)
	}
	r := c.Definitions[0].Rule
	if r.Order != 5 || r.OnError != OnErrorSkip || r.Name != "critical-needs-assignee" || len(r.Run) != 1 {
		t.Errorf("merged rule: %+v", r)
	}
}

// A rule is stored as JSON and read back by the registry. A literal 1 must come back as a whole number: it once came
// back as 1.0, so the rule installed fine and was then dropped as invalid every time the registry loaded.
func TestRuleValuesSurviveTheStoredForm(t *testing.T) {
	src := "kind: rule\nname: rr\nrecord_type: incident\ntiming: before\non: [create]\nrun:\n  - action: set\n    field: priority\n    value: 1\n  - action: set\n    field: title\n    value: hello\n  - action: set\n    field: priority\n    value: =record.priority + 1\n"
	c, p := compile(t, ruleApp(map[string]string{"r.yaml": src}))
	if p != nil {
		t.Fatal(p)
	}
	body, err := json.Marshal(c.Definitions[0].Rule)
	if err != nil {
		t.Fatal(err)
	}
	var back Rule
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	if n, ok := back.Run[0].Value.Literal.(int64); !ok || n != 1 {
		t.Errorf("literal came back as %T %v", back.Run[0].Value.Literal, back.Run[0].Value.Literal)
	}
	if back.Run[1].Value.Literal != "hello" || back.Run[2].Value.Expr != "record.priority + 1" {
		t.Errorf("values: %+v %+v", back.Run[1].Value, back.Run[2].Value)
	}
	var problems Problems
	back.validate("stored", &problems)
	if problems.Error() != "" {
		t.Errorf("a stored rule no longer validates: %v", problems.Error())
	}
}

func TestAnAsyncRuleMayAddCommentsAndNothingElse(t *testing.T) {
	c, p := compile(t, ruleApp(map[string]string{"r.yaml": asyncRule}))
	if p != nil {
		t.Fatal(p)
	}
	r := c.Definitions[0].Rule
	if !r.Applies(TimingAsync, EventUpdate) || r.Applies(TimingAfter, EventUpdate) || r.OnError != OnErrorBlock {
		t.Errorf("rule: %+v", r)
	}
	// More than 20 actions is fine here: nothing waits for it.
	many := strings.Repeat("  - action: comment.add\n    body: x\n", 100)
	if _, p := compile(t, ruleWith("async", "incident", many)); p != nil {
		t.Errorf("100 actions in an async rule: %v", p)
	}
}

func TestAnAsyncRuleMayUpdateTheRecord(t *testing.T) {
	src := "kind: rule\nname: bump\nrecord_type: incident\ntiming: async\non: [create]\nrun:\n" +
		"  - action: record.update\n    field: priority\n    value: 2\n" +
		"  - action: record.update\n    field: title\n    value: '=record.title + \"!\"'\n    when: 'record.priority == 2'\n"
	c, p := compile(t, ruleApp(map[string]string{"bump.yaml": src}))
	if p != nil {
		t.Fatal(p)
	}
	if r := c.Definitions[0].Rule; len(r.Run) != 2 || r.Run[0].Action != "record.update" {
		t.Errorf("rule: %+v", r)
	}
}
