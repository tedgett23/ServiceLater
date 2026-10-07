package apps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/servicelater/servicelater/backend/internal/expr"
)

// An SLA's conditions when its definition leaves them out: the clock starts with the record, stands still while the
// record waits on someone else, and stops when it is done.
const (
	DefaultSLAStart = "true"
	DefaultSLAPause = `record.state_category == "waiting"`
	DefaultSLAStop  = `record.state_category in ["done", "closed"]`

	// MaxSLATarget is the longest target an SLA may have.
	MaxSLATarget = 366 * 24 * time.Hour

	maxSLAWarnings = 3
	maxSLALabel    = 100
)

// SLAActions are what an SLA's warnings and breach may do. They run after the fact, as the app, with nobody's save
// to change or stop; a workflow they start can do the rest (reassign, escalate).
var SLAActions = []string{"notify", "workflow.start", "comment.add"}

// SLA is a service level agreement an app ships in slas/ (docs/apps-platform.md §6.6): a clock that starts on a
// record, pauses and stops by conditions on it, counts working time by a business calendar, and acts when a share
// of its target is used (warnings) and when all of it is (the breach).
//
// The clocks are rows (task_slas). A save moves them (internal/records), so a pause begins exactly when the record
// changes; a timer in the worker finds the ones that fall due and queues their actions (internal/slatimer).
type SLA struct {
	Name       string `json:"name"`
	Label      string `json:"label"`
	RecordType string `json:"record_type"`
	// Start, Pause and Stop are conditions on the record (the rule environment: record, old, user, now, event).
	// A clock attaches when Start holds; it counts while neither Stop nor Pause does.
	Start string `json:"start"`
	Pause string `json:"pause"`
	Stop  string `json:"stop"`
	// Target is how much working time the record has: a literal such as "4h" or "2d", or an =expression giving a
	// duration, so it can depend on the priority. It is worked out again on every save, keeping the time used.
	Target *Value `json:"target"`
	// Calendar names a business calendar (business_calendars); empty counts every hour.
	Calendar string       `json:"calendar,omitempty"`
	Warnings []SLAWarning `json:"warnings,omitempty"`
	OnBreach []Action     `json:"on_breach,omitempty"`

	// App and Version say where the SLA came from. They are set at runtime and are not part of the definition.
	App     string `json:"-"`
	Version string `json:"-"`
	// Checksum names the definition as stored, so a clock knows which one it started with.
	Checksum string `json:"-"`
	// Body is the definition as stored, which a new clock keeps.
	Body json.RawMessage `json:"-"`

	start, pause, stop          *expr.Program
	startErr, pauseErr, stopErr error
	warnRules                   []*Rule
	breachRule                  *Rule
}

// SLAWarning is what to do when At percent of the target is used.
type SLAWarning struct {
	At  int      `json:"at"`
	Run []Action `json:"run"`
}

type slaFile struct {
	Kind       string           `yaml:"kind"`
	Name       string           `yaml:"name"`
	Label      string           `yaml:"label"`
	RecordType string           `yaml:"record_type"`
	Start      string           `yaml:"start"`
	Pause      string           `yaml:"pause"`
	Stop       string           `yaml:"stop"`
	Target     *Value           `yaml:"target"`
	Calendar   string           `yaml:"calendar"`
	Warnings   []slaWarningFile `yaml:"warnings"`
	OnBreach   []actionFile     `yaml:"on_breach"`
}

type slaWarningFile struct {
	At  int          `yaml:"at"`
	Run []actionFile `yaml:"run"`
}

func actionsFrom(files []actionFile) []Action {
	var out []Action
	for _, a := range files {
		out = append(out, Action{Action: a.Action, When: a.When, Field: a.Field, Value: a.Value, Message: a.Message, Body: a.Body,
			Internal: a.Internal, Workflow: a.Workflow, Template: a.Template, To: a.To})
	}
	return out
}

func parseSLA(file string, data []byte, p *Problems) *SLA {
	var f slaFile
	if err := decodeStrict(data, &f); err != nil {
		p.add(file, "%v", err)
		return nil
	}
	if f.Kind != "sla" {
		p.add(file, "kind must be sla in this folder, got %q", f.Kind)
		return nil
	}
	s := &SLA{Name: f.Name, Label: f.Label, RecordType: f.RecordType, Start: f.Start, Pause: f.Pause, Stop: f.Stop,
		Target: f.Target, Calendar: f.Calendar, OnBreach: actionsFrom(f.OnBreach)}
	if s.Start == "" {
		s.Start = DefaultSLAStart
	}
	if s.Pause == "" {
		s.Pause = DefaultSLAPause
	}
	if s.Stop == "" {
		s.Stop = DefaultSLAStop
	}
	for _, w := range f.Warnings {
		s.Warnings = append(s.Warnings, SLAWarning{At: w.At, Run: actionsFrom(w.Run)})
	}
	before := len(*p)
	s.validate(file, p)
	if len(*p) > before {
		return nil
	}
	return s
}

