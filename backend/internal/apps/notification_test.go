package apps

import (
	"strings"
	"testing"
)

const goodTemplate = `kind: notification
name: assigned
title: "{{record.reference}} was assigned to you"
body: "{{ actor.name }} gave you {{record.title}}"
`

const notifyRule = `kind: rule
name: tell-assignee
record_type: task
timing: after
on: [create, update]
when: has(record.assignee_id)
run:
  - action: notify
    template: assigned
    to: [assignee, assignment_group, "=[record.requester_id]"]
`

func notifyApp(templates, rules map[string]string) map[string]string {
	out := map[string]string{"app.yaml": goodManifest}
	for name, content := range templates {
		out["default/notifications/"+name] = content
	}
	for name, content := range rules {
		out["default/rules/"+name] = content
	}
	return out
}

func TestATemplateAndANotifyRuleCompile(t *testing.T) {
	c, p := compile(t, notifyApp(map[string]string{"assigned.yaml": goodTemplate}, map[string]string{"tell.yaml": notifyRule}))
	if p != nil {
		t.Fatal(p)
	}
	var n *Notification
	var r *Rule
	for _, d := range c.Definitions {
		if d.Notification != nil {
			n = d.Notification
		}
		if d.Rule != nil {
			r = d.Rule
		}
	}
	if n == nil || n.Name != "assigned" || len(n.Channels) != 1 || n.Channels[0] != ChannelInApp {
		t.Fatalf("template: %+v", n)
	}
	title, body := n.Render(map[string]string{"record.reference": "INC7", "record.title": "VPN", "actor.name": "Priya"})
	if title != "INC7 was assigned to you" || body != "Priya gave you VPN" {
		t.Errorf("rendered %q / %q", title, body)
	}
	a := r.Run[0]
	if a.Template != "assigned" || len(a.To) != 3 {
		t.Fatalf("action: %+v", a)
	}
	if p, err := a.Recipient(0); p != nil || err != nil {
		t.Errorf("a named recipient has no expression: %v %v", p, err)
	}
	if p, err := a.Recipient(2); p == nil || err != nil {
		t.Errorf("an =expression compiles: %v %v", p, err)
	}
}

func TestTemplateMistakesFailTheInstall(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"unknown placeholder", "kind: notification\nname: a\ntitle: \"{{record.secret}}\"\n", "{{record.secret}} is not a placeholder"},
		{"stray braces", "kind: notification\nname: a\ntitle: \"hi {{ there\"\n", "not a placeholder"},
		{"no title", "kind: notification\nname: a\nbody: x\n", "title is required"},
		{"email", "kind: notification\nname: a\ntitle: x\nchannels: [email]\n", "email is not supported yet"},
		{"unknown channel", "kind: notification\nname: a\ntitle: x\nchannels: [pigeon]\n", `"pigeon" is not a channel`},
		{"bad name", "kind: notification\nname: A\ntitle: x\n", "name \"A\""},
		{"wrong kind", "kind: rule\nname: a\ntitle: x\n", "kind must be notification"},
		{"unknown field", "kind: notification\nname: a\ntitle: x\nto: [assignee]\n", "to"},
		{"long title", "kind: notification\nname: a\ntitle: \"" + strings.Repeat("x", 201) + "\"\n", "at most 200"},
	}
	for _, c := range cases {
		_, p := compile(t, notifyApp(map[string]string{"a.yaml": c.src}, nil))
		if p == nil || !strings.Contains(p.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, p)
		}
	}
}

func TestNotifyActionMistakesFailTheInstall(t *testing.T) {
	rule := func(timing, action string) string {
		return "kind: rule\nname: rr\nrecord_type: task\ntiming: " + timing + "\non: [update]\nrun:\n" + action
	}
	tpl := map[string]string{"assigned.yaml": goodTemplate}
	cases := []struct{ name, src, want string }{
		{"before", rule("before", "  - action: notify\n    template: assigned\n    to: [assignee]\n"), "only allowed in an after or async rule"},
		{"no template", rule("after", "  - action: notify\n    to: [assignee]\n"), "notify needs template"},
		{"missing template", rule("after", "  - action: notify\n    template: nope\n    to: [assignee]\n"), `no notification template "nope" in this app (templates: assigned)`},
		{"no recipients", rule("after", "  - action: notify\n    template: assigned\n"), "notify needs to"},
		{"unknown recipient", rule("after", "  - action: notify\n    template: assigned\n    to: [everyone]\n"), `"everyone" is not a recipient`},
		{"not a list", rule("after", "  - action: notify\n    template: assigned\n    to: [\"=record.requester_id\"]\n"), "to[0]"},
		{"bad expression", rule("after", "  - action: notify\n    template: assigned\n    to: [\"=[record.nope]\"]\n"), "to[0]"},
		{"to on another action", rule("after", "  - action: comment.add\n    body: hi\n    to: [assignee]\n"), "comment.add does not take to"},
	}
	for _, c := range cases {
		_, p := compile(t, notifyApp(tpl, map[string]string{"r.yaml": c.src}))
		if p == nil || !strings.Contains(p.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, p)
		}
	}
}

// An operator rewords a shipped template from the instance's local layer, and keeps everything else.
func TestLocalRewordsATemplate(t *testing.T) {
	b := layered(notifyApp(map[string]string{"assigned.yaml": goodTemplate}, nil),
		map[string]string{"notifications/assigned.yaml": "title: \"New for you: {{record.reference}}\"\n"})
	c, err := b.Compile()
	if err != nil {
		t.Fatal(err)
	}
	n := c.Definitions[0].Notification
	if n.Title != "New for you: {{record.reference}}" || n.Body != "{{ actor.name }} gave you {{record.title}}" {
		t.Errorf("merged: %+v", n)
	}
	if got := c.Definitions[0].Source(); got != "default+instance" {
		t.Errorf("layers: %s", got)
	}
}

func TestRenderTruncatesAndBlanksMissingValues(t *testing.T) {
	n := &Notification{Title: "{{record.assignee_name}}" + strings.Repeat("x", 250)}
	title, _ := n.Render(map[string]string{})
	if len([]rune(title)) != maxNotificationTitle || !strings.HasSuffix(title, "…") || strings.Contains(title, "{{") {
		t.Errorf("title: %q", title)
	}
}
