package api_test

import (
	"net/url"
	"slices"
	"testing"
	"time"
)

type calendarEntry struct {
	Reference    string    `json:"reference"`
	Status       string    `json:"status"`
	PlannedStart time.Time `json:"planned_start"`
	Overlaps     []string  `json:"overlaps"`
}

type calendarPage struct {
	Items     []calendarEntry `json:"items"`
	Truncated bool            `json:"truncated"`
}

// The calendar lists the changes planned in a range, as the caller may see them, and flags the ones still to be done
// whose windows overlap.
func TestTheCalendarListsPlannedChangesAndTheirOverlaps(t *testing.T) {
	h, pool := setup(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	plan := func(title string, from, to time.Duration, requester string) typedTicket {
		body := `{"type":"change","title":"` + title + `","change":{"planned_start":"` + day.Add(from).Format(time.RFC3339) +
			`","planned_end":"` + day.Add(to).Format(time.RFC3339) + `"}`
		if requester != "" {
			body += `,"requester_id":"` + requester + `"`
		}
		return decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", body+`}`))
	}
	switchA := plan("Core switch", 22*time.Hour, 26*time.Hour, "")
	firewall := plan("Firewall", 25*time.Hour, 27*time.Hour, lena.id)
	dns := plan("DNS", 30*time.Hour, 31*time.Hour, "")
	later := plan("Next month", 40*24*time.Hour, 41*24*time.Hour, "")
	decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Unplanned"}`))

	week := func(a actor) calendarPage {
		q := url.Values{"from": {day.Format(time.RFC3339)}, "to": {day.Add(7 * 24 * time.Hour).Format(time.RFC3339)}}
		return decodeBody[calendarPage](t, a.do(h, "GET", "/api/v1/calendar?"+q.Encode(), ""))
	}
	refs := func(p calendarPage) (out []string) {
		for _, e := range p.Items {
			out = append(out, e.Reference)
		}
		return out
	}
	got := week(agent)
	if want := []string{switchA.Reference, firewall.Reference, dns.Reference}; !slices.Equal(refs(got), want) {
		t.Fatalf("the week, by start: %v, want %v", refs(got), want)
	}
	if !slices.Equal(got.Items[0].Overlaps, []string{firewall.Reference}) || !slices.Equal(got.Items[1].Overlaps, []string{switchA.Reference}) ||
		len(got.Items[2].Overlaps) != 0 {
		t.Errorf("overlaps: %+v", got.Items)
	}
	if slices.Contains(refs(got), later.Reference) {
		t.Error("a change outside the range is listed")
	}

	// A cancelled change is still on the calendar, but no longer conflicts with anything.
	expect(t, agent.do(h, "POST", "/api/v1/tickets/"+firewall.ID+"/transitions", `{"to":"cancelled"}`), 200, "cancelling")
	if got := week(agent); len(got.Items[0].Overlaps) != 0 || len(got.Items[1].Overlaps) != 0 {
		t.Errorf("a cancelled change still overlaps: %+v", got.Items)
	}

	// An end user sees only their own.
	if mine := refs(week(lena)); !slices.Equal(mine, []string{firewall.Reference}) {
		t.Errorf("an end user's calendar: %v", mine)
	}

	for q, what := range map[string]string{
		"": "no range",
		"from=2026-11-02T00:00:00Z&to=2026-11-01T00:00:00Z": "backwards",
		"from=2026-01-01T00:00:00Z&to=2026-12-31T00:00:00Z": "a year",
		"from=yesterday&to=2026-11-01T00:00:00Z":            "not a time",
	} {
		expect(t, agent.do(h, "GET", "/api/v1/calendar?"+q, ""), 400, what)
	}
}
