// Package businesstime measures time the way a service level agreement does: only the hours a business calendar says
// are working hours count. It is pure: calendars come from the business_calendars table (migration 00025), and an SLA
// clock (internal/records) uses Elapsed and Add to know how much of its target is used and when it falls due.
//
// A nil *Calendar is 24x7: every second counts.
package businesstime

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

// Window is one stretch of working time in a week: on Day, from Start to End minutes after local midnight. End may be
// 1440 (midnight at the end of the day).
type Window struct {
	Day        time.Weekday
	Start, End int
}

// Calendar is a working week in a time zone, less holidays (dates in that zone).
type Calendar struct {
	Loc      *time.Location
	Windows  []Window
	Holidays []string // YYYY-MM-DD
}

// maxSpan bounds how far Add searches for working time, so a calendar that is almost all holidays fails instead of
// looping.
const maxSpan = 10 * 366

// ErrTooFar is returned when the time asked for lies more than ten years of working time away.
var ErrTooFar = errors.New("business time: no working time within ten years")

// New checks a calendar and returns it ready to use.
func New(loc *time.Location, windows []Window, holidays []string) (*Calendar, error) {
	if loc == nil {
		return nil, errors.New("a calendar needs a time zone")
	}
	if len(windows) == 0 {
		return nil, errors.New("a calendar needs at least one stretch of working time")
	}
	ws := slices.Clone(windows)
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].Day != ws[j].Day {
			return ws[i].Day < ws[j].Day
		}
		return ws[i].Start < ws[j].Start
	})
	for i, w := range ws {
		if w.Day < time.Sunday || w.Day > time.Saturday {
			return nil, fmt.Errorf("%d is not a day of the week", w.Day)
		}
		if w.Start < 0 || w.End > 24*60 || w.Start >= w.End {
			return nil, fmt.Errorf("%s: working time must start before it ends, within the day", w.Day)
		}
		if i > 0 && ws[i-1].Day == w.Day && ws[i-1].End > w.Start {
			return nil, fmt.Errorf("%s: working times overlap", w.Day)
		}
	}
	hs := slices.Clone(holidays)
	for _, h := range hs {
		if _, err := time.Parse(time.DateOnly, h); err != nil {
			return nil, fmt.Errorf("holiday %q is not a date (YYYY-MM-DD)", h)
		}
	}
	slices.Sort(hs)
	return &Calendar{Loc: loc, Windows: ws, Holidays: slices.Compact(hs)}, nil
}

func (c *Calendar) holiday(day time.Time) bool {
	_, found := slices.BinarySearch(c.Holidays, day.Format(time.DateOnly))
	return found
}

// day is local midnight of the day t falls on.
func (c *Calendar) day(t time.Time) time.Time {
	l := t.In(c.Loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, c.Loc)
}

// spans calls fn with each stretch of working time on the day starting at midnight d, in order, until fn returns
// false. Times are built with time.Date, so a day that loses or gains an hour to daylight saving is measured as the
// wall clock says.
func (c *Calendar) spans(d time.Time, fn func(from, to time.Time) bool) bool {
	if c.holiday(d) {
		return true
	}
	for _, w := range c.Windows {
		if w.Day != d.Weekday() {
			continue
		}
		from := time.Date(d.Year(), d.Month(), d.Day(), 0, w.Start, 0, 0, c.Loc)
		to := time.Date(d.Year(), d.Month(), d.Day(), 0, w.End, 0, 0, c.Loc)
		if !fn(from, to) {
			return false
		}
	}
	return true
}

// next is midnight of the day after d.
func next(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, d.Location())
}

// Elapsed is how much working time lies between from and to (zero if to is not after from).
func (c *Calendar) Elapsed(from, to time.Time) time.Duration {
	if !to.After(from) {
		return 0
	}
	if c == nil {
		return to.Sub(from)
	}
	var sum time.Duration
	for d := c.day(from); d.Before(to); d = next(d) {
		c.spans(d, func(a, b time.Time) bool {
			if a.Before(from) {
				a = from
			}
			if b.After(to) {
				b = to
			}
			if b.After(a) {
				sum += b.Sub(a)
			}
			return true
		})
	}
	return sum
}

// Add is the moment at which d of working time will have passed since from. A d of zero or less is from itself.
func (c *Calendar) Add(from time.Time, d time.Duration) (time.Time, error) {
	if d <= 0 {
		return from, nil
	}
	if c == nil {
		return from.Add(d), nil
	}
	left := d
	var at time.Time
	day := c.day(from)
	for i := 0; i < maxSpan; i, day = i+1, next(day) {
		done := !c.spans(day, func(a, b time.Time) bool {
			if a.Before(from) {
				a = from
			}
			if !b.After(a) {
				return true
			}
			if span := b.Sub(a); span >= left {
				at = a.Add(left)
				return false
			} else {
				left -= span
			}
			return true
		})
		if done {
			return at, nil
		}
	}
	return time.Time{}, ErrTooFar
}
