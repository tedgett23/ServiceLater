package records

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/businesstime"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/slaclock"
)

// trackSLAs moves the record's SLA clocks to match the record as saved, in the save's transaction and as the person
// saving: a clock starts when its SLA's start condition first holds, pauses and stops by its conditions, and has its
// target worked out again. A warning or breach the save makes owed is queued with it.
//
// An SLA that cannot be worked out never blocks the save: the clock keeps its stage, says what is wrong (its
// problem), and the audit records it. Stopping someone's work because a timer is broken would be worse than the timer
// being wrong.
func (s *Service) trackSLAs(ctx context.Context, q *dbgen.Queries, a Actor, event string, cur *Ticket, saved Ticket) error {
	defs := s.Apps.SLAs(saved.Type)
	rows, err := q.ListTaskSLAsForUpdate(ctx, saved.ID)
	if err != nil || (len(defs) == 0 && len(rows) == 0) {
		return err
	}
	in, err := s.ruleInput(ctx, q, a, event, cur, saved)
	if err != nil {
		return err
	}
	now := in.Now
	cals := slaclock.NewCalendars(q)
	current := map[string]*apps.SLA{}
	for _, d := range defs {
		current[d.App+"/"+d.Name] = d
	}
	have := map[string]bool{}
	for _, row := range rows {
		c := slaclock.FromRow(row)
		have[c.App+"/"+c.SLA] = true
		before := c
		if current[c.App+"/"+c.SLA] == nil {
			// The app no longer defines it: the clock ends where it stands.
			c.MoveTo(slaclock.Cancelled, now, nil)
			c.DueAt, c.NextAlertAt, c.Problem = time.Time{}, time.Time{}, ""
		} else if def, err := apps.SLAFromSnapshot(c.Definition, c.Checksum, c.App); err != nil {
			c.Problem = err.Error()
		} else if err := s.stepClock(ctx, q, cals, def, &c, in, now); err != nil {
			return err
		}
		if !slaclock.Same(before, c) {
			if err := slaclock.Save(ctx, q, c); err != nil {
				return err
			}
		}
	}
	for _, def := range defs {
		if have[def.App+"/"+def.Name] {
			continue
		}
		start, _, _, err := def.Conditions()
		if err == nil {
			var ok bool
			ok, err = start.Eval(ctx, in)
			if err == nil && !ok {
				continue
			}
		}
		if err != nil {
			// A clock that cannot tell whether it starts is not started; the audit says why.
			s.slaProblem(ctx, def.App, def.Name, saved, fmt.Sprintf("start: %v", err))
			continue
		}
		c := slaclock.Clock{TaskID: saved.ID, App: def.App, SLA: def.Name, Definition: def.Body, Checksum: def.Checksum, StartedAt: now}
		if err := s.stepClock(ctx, q, cals, def, &c, in, now); err != nil {
			return err
		}
		if c.Stage == "" {
			continue // its conditions could not be worked out; the problem was recorded
		}
		if _, err := slaclock.Create(ctx, q, c); err != nil {
			return err
		}
	}
	return nil
}

// stepClock applies one save to one clock: alerts owed up to now are queued, then the clock moves to the stage its
// conditions give, with its target worked out again, and is scheduled. A new clock (no stage yet) starts here.
func (s *Service) stepClock(ctx context.Context, q *dbgen.Queries, cals *slaclock.Calendars, def *apps.SLA, c *slaclock.Clock, in expr.Input, now time.Time) error {
	cal, calProblem, err := cals.Get(ctx, def.Calendar)
	if err != nil {
		return err
	}
	warnings := warningShares(def)
	problem := func(msg string) error {
		c.Problem = msg
		s.slaProblem(ctx, def.App, def.Name, Ticket{ID: c.TaskID}, msg)
		return nil
	}
	stage, err := clockStage(ctx, def, in)
	if err != nil {
		return problem(err.Error())
	}
	target, err := clockTarget(ctx, def, in)
	if err != nil {
		return problem(err.Error())
	}
	if c.Stage != "" {
		fire, breach := c.Owed(now, cal, warnings)
		if len(fire) > 0 || breach {
			if c.ID == uuid.Nil {
				return errors.New("sla: a new clock cannot owe alerts")
			}
			if err := slaclock.Queue(ctx, q, c.ID, fire, breach); err != nil {
				return err
			}
		}
	}
	c.MoveTo(stage, now, cal)
	c.Target = target
	c.Problem = calProblem
	if err := c.Schedule(cal, warnings); err != nil {
		return problem(fmt.Sprintf("the due time cannot be worked out: %v", err))
	}
	if calProblem != "" {
		slog.Warn("sla calendar", "app", def.App, "sla", def.Name, "problem", calProblem)
	}
	return nil
}

