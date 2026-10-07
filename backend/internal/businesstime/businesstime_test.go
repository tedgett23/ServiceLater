package businesstime

import (
	"errors"
	"testing"
	"time"
)

func weekdays(loc *time.Location, start, end int, holidays ...string) *Calendar {
	var ws []Window
	for d := time.Monday; d <= time.Friday; d++ {
		ws = append(ws, Window{Day: d, Start: start, End: end})
	}
	c, err := New(loc, ws, holidays)
	if err != nil {
		panic(err)
	}
	return c
}

func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Monday to Friday, nine to five: working time is counted only inside those hours, and Add finds the moment a
// target is used up, carrying over nights, weekends and holidays.
func TestWorkingHoursAreCountedAndAdded(t *testing.T) {
	utc := time.UTC
	c := weekdays(utc, 9*60, 17*60, "2026-12-25")
	for _, tc := range []struct {
		name, from, to string
		want           time.Duration
	}{
		{"inside one day", "2026-10-05 10:00", "2026-10-05 12:30", 150 * time.Minute},
		{"from before opening", "2026-10-05 07:00", "2026-10-05 10:00", time.Hour},
		{"over a night", "2026-10-05 16:00", "2026-10-06 10:00", 2 * time.Hour},
		{"over a weekend", "2026-10-09 16:00", "2026-10-12 10:00", 2 * time.Hour},
		{"all weekend", "2026-10-10 08:00", "2026-10-11 20:00", 0},
		{"a whole week", "2026-10-05 00:00", "2026-10-12 00:00", 40 * time.Hour},
		{"over a holiday", "2026-12-24 16:00", "2026-12-28 10:00", 2 * time.Hour},
		{"backwards", "2026-10-06 10:00", "2026-10-05 10:00", 0},
	} {
		got := c.Elapsed(at(t, utc, tc.from), at(t, utc, tc.to))
		if got != tc.want {
			t.Errorf("%s: elapsed %v, want %v", tc.name, got, tc.want)
		}
		if tc.want <= 0 {
			continue
		}
		// Add is Elapsed's inverse, landing inside working time.
		end, err := c.Add(at(t, utc, tc.from), tc.want)
		if err != nil || c.Elapsed(at(t, utc, tc.from), end) != tc.want || end.After(at(t, utc, tc.to)) {
			t.Errorf("%s: add %v from %s gave %v (%v)", tc.name, tc.want, tc.from, end, err)
		}
	}
	for _, tc := range []struct {
		name, from string
		d          time.Duration
		want       string
	}{
		{"same day", "2026-10-05 09:00", 4 * time.Hour, "2026-10-05 13:00"},
		{"exactly the day", "2026-10-05 09:00", 8 * time.Hour, "2026-10-05 17:00"},
		{"into the next day", "2026-10-05 15:00", 4 * time.Hour, "2026-10-06 11:00"},
		{"filed on Saturday", "2026-10-10 11:00", time.Hour, "2026-10-12 10:00"},
		{"over Christmas", "2026-12-24 16:00", 2 * time.Hour, "2026-12-28 10:00"},
		{"zero", "2026-10-10 11:00", 0, "2026-10-10 11:00"},
	} {
		got, err := c.Add(at(t, utc, tc.from), tc.d)
		if err != nil || !got.Equal(at(t, utc, tc.want)) {
			t.Errorf("%s: %v, want %s (%v)", tc.name, got, tc.want, err)
		}
	}
}

// A nil calendar is 24x7.
func TestNoCalendarCountsEverySecond(t *testing.T) {
	var c *Calendar
	from := time.Date(2026, 10, 10, 11, 0, 0, 0, time.UTC)
	if got := c.Elapsed(from, from.Add(50*time.Hour)); got != 50*time.Hour {
		t.Errorf("elapsed %v", got)
	}
	if got, _ := c.Add(from, 50*time.Hour); !got.Equal(from.Add(50 * time.Hour)) {
		t.Errorf("add %v", got)
	}
}

// Hours are wall-clock hours in the calendar's zone, so on the day the clocks go forward a nine-to-five day is still
// eight hours, and an overnight stretch is an hour shorter.
func TestDaylightSavingFollowsTheWallClock(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	c := weekdays(ny, 9*60, 17*60)
	// 2027-03-14 is a Sunday: the clocks go forward at 02:00. A 24x7 calendar in that zone loses the hour.
	allDay, _ := New(ny, []Window{{Day: time.Sunday, End: 24 * 60}}, nil)
	if got := allDay.Elapsed(at(t, ny, "2027-03-14 00:00"), at(t, ny, "2027-03-15 00:00")); got != 23*time.Hour {
		t.Errorf("a spring-forward Sunday has %v", got)
	}
	if got := c.Elapsed(at(t, ny, "2027-03-12 09:00"), at(t, ny, "2027-03-15 17:00")); got != 16*time.Hour {
		t.Errorf("Friday and Monday around the change: %v", got)
	}
	// Due times land at local office hours either side of the change, though UTC moved by an hour.
	got, _ := c.Add(at(t, ny, "2027-03-12 16:00"), 2*time.Hour)
	if want := at(t, ny, "2027-03-15 10:00"); !got.Equal(want) || got.UTC().Hour() != 14 {
		t.Errorf("add over the change: %v (UTC %v)", got, got.UTC())
	}
	// Elapsed and Add agree whatever zone the times are given in.
	if got := c.Elapsed(at(t, ny, "2027-03-12 09:00").UTC(), at(t, ny, "2027-03-12 17:00").UTC()); got != 8*time.Hour {
		t.Errorf("times in UTC: %v", got)
	}
}

func TestCalendarsAreChecked(t *testing.T) {
	utc := time.UTC
	for name, tc := range map[string]struct {
		ws       []Window
		holidays []string
	}{
		"no hours":    {nil, nil},
		"backwards":   {[]Window{{Day: time.Monday, Start: 600, End: 500}}, nil},
		"past 24:00":  {[]Window{{Day: time.Monday, Start: 600, End: 1500}}, nil},
		"overlapping": {[]Window{{Day: time.Monday, Start: 540, End: 720}, {Day: time.Monday, Start: 700, End: 1000}}, nil},
		"bad day":     {[]Window{{Day: 7, Start: 0, End: 60}}, nil},
		"bad holiday": {[]Window{{Day: time.Monday, Start: 0, End: 60}}, []string{"25/12/2026"}},
	} {
		if _, err := New(utc, tc.ws, tc.holidays); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(nil, []Window{{Day: time.Monday, End: 60}}, nil); err == nil {
		t.Error("no zone: accepted")
	}
	// Working time that never comes within ten years is an error, not a hang.
	c, _ := New(utc, []Window{{Day: time.Monday, Start: 0, End: 60}}, nil)
	var all []string
	for d := time.Date(2026, 10, 1, 0, 0, 0, 0, utc); d.Year() < 2038; d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Monday {
			all = append(all, d.Format(time.DateOnly))
		}
	}
	c.Holidays = all
	if _, err := c.Add(time.Date(2026, 10, 1, 0, 0, 0, 0, utc), time.Hour); !errors.Is(err, ErrTooFar) {
		t.Errorf("no working time: %v", err)
	}
}