// validate checks the SLA's structure and compiles every expression in it, as an install does.
func (s *SLA) validate(file string, p *Problems) {
	before := len(*p)
	s.validateStructure(file, p)
	if len(*p) > before {
		return
	}
	s.compile()
	for _, c := range []struct {
		name string
		prog *expr.Program
		err  error
	}{{"start", s.start, s.startErr}, {"pause", s.pause, s.pauseErr}, {"stop", s.stop, s.stopErr}} {
		if c.err != nil {
			p.add(file, "%s: %v", c.name, c.err)
		} else if names := Lookups(nil).undeclared(c.prog); len(names) > 0 {
			p.add(file, "%s: an SLA cannot read lookups", c.name)
		}
	}
	if s.Target.err != nil {
		p.add(file, "target: %v", s.Target.err)
	} else if prog := s.Target.program; prog != nil && len(Lookups(nil).undeclared(prog)) > 0 {
		p.add(file, "target: an SLA cannot read lookups")
	}
	// The actions are checked as an async rule's would be, since that is how they run.
	for i, r := range s.warnRules {
		var sub Problems
		r.validate("", &sub)
		for _, x := range sub {
			p.add(file, "warnings[%d]: %s", i, x.Message)
		}
	}
	if s.breachRule != nil {
		var sub Problems
		s.breachRule.validate("", &sub)
		for _, x := range sub {
			p.add(file, "on_breach: %s", x.Message)
		}
	}
}

var durationDaysRE = regexp.MustCompile(`^(\d+)d(.*)$`)

// ParseTarget reads a literal target: a Go duration ("4h", "90m", "1h30m") optionally led by whole days ("2d",
// "1d12h").
func ParseTarget(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var days time.Duration
	if m := durationDaysRE.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n > 366 {
			return 0, fmt.Errorf("%q is too many days", m[1])
		}
		days, s = time.Duration(n)*24*time.Hour, m[2]
	}
	var rest time.Duration
	if s != "" {
		var err error
		if rest, err = time.ParseDuration(s); err != nil {
			return 0, errors.New("write a target such as 4h, 90m, 2d or 1d12h")
		}
	}
	return days + rest, nil
}

func (s *SLA) validateStructure(file string, p *Problems) {
	if !ruleNameRE.MatchString(s.Name) {
		p.add(file, "name %q must be 2-63 characters: lowercase letters, digits and hyphens, starting with a letter", s.Name)
	}
	switch {
	case strings.TrimSpace(s.Label) == "":
		p.add(file, "label is required: what people see next to the clock, such as Resolve or Respond")
	case utf8.RuneCountInString(s.Label) > maxSLALabel:
		p.add(file, "label must be at most %d characters", maxSLALabel)
	}
	if !slices.Contains(KnownRecordTypes, s.RecordType) {
		p.add(file, "record_type %q is not a record type this version provides (available: %v)", s.RecordType, KnownRecordTypes)
	}
	switch {
	case s.Target == nil:
		p.add(file, "target is required: a duration such as 4h or 2d, or an =expression giving one")
	case s.Target.Expr == "":
		text, ok := s.Target.Literal.(string)
		if !ok {
			p.add(file, "target is a duration such as 4h or 2d (a bare number has no unit)")
			break
		}
		d, err := ParseTarget(text)
		switch {
		case err != nil:
			p.add(file, "target: %v", err)
		case d <= 0 || d > MaxSLATarget:
			p.add(file, "target must be more than nothing and at most 366 days")
		}
	}
	if s.Calendar != "" && !ruleNameRE.MatchString(s.Calendar) {
		p.add(file, "calendar %q is not a calendar name (lowercase letters, digits and hyphens)", s.Calendar)
	}
	if len(s.Warnings) > maxSLAWarnings {
		p.add(file, "an SLA may have at most %d warnings", maxSLAWarnings)
	}
	for i, w := range s.Warnings {
		switch {
		case w.At < 1 || w.At > 99:
			p.add(file, "warnings[%d]: at is the percentage of the target used, from 1 to 99", i)
		case i > 0 && w.At <= s.Warnings[i-1].At:
			p.add(file, "warnings[%d]: list warnings from the lowest percentage up, each once", i)
		}
		if len(w.Run) == 0 {
			p.add(file, "warnings[%d]: run is required: what to do (%s)", i, strings.Join(SLAActions, ", "))
		}
		slaActionProblems(file, fmt.Sprintf("warnings[%d]", i), w.Run, p)
	}
	slaActionProblems(file, "on_breach", s.OnBreach, p)
}

