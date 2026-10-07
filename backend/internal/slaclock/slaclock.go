// Package slaclock is an SLA clock's state and arithmetic (docs/apps-platform.md §6.6): how much working time it has
// used, when it falls due, and which warnings and breach are owed. The save path (internal/records) moves clocks as
// records change; the timer (internal/slatimer) finds the ones whose time has come. Both use this package, which sits
// below them both and does no work of its own beyond reading a calendar and queuing an alert.
package slaclock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	// Calendars name IANA zones; the binary carries the zone database, so it works on images without one.
	_ "time/tzdata"

	"github.com/servicelater/servicelater/backend/internal/businesstime"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/jobs"
)

// Stages of a clock.
const (
	Running   = "running"
	Paused    = "paused"
	Stopped   = "stopped"
	Cancelled = "cancelled"
)

// AlertKind is the job kind a clock's warnings and breaches are queued under.
const AlertKind = "sla.alert"

// Alert is the job payload: which clock, and which of its alerts. Warning is the warning's index, or -1 for the breach.
type Alert struct {
	ClockID uuid.UUID `json:"clock_id"`
	Warning int       `json:"warning"`
}

// Breach is Alert.Warning for a breach.
const Breach = -1

// Clock is one task_slas row. A zero time is unset.
type Clock struct {
	ID         uuid.UUID
	TaskID     uuid.UUID
	App, SLA   string
	Definition json.RawMessage
	Checksum   string

	Stage           string
	Target, Elapsed time.Duration
	RunningSince    time.Time
	DueAt           time.Time
	BreachedAt      time.Time
	WarningsSent    int
	NextAlertAt     time.Time
	Problem         string
	StartedAt       time.Time
	StoppedAt       time.Time
}