func warningShares(def *apps.SLA) []int {
	out := make([]int, len(def.Warnings))
	for i, w := range def.Warnings {
		out[i] = w.At
	}
	return out
}

// clockStage is where the SLA's conditions put a clock: stopped, else paused, else running.
func clockStage(ctx context.Context, def *apps.SLA, in expr.Input) (string, error) {
	_, pause, stop, err := def.Conditions()
	if err != nil {
		return "", err
	}
	if done, err := stop.Eval(ctx, in); err != nil {
		return "", fmt.Errorf("stop: %w", err)
	} else if done {
		return slaclock.Stopped, nil
	}
	if waiting, err := pause.Eval(ctx, in); err != nil {
		return "", fmt.Errorf("pause: %w", err)
	} else if waiting {
		return slaclock.Paused, nil
	}
	return slaclock.Running, nil
}

// clockTarget is the SLA's target for the record as it is now.
func clockTarget(ctx context.Context, def *apps.SLA, in expr.Input) (time.Duration, error) {
	fixed, prog, err := def.TargetValue()
	if err != nil || prog == nil {
		return fixed, err
	}
	v, err := prog.EvalValue(ctx, in)
	if err != nil {
		return 0, fmt.Errorf("target: %w", err)
	}
	d, ok := v.(time.Duration)
	switch {
	case !ok:
		return 0, fmt.Errorf("target: produced %T, not a duration", v)
	case d <= 0 || d > apps.MaxSLATarget:
		return 0, fmt.Errorf("target: %v is not more than nothing and at most 366 days", d)
	}
	return d, nil
}

// slaProblem records an SLA that could not be worked out, on a connection of its own like every audit line.
func (s *Service) slaProblem(ctx context.Context, app, name string, t Ticket, msg string) {
	slog.Error("sla could not be worked out", "app", app, "sla", name, "record", t.ID, "problem", msg)
	ruleruns.Record(ctx, s.Pool, ruleruns.Run{App: app, Rule: name, Timing: ruleruns.TimingSLA, Event: "evaluate", RecordID: &t.ID,
		Outcome: ruleruns.Failed, Detail: msg})
}

// SLAClock is an SLA clock as people see it on its record.
type SLAClock struct {
	ID uuid.UUID `json:"id"`
	// App and Name say which SLA definition it runs under; Label is its name for people (Respond, Resolve).
	App   string `json:"app"`
	Name  string `json:"name"`
	Label string `json:"label"`
	// Stage is running, paused or stopped. A stopped clock was met, unless BreachedAt is set.
	Stage string `json:"stage"`
	// TargetMS is the working time allowed and UsedMS the working time used, as of the response.
	TargetMS int64 `json:"target_ms"`
	UsedMS   int64 `json:"used_ms"`
	// Calendar is the business calendar it counts by; empty means every hour counts.
	Calendar string `json:"calendar"`
	// DueAt is when the target is used up, while running.
	DueAt      *time.Time `json:"due_at"`
	BreachedAt *time.Time `json:"breached_at"`
	StartedAt  time.Time  `json:"started_at"`
	StoppedAt  *time.Time `json:"stopped_at"`
	// Problem says why the clock could not be worked out on the last save, or is empty.
	Problem string `json:"problem"`
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// LoadSLAs reads a record's clocks as q's caller may see them, with the time used worked out to now.
func LoadSLAs(ctx context.Context, q *dbgen.Queries, taskID uuid.UUID) ([]SLAClock, error) {
	rows, err := q.ListTaskSLAs(ctx, taskID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	cals := slaclock.NewCalendars(q)
	out := make([]SLAClock, 0, len(rows))
	for _, row := range rows {
		c := slaclock.FromRow(row)
		v := SLAClock{ID: c.ID, App: c.App, Name: c.SLA, Label: c.SLA, Stage: c.Stage, TargetMS: c.Target.Milliseconds(),
			DueAt: optTime(c.DueAt), BreachedAt: optTime(c.BreachedAt), StartedAt: c.StartedAt, StoppedAt: optTime(c.StoppedAt), Problem: c.Problem}
		var cal *businesstime.Calendar
		if def, err := apps.SLAFromSnapshot(c.Definition, c.Checksum, c.App); err == nil {
			v.Label, v.Calendar = def.Label, def.Calendar
			if cal, _, err = cals.Get(ctx, def.Calendar); err != nil {
				return nil, err
			}
		}
		v.UsedMS = c.Used(now, cal).Milliseconds()
		out = append(out, v)
	}
	return out, nil
}
