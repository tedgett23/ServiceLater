package slaclock

import (
	"testing"
	"time"

	"github.com/servicelater/servicelater/backend/internal/businesstime"
)

// A clock banks working time when it stops running and picks up from it when it runs again; owed alerts are
// reported once each, and a warning the breach overtakes is not sent.
func TestAClockCountsOnlyWhileRunning(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	var cal *businesstime.Calendar
	c := Clock{Target: 4 * time.Hour}
	c.MoveTo(Running, t0, cal)
	warnings := []int{50, 75}
	if err := c.Schedule(cal, warnings); err != nil || !c.DueAt.Equal(t0.Add(4*time.Hour)) || !c.NextAlertAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("scheduled: due %v, next %v (%v)", c.DueAt, c.NextAlertAt, err)
	}
	c.MoveTo(Paused, t0.Add(time.Hour), cal)
	if c.Elapsed != time.Hour || !c.RunningSince.IsZero() || c.Used(t0.Add(5*time.Hour), cal) != time.Hour {
		t.Errorf("paused: %+v", c)
	}
	if _ = c.Schedule(cal, warnings); !c.DueAt.IsZero() || !c.NextAlertAt.IsZero() {
		t.Errorf("a paused clock has no due time: %+v", c)
	}
	c.MoveTo(Running, t0.Add(3*time.Hour), cal)
	if fire, breach := c.Owed(t0.Add(4*time.Hour+30*time.Minute), cal, warnings); len(fire) != 1 || fire[0] != 0 || breach {
		t.Errorf("at 2h30 used: %v %v", fire, breach)
	}
	if fire, breach := c.Owed(t0.Add(4*time.Hour+31*time.Minute), cal, warnings); len(fire) != 0 || breach {
		t.Errorf("owed twice: %v %v", fire, breach)
	}
	// Both the 75% warning and the breach are past: only the breach is sent, dated when the target ran out.
	fire, breach := c.Owed(t0.Add(8*time.Hour), cal, warnings)
	if len(fire) != 0 || !breach || c.WarningsSent != 2 || !c.BreachedAt.Equal(t0.Add(6*time.Hour)) {
		t.Errorf("late: %v %v %+v", fire, breach, c)
	}
	_ = c.Schedule(cal, warnings)
	if !c.NextAlertAt.IsZero() || !c.DueAt.Equal(t0.Add(6*time.Hour)) {
		t.Errorf("nothing left to send, still overdue: %+v", c)
	}
	c.MoveTo(Stopped, t0.Add(9*time.Hour), cal)
	if c.Elapsed != 7*time.Hour || !c.StoppedAt.Equal(t0.Add(9*time.Hour)) {
		t.Errorf("stopped: %+v", c)
	}
	if fire, breach := c.Owed(t0.Add(20*time.Hour), cal, warnings); fire != nil || breach {
		t.Error("a stopped clock owes nothing")
	}
}

// Due times follow the calendar: a clock started on Friday afternoon falls due on Monday.
func TestDueTimesFollowTheCalendar(t *testing.T) {
	cal, err := Calendar("Europe/London", []Hours{{1, "09:00", "17:00"}, {2, "09:00", "17:00"}, {3, "09:00", "17:00"},
		{4, "09:00", "17:00"}, {5, "09:00", "17:00"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	london, _ := time.LoadLocation("Europe/London")
	fri := time.Date(2026, 10, 9, 15, 0, 0, 0, london)
	c := Clock{Target: 4 * time.Hour}
	c.MoveTo(Running, fri, cal)
	if err := c.Schedule(cal, nil); err != nil || !c.DueAt.Equal(time.Date(2026, 10, 12, 11, 0, 0, 0, london)) {
		t.Errorf("due %v (%v)", c.DueAt.In(london), err)
	}
	if got := c.Used(time.Date(2026, 10, 11, 12, 0, 0, 0, london), cal); got != 2*time.Hour {
		t.Errorf("used by Sunday: %v", got)
	}
	for _, bad := range []struct{ zone, start, end string }{{"Nowhere/Town", "09:00", "17:00"}, {"UTC", "9", "17:00"}, {"UTC", "09:00", "24:30"}, {"", "09:00", "17:00"}} {
		if _, err := Calendar(bad.zone, []Hours{{1, bad.start, bad.end}}, nil); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
