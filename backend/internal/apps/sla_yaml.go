package apps

import (
	"errors"

	"gopkg.in/yaml.v3"
)

// CalendarRulesOf is an SLA's calendar choice as a list of rules, however its file says it: a single calendar is one
// rule, and none counts every hour (24x7).
func (s *SLA) CalendarRulesOf() []CalendarRule {
	switch {
	case len(s.CalendarRules) > 0:
		return s.CalendarRules
	case s.Calendar != "":
		return []CalendarRule{{Use: s.Calendar}}
	}
	return []CalendarRule{{Use: CalendarAlways}}
}

// calendarNode is calendar: as a file says it: a calendar's name when that is all it is, else the list of rules.
func calendarNode(rules []CalendarRule) (*yaml.Node, error) {
	n := &yaml.Node{}
	if len(rules) == 1 && rules[0].When == "" && rules[0].Use != CalendarAlways && rules[0].Use != CalendarGroup {
		return n, n.Encode(rules[0].Use)
	}
	return n, n.Encode(rules)
}

// SLACalendarOverride is the studio override that makes an SLA count by rules: below is the SLA's file as the layers
// under the studio give it, current its studio override ("" for none). Only calendar: changes; anything else the
// override says is kept. Nil means no override is needed.
func SLACalendarOverride(below, current []byte, rules []CalendarRule) ([]byte, error) {
	want := below
	if len(current) > 0 {
		merged, err := mergeYAML(below, current)
		if err != nil {
			return nil, err
		}
		want = merged
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(want, &doc); err != nil {
		return nil, err
	}
	m := documentMap(&doc)
	if m == nil {
		return nil, errors.New("the SLA's file is not a YAML map")
	}
	cal, err := calendarNode(rules)
	if err != nil {
		return nil, err
	}
	set := false
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "calendar" {
			m.Content[i+1], set = cal, true
		}
	}
	if !set {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "calendar"}, cal)
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	return OverrideFor(below, out)
}
