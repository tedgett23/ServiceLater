package apps

import (
	"context"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

const resolveSLA = `kind: sla
name: resolve
label: Resolve
record_type: incident
target: 8h
calendar: business-hours
`

func compileSLA(t *testing.T, src string) *SLA {
	t.Helper()
	var p Problems
	s := parseSLA("slas/resolve.yaml", []byte(src), formSet{}, &p)
	if len(p) > 0 || s == nil {
		t.Fatalf("problems: %v", p)
	}
	return s
}

// An SLA's calendar may follow the record: the first rule that holds decides, "group" is the assignment group's own
// hours (skipped when it has none), and 24x7 counts every hour.
func TestSLACalendarRulesChooseByTheRecord(t *testing.T) {
	s := compileSLA(t, strings.Replace(resolveSLA, "calendar: business-hours", `calendar:
  - when: record.priority == 1
    use: 24x7
  - when: 'has(record.assignment_group) && record.assignment_group == "Network"'
    use: extended-hours
  - use: group
  - use: business-hours`, 1))
	group := "London"
	groupCal := func() (string, error) {
		if group == "" {
			return "", nil
		}
		return "london-office", nil
	}
	net := "Network"
	for _, c := range []struct {
		rec  expr.Record
		grp  string
		want string
	}{
		{expr.Record{Priority: 1}, "London", ""},
		{expr.Record{Priority: 3, AssignmentGroup: &net}, "London", "extended-hours"},
		{expr.Record{Priority: 3}, "London", "london-office"},
		{expr.Record{Priority: 3}, "", "business-hours"},
	} {
		group = c.grp
		rec := c.rec
		got, err := s.CalendarFor(context.Background(), expr.Input{Record: &rec}, groupCal)
		if err != nil || got != c.want {
			t.Errorf("%+v in %q: %q %v, want %q", c.rec, c.grp, got, err, c.want)
		}
	}
	if rules := s.CalendarRulesOf(); len(rules) != 4 || rules[2].Use != CalendarGroup {
		t.Errorf("rules: %+v", rules)
	}
	if one := compileSLA(t, resolveSLA).CalendarRulesOf(); len(one) != 1 || one[0].Use != "business-hours" {
		t.Errorf("a single calendar is one rule: %+v", one)
	}
	if none := compileSLA(t, strings.Replace(resolveSLA, "calendar: business-hours\n", "", 1)).CalendarRulesOf(); none[0].Use != CalendarAlways {
		t.Errorf("no calendar is every hour: %+v", none)
	}
}

func TestSLACalendarRulesAreChecked(t *testing.T) {
	for src, want := range map[string]string{
		"calendar:\n  - use: Business Hours\n":                  "use must be 24x7, group, or a calendar's name",
		"calendar:\n  - when: record.nonsense\n    use: 24x7\n": "calendar[0]: when",
		"calendar:\n  - when: record.priority\n    use: 24x7\n": "calendar[0]: when",
		"calendar:\n  - use: 24x7\n    because: reasons\n":      "because",
		"calendar: {use: 24x7}\n":                               "calendar is a calendar's name, or a list of rules",
	} {
		var p Problems
		s := parseSLA("slas/resolve.yaml", []byte(strings.Replace(resolveSLA, "calendar: business-hours\n", src, 1)), formSet{}, &p)
		if s != nil && len(p) == 0 {
			s.validate("slas/resolve.yaml", &p)
		}
		if !strings.Contains(p.Error(), want) {
			t.Errorf("%q: %v, want %q", src, p, want)
		}
	}
}

// The editor's override says only the calendar, and keeps whatever else an override already said.
func TestSLACalendarOverride(t *testing.T) {
	out, err := SLACalendarOverride([]byte(resolveSLA), nil, []CalendarRule{{When: "record.priority == 1", Use: "24x7"}, {Use: "business-hours"}})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); strings.Contains(s, "target") || !strings.Contains(s, "use: 24x7") || !strings.Contains(s, "record.priority == 1") {
		t.Errorf("override: %s", s)
	}
	merged, err := mergeYAML([]byte(resolveSLA), out)
	if err != nil {
		t.Fatal(err)
	}
	if s := compileSLA(t, string(merged)); len(s.CalendarRules) != 2 || s.Target.Literal != "8h" {
		t.Errorf("merged: %+v", s)
	}
	kept, err := SLACalendarOverride([]byte(resolveSLA), []byte("label: Fix\n"), []CalendarRule{{Use: "extended-hours"}})
	if err != nil || !strings.Contains(string(kept), "label: Fix") || !strings.Contains(string(kept), "calendar: extended-hours") {
		t.Errorf("an override's other changes are kept: %s %v", kept, err)
	}
	if same, err := SLACalendarOverride([]byte(resolveSLA), nil, []CalendarRule{{Use: "business-hours"}}); err != nil || same != nil {
		t.Errorf("the shipped calendar needs no override: %s %v", same, err)
	}
}