func ts(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

func pgts(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// FromRow reads a clock from its row.
func FromRow(r dbgen.TaskSla) Clock {
	return Clock{ID: r.ID, TaskID: r.TaskID, App: r.App, SLA: r.Sla, Definition: r.Definition, Checksum: r.Checksum,
		Stage: r.Stage, Target: time.Duration(r.TargetMs) * time.Millisecond, Elapsed: time.Duration(r.ElapsedMs) * time.Millisecond,
		RunningSince: ts(r.RunningSince), DueAt: ts(r.DueAt), BreachedAt: ts(r.BreachedAt), WarningsSent: int(r.WarningsSent),
		NextAlertAt: ts(r.NextAlertAt), Problem: r.Problem, StartedAt: ts(r.StartedAt), StoppedAt: ts(r.StoppedAt)}
}

// Create writes a new clock and returns its id.
func Create(ctx context.Context, q *dbgen.Queries, c Clock) (uuid.UUID, error) {
	return q.CreateTaskSLA(ctx, dbgen.CreateTaskSLAParams{TaskID: c.TaskID, App: c.App, Sla: c.SLA, Definition: c.Definition,
		Checksum: c.Checksum, Stage: c.Stage, TargetMs: c.Target.Milliseconds(), ElapsedMs: c.Elapsed.Milliseconds(),
		RunningSince: pgts(c.RunningSince), DueAt: pgts(c.DueAt), BreachedAt: pgts(c.BreachedAt),
		WarningsSent: int32(c.WarningsSent), NextAlertAt: pgts(c.NextAlertAt), Problem: c.Problem, //nolint:gosec // at most 3 warnings
		StartedAt: pgts(c.StartedAt), StoppedAt: pgts(c.StoppedAt)})
}

// Save writes a clock's state back.
func Save(ctx context.Context, q *dbgen.Queries, c Clock) error {
	return q.UpdateTaskSLA(ctx, dbgen.UpdateTaskSLAParams{ID: c.ID, Stage: c.Stage, TargetMs: c.Target.Milliseconds(),
		ElapsedMs: c.Elapsed.Milliseconds(), RunningSince: pgts(c.RunningSince), DueAt: pgts(c.DueAt), BreachedAt: pgts(c.BreachedAt),
		WarningsSent: int32(c.WarningsSent), NextAlertAt: pgts(c.NextAlertAt), Problem: c.Problem, //nolint:gosec // at most 3 warnings
		StoppedAt: pgts(c.StoppedAt)})
}

// Same reports whether two states would write the same row.
func Same(a, b Clock) bool {
	eq := func(x, y time.Time) bool { return x.Equal(y) }
	return a.Stage == b.Stage && a.Target.Milliseconds() == b.Target.Milliseconds() && a.Elapsed.Milliseconds() == b.Elapsed.Milliseconds() &&
		eq(a.RunningSince, b.RunningSince) && eq(a.DueAt, b.DueAt) && eq(a.BreachedAt, b.BreachedAt) &&
		a.WarningsSent == b.WarningsSent && eq(a.NextAlertAt, b.NextAlertAt) && a.Problem == b.Problem && eq(a.StoppedAt, b.StoppedAt)
}

// Used is the working time the clock has used by now.
func (c *Clock) Used(now time.Time, cal *businesstime.Calendar) time.Duration {
	if c.Stage == Running {
		return c.Elapsed + cal.Elapsed(c.RunningSince, now)
	}
	return c.Elapsed
}

// MoveTo puts the clock in a stage at now, banking the working time used so far when it stops running.
func (c *Clock) MoveTo(stage string, now time.Time, cal *businesstime.Calendar) {
	if c.Stage == stage {
		return
	}
	if c.Stage == Running {
		c.Elapsed = c.Used(now, cal)
		c.RunningSince = time.Time{}
	}
	c.Stage = stage
	switch stage {
	case Running:
		c.RunningSince, c.StoppedAt = now, time.Time{}
	case Stopped, Cancelled:
		c.StoppedAt = now
	default:
		c.StoppedAt = time.Time{}
	}
}

// share is pct percent of the target.
func (c *Clock) share(pct int) time.Duration { return c.Target * time.Duration(pct) / 100 }

// Owed works out which alerts a running clock owes by now, and records them as sent: the warnings whose share of the
// target is used (by index), and whether it has just breached. Warnings overtaken by the breach are counted as sent
// but not returned, since the breach says more.
func (c *Clock) Owed(now time.Time, cal *businesstime.Calendar, warnings []int) (fire []int, breach bool) {
	if c.Stage != Running {
		return nil, false
	}
	used := c.Used(now, cal)
	if c.BreachedAt.IsZero() && used >= c.Target {
		breach = true
		// When the target was used up, which may be a little before the save or the timer noticed.
		at, err := cal.Add(c.RunningSince, c.Target-c.Elapsed)
		if err != nil || at.After(now) || at.Before(c.RunningSince) {
			at = now
		}
		c.BreachedAt = at
	}
	for c.WarningsSent < len(warnings) && used >= c.share(warnings[c.WarningsSent]) {
		if !breach {
			fire = append(fire, c.WarningsSent)
		}
		c.WarningsSent++
	}
	return fire, breach
}

// Schedule works out when a running clock falls due and when the timer must next look at it: the next warning, or
// the breach. A clock that is not running has neither.
func (c *Clock) Schedule(cal *businesstime.Calendar, warnings []int) error {
	c.DueAt, c.NextAlertAt = time.Time{}, time.Time{}
	if c.Stage != Running {
		return nil
	}
	due, err := cal.Add(c.RunningSince, c.Target-c.Elapsed)
	if err != nil {
		return err
	}
	c.DueAt = due
	if c.BreachedAt.IsZero() {
		c.NextAlertAt = due
	}
	if c.WarningsSent < len(warnings) {
		at, err := cal.Add(c.RunningSince, c.share(warnings[c.WarningsSent])-c.Elapsed)
		if err != nil {
			return err
		}
		if c.NextAlertAt.IsZero() || at.Before(c.NextAlertAt) {
			c.NextAlertAt = at
		}
	}
	return nil
}

// Queue queues the alerts a clock owes, in the caller's transaction, so an alert is sent only if the change that made
// it owed commits. Each alert is queued once per clock, whatever retries or races happen.
func Queue(ctx context.Context, q *dbgen.Queries, clock uuid.UUID, warnings []int, breach bool) error {
	if breach {
		warnings = append(warnings, Breach)
	}
	for _, w := range warnings {
		key := fmt.Sprintf("sla:%s:%d", clock, w)
		if _, err := jobs.Enqueue(ctx, q, AlertKind, Alert{ClockID: clock, Warning: w}, jobs.EnqueueOptions{DedupeKey: key}); err != nil {
			return err
		}
	}
	return nil
}

// ---- calendars ----

// Hours is one stretch of a calendar's working week as stored and as the API takes it: day 0 is Sunday, times are
// HH:MM, and end may be 24:00.
type Hours struct {
	Day   int    `json:"day"`
	Start string `json:"start"`
	End   string `json:"end"`
}

func minutes(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || len(m) != 2 || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
		return 0, fmt.Errorf("%q is not a time of day (HH:MM, 00:00 to 24:00)", s)
	}
	return hh*60 + mm, nil
}

// Calendar builds a calendar from what is stored.
func Calendar(zone string, hours []Hours, holidays []string) (*businesstime.Calendar, error) {
	if zone == "" {
		return nil, errors.New("a calendar needs a time zone")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "Local" {
		return nil, fmt.Errorf("%q is not a time zone (use a name such as Europe/London or UTC)", zone)
	}
	ws := make([]businesstime.Window, 0, len(hours))
	for _, h := range hours {
		start, err := minutes(h.Start)
		if err != nil {
			return nil, err
		}
		end, err := minutes(h.End)
		if err != nil {
			return nil, err
		}
		ws = append(ws, businesstime.Window{Day: time.Weekday(h.Day), Start: start, End: end})
	}
	return businesstime.New(loc, ws, holidays)
}

// FromCalendarRow builds a calendar from its row.
func FromCalendarRow(r dbgen.BusinessCalendar) (*businesstime.Calendar, error) {
	var hours []Hours
	if err := json.Unmarshal(r.Hours, &hours); err != nil {
		return nil, fmt.Errorf("calendar %s: unreadable hours: %w", r.Name, err)
	}
	days := make([]string, len(r.Holidays))
	for i, d := range r.Holidays {
		days[i] = d.Time.Format(time.DateOnly)
	}
	return Calendar(r.TimeZone, hours, days)
}

// Calendars loads calendars by name, each once, through q (so under the caller's RLS). An SLA without a calendar
// counts every hour (nil). A calendar that is missing or broken also counts every hour, and says why, because a clock
// that counts something is better than one that stops.
type Calendars struct {
	q    *dbgen.Queries
	seen map[string]loaded
}

type loaded struct {
	cal     *businesstime.Calendar
	problem string
}

// NewCalendars is a calendar cache for one transaction.
func NewCalendars(q *dbgen.Queries) *Calendars { return &Calendars{q: q, seen: map[string]loaded{}} }

// Get returns the calendar and, if it could not be used, why.
func (c *Calendars) Get(ctx context.Context, name string) (*businesstime.Calendar, string, error) {
	if name == "" {
		return nil, "", nil
	}
	if l, ok := c.seen[name]; ok {
		return l.cal, l.problem, nil
	}
	var l loaded
	row, err := c.q.GetBusinessCalendarByName(ctx, name)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		l.problem = fmt.Sprintf("the calendar %s does not exist, so every hour is counted", name)
	case err != nil:
		return nil, "", err
	default:
		if l.cal, err = FromCalendarRow(row); err != nil {
			l.problem = fmt.Sprintf("the calendar %s cannot be used (%v), so every hour is counted", name, err)
		}
	}
	c.seen[name] = l
	return l.cal, l.problem, nil
}
