package apps

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Notification channels. In-app always; email when the instance's email settings turn it on and the person has not
// turned it off; SMS when SMS is on and the person verified a number and turned SMS on; Teams as one post to the
// channel of each group a notify action names, when Teams is on and the group has a channel (internal/notify).
const (
	ChannelInApp = "in_app"
	ChannelEmail = "email"
	ChannelSMS   = "sms"
	ChannelTeams = "teams"
)

// Channels are every channel a template may list.
var Channels = []string{ChannelInApp, ChannelEmail, ChannelSMS, ChannelTeams}

const (
	maxNotificationTitle = 200
	maxNotificationBody  = 2000
)

// Notification is a message template an app ships in notifications/: what a person is told, with placeholders
// filled from the record and the person who made the change. Which event sends it, and to whom, is a rule's
// `notify` action; the template only says what it reads like.
//
// A notification row stores which template and which record, never the text: the text is filled in when the
// recipient reads it, through the record's own RLS, so nothing about a ticket is copied where its policy does not
// reach (the same reason realtime events carry ids only).
type Notification struct {
	Name     string   `json:"name"`
	Title    string   `json:"title"`
	Body     string   `json:"body,omitempty"`
	Channels []string `json:"channels"`

	// App and Version say where the template came from. They are set at runtime and are not part of the definition.
	App     string `json:"-"`
	Version string `json:"-"`
}

// Placeholders a template may use: {{record.reference}} and so on. A fixed list, so a typo fails the install
// instead of reaching someone as a blank.
var Placeholders = []string{
	"record.reference", "record.title", "record.type", "record.status", "record.priority",
	"record.requester_name", "record.assignee_name", "record.assignment_group",
	"actor.name",
}

var placeholderRE = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

type notificationFile struct {
	Kind     string   `yaml:"kind"`
	Name     string   `yaml:"name"`
	Title    string   `yaml:"title"`
	Body     string   `yaml:"body"`
	Channels []string `yaml:"channels"`
}

func parseNotification(file string, data []byte, p *Problems) *Notification {
	var f notificationFile
	if err := decodeStrict(data, &f); err != nil {
		p.add(file, "%v", err)
		return nil
	}
	if f.Kind != "notification" {
		p.add(file, "kind must be notification in this folder, got %q", f.Kind)
		return nil
	}
	n := &Notification{Name: f.Name, Title: f.Title, Body: f.Body, Channels: f.Channels}
	if len(n.Channels) == 0 {
		n.Channels = []string{ChannelInApp}
	}
	before := len(*p)
	n.validateStructure(file, p)
	if len(*p) > before {
		return nil
	}
	return n
}

func (n *Notification) validateStructure(file string, p *Problems) {
	if !ruleNameRE.MatchString(n.Name) {
		p.add(file, "name %q must be 2-63 characters: lowercase letters, digits and hyphens, starting with a letter", n.Name)
	}
	switch {
	case strings.TrimSpace(n.Title) == "":
		p.add(file, "title is required: the line the person sees")
	case utf8.RuneCountInString(n.Title) > maxNotificationTitle:
		p.add(file, "title must be at most %d characters", maxNotificationTitle)
	}
	if utf8.RuneCountInString(n.Body) > maxNotificationBody {
		p.add(file, "body must be at most %d characters", maxNotificationBody)
	}
	for _, field := range []struct{ name, text string }{{"title", n.Title}, {"body", n.Body}} {
		for _, msg := range templateProblems(field.text) {
			p.add(file, "%s: %s", field.name, msg)
		}
	}
	seen := map[string]bool{}
	for _, c := range n.Channels {
		switch {
		case c == "slack":
			p.add(file, "channels: slack is not supported yet (%s are)", strings.Join(Channels, ", "))
		case !slices.Contains(Channels, c):
			p.add(file, "channels: %q is not a channel (%s)", c, strings.Join(Channels, ", "))
		case seen[c]:
			p.add(file, "channels: %q is listed twice", c)
		}
		seen[c] = true
	}
	if !seen[ChannelInApp] {
		p.add(file, "channels: must include in_app: an email is a copy of a notification in the app")
	}
}

// templateProblems lists unknown placeholders, and braces that do not form one.
func templateProblems(s string) (out []string) {
	for _, m := range placeholderRE.FindAllStringSubmatch(s, -1) {
		if !slices.Contains(Placeholders, m[1]) {
			out = append(out, "{{"+m[1]+"}} is not a placeholder (available: "+strings.Join(Placeholders, ", ")+")")
		}
	}
	if rest := placeholderRE.ReplaceAllString(s, ""); strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		out = append(out, "has {{ or }} that is not a placeholder")
	}
	return out
}

// Render fills the placeholders. A value the record does not have (no assignee yet) reads as empty.
func (n *Notification) Render(values map[string]string) (title, body string) {
	fill := func(s string) string {
		return placeholderRE.ReplaceAllStringFunc(s, func(m string) string {
			return values[placeholderRE.FindStringSubmatch(m)[1]]
		})
	}
	return truncate(fill(n.Title), maxNotificationTitle), truncate(fill(n.Body), maxNotificationBody)
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// checkNotifyTemplates checks what only the whole app can tell: that every template a rule's notify action names is
// defined in the same app.
func checkNotifyTemplates(defs []Definition, p *Problems) {
	templates := map[string]bool{}
	for _, d := range defs {
		if d.Notification != nil {
			templates[d.Notification.Name] = true
		}
	}
	for _, d := range defs {
		for _, lr := range d.actionRules() {
			checkRuleTemplates(d.File, lr, templates, p)
		}
	}
}

func checkRuleTemplates(file string, lr locatedRule, templates map[string]bool, p *Problems) {
	for i, a := range lr.rule.Run {
		if a.Action == "notify" && a.Template != "" && !templates[a.Template] {
			names := make([]string, 0, len(templates))
			for n := range templates {
				names = append(names, n)
			}
			slices.Sort(names)
			list := strings.Join(names, ", ")
			if list == "" {
				list = "none"
			}
			p.add(file, "%srun[%d]: there is no notification template %q in this app (templates: %s)", lr.at, i, a.Template, list)
		}
	}
}