func slaActionProblems(file, at string, actions []Action, p *Problems) {
	for j, a := range actions {
		if a.Action != "" && !slices.Contains(SLAActions, a.Action) {
			p.add(file, "%s: run[%d]: an SLA can %s, not %s", at, j, strings.Join(SLAActions, ", "), a.Action)
		}
	}
}

// stageRule is an SLA's warning or breach as the async rule it runs as: the same actions, checked and run the same way.
func (s *SLA) stageRule(actions []Action) *Rule {
	if len(actions) == 0 {
		return nil
	}
	return &Rule{Name: s.Name, RecordType: s.RecordType, Timing: TimingAsync, On: []string{EventUpdate}, Order: defaultOrder,
		OnError: OnErrorBlock, Run: slices.Clone(actions), App: s.App, Version: s.Version}
}

// compile compiles every expression. Anything that fails is remembered, and the clock reports it as its problem.
func (s *SLA) compile() {
	env, err := expr.Rule()
	compile := func(src string) (*expr.Program, error) {
		if err != nil {
			return nil, err
		}
		return env.Compile(src)
	}
	s.start, s.startErr = compile(s.Start)
	s.pause, s.pauseErr = compile(s.Pause)
	s.stop, s.stopErr = compile(s.Stop)
	s.Target.compile(env, err, expr.Duration)
	s.warnRules = nil
	for _, w := range s.Warnings {
		r := s.stageRule(w.Run)
		if r == nil {
			r = &Rule{Name: s.Name}
		}
		r.compile()
		s.warnRules = append(s.warnRules, r)
	}
	s.breachRule = s.stageRule(s.OnBreach)
	if s.breachRule != nil {
		s.breachRule.compile()
	}
}

// Conditions returns the compiled start, pause and stop conditions, and the first reason one cannot be evaluated.
func (s *SLA) Conditions() (start, pause, stop *expr.Program, err error) {
	return s.start, s.pause, s.stop, errors.Join(s.startErr, s.pauseErr, s.stopErr)
}

// TargetValue returns the compiled target: a fixed duration, or the expression to work it out from.
func (s *SLA) TargetValue() (fixed time.Duration, prog *expr.Program, err error) {
	if s.Target.Expr == "" {
		text, _ := s.Target.Literal.(string)
		d, err := ParseTarget(text)
		return d, nil, err
	}
	return 0, s.Target.program, s.Target.err
}

// WarningRule is the i-th warning's actions as a rule, or nil.
func (s *SLA) WarningRule(i int) *Rule {
	if i < 0 || i >= len(s.warnRules) || len(s.Warnings[i].Run) == 0 {
		return nil
	}
	return s.warnRules[i]
}

// BreachRule is the breach's actions as a rule, or nil when the SLA does nothing on a breach but show it.
func (s *SLA) BreachRule() *Rule { return s.breachRule }

// actionRules lists the rules an SLA's warnings and breach run as, with where each is in the file.
func (s *SLA) actionRules() (out []locatedRule) {
	for i, w := range s.Warnings {
		if r := s.stageRule(w.Run); r != nil {
			out = append(out, locatedRule{fmt.Sprintf("warnings[%d]: ", i), r})
		}
	}
	if r := s.stageRule(s.OnBreach); r != nil {
		out = append(out, locatedRule{"on_breach: ", r})
	}
	return out
}

var snapshots sync.Map // checksum -> *SLA

// SLAFromSnapshot compiles the definition a clock started with. Compiled definitions are kept by checksum, since many
// clocks share one.
func SLAFromSnapshot(body json.RawMessage, checksum, app string) (*SLA, error) {
	if s, ok := snapshots.Load(checksum); ok {
		return s.(*SLA), nil
	}
	var s SLA
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("the SLA definition is unreadable: %w", err)
	}
	var p Problems
	if s.validateStructure("", &p); len(p) > 0 {
		return nil, fmt.Errorf("the SLA definition is invalid: %s", p.Error())
	}
	s.App, s.Checksum, s.Body = app, checksum, body
	s.compile()
	snapshots.Store(checksum, &s)
	return &s, nil
}

// Checksum is how a stored definition is named: the same as compileDefinition's.
func Checksum(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
