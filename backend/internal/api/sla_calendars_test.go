package api_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

type slaDefBody struct {
	App             string `json:"app"`
	Name            string `json:"name"`
	RecordTypeLabel string `json:"record_type_label"`
	Target          string `json:"target"`
	TargetRule      string `json:"target_rule"`
	Calendar        []struct {
		When string `json:"when"`
		Use  string `json:"use"`
	} `json:"calendar"`
	Edited bool `json:"edited"`
}

// An SLA counts the hours its rules choose for the record, and chooses again on every save: a P1 counts every hour, a
// record routed to a team counts that team's hours, and the time already used is kept through each switch.
func TestSLAHoursFollowTheRecord(t *testing.T) {
	h, pool, reg := setupApps(t)
	reload := func() {
		t.Helper()
		if err := reg.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	everyHour(t, h, admin)

	// A team that works two hours a week (Sunday night), so time spent with it barely counts.
	tiny := decodeBody[calendarBody](t, admin.do(h, "POST", "/api/v1/business-calendars",
		`{"name":"tiny","label":"Tiny","time_zone":"UTC","hours":[{"day":0,"start":"00:00","end":"02:00"}]}`))
	network := makeGroup(t, h, admin, "Network")
	g := decodeBody[struct {
		CalendarID *string `json:"calendar_id"`
	}](t, admin.do(h, "PATCH", "/api/v1/groups/"+network, `{"calendar_id":"`+tiny.ID+`"}`))
	if g.CalendarID == nil || *g.CalendarID != tiny.ID {
		t.Fatalf("the group's hours: %+v", g)
	}
	for _, path := range []string{"/api/v1/groups/" + network, "/api/v1/groups"} {
		if body := admin.do(h, "GET", path, "").Body.String(); !strings.Contains(body, `"calendar_id":"`+tiny.ID+`"`) {
			t.Errorf("%s carries the group's hours: %s", path, body)
		}
	}
	refusal(t, admin.do(h, "PATCH", "/api/v1/groups/"+network, `{"calendar_id":"`+network+`"}`), 422, "not a business calendar", "a calendar that does not exist")

	list := decodeBody[struct{ Items []slaDefBody }](t, admin.do(h, "GET", "/api/v1/slas", ""))
	var resolve slaDefBody
	for _, s := range list.Items {
		if s.Name == "resolve" {
			resolve = s
		}
	}
	if resolve.RecordTypeLabel != "Incidents" || resolve.TargetRule == "" || len(resolve.Calendar) != 1 || resolve.Calendar[0].Use != "business-hours" || resolve.Edited {
		t.Fatalf("resolve as shipped: %+v", resolve)
	}
	expect(t, agent.do(h, "GET", "/api/v1/slas", ""), 403, "an agent listing SLAs")
	path := "/api/v1/slas/incident-management/resolve"
	rules := `{"calendar":[{"when":"record.priority == 1","use":"24x7"},{"use":"group"},{"use":"business-hours"}]}`
	expect(t, agent.do(h, "PUT", path+"/calendar", rules), 403, "an agent choosing hours")
	refusal(t, admin.do(h, "PUT", path+"/calendar", `{"calendar":[{"when":"record.nonsense","use":"24x7"}]}`), 422, "calendar[0]", "a condition that does not compile")
	refusal(t, admin.do(h, "PUT", path+"/calendar", `{"calendar":[{"use":"Tea Time"}]}`), 422, "is not a calendar name", "a name that is not a calendar's")
	saved := decodeBody[slaDefBody](t, admin.do(h, "PUT", path+"/calendar", rules))
	if !saved.Edited || len(saved.Calendar) != 3 || saved.Calendar[1].Use != "group" {
		t.Fatalf("saved: %+v", saved)
	}
	reload() // the servers' registries reload on the install's notification

	tk := decodeBody[slaTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Switch is down","priority":3}`))
	if c := tk.clock(t, "resolve"); c.Calendar != "business-hours" {
		t.Fatalf("no team yet: %+v", c)
	}
	ticket := "/api/v1/tickets/" + tk.ID
	backdate(t, pool, tk.ID, "resolve", 2*time.Hour)
	moved := decodeBody[slaTicket](t, agent.do(h, "PATCH", ticket, `{"assignment_group_id":"`+network+`"}`)).clock(t, "resolve")
	if moved.Calendar != "tiny" || !near(time.Duration(moved.UsedMS)*time.Millisecond, 2*time.Hour, time.Minute) {
		t.Fatalf("handed to the team: %+v", moved)
	}
	backdate(t, pool, tk.ID, "resolve", 2*time.Hour)
	if c := decodeBody[slaTicket](t, agent.do(h, "GET", ticket, "")).clock(t, "resolve"); !near(time.Duration(c.UsedMS)*time.Millisecond, 2*time.Hour, 2*time.Minute) {
		t.Errorf("time with the team counts in its hours: %+v", c)
	}
	p1 := decodeBody[slaTicket](t, agent.do(h, "PATCH", ticket, `{"priority":1}`)).clock(t, "resolve")
	if p1.Calendar != "" || !near(time.Duration(p1.UsedMS)*time.Millisecond, 2*time.Hour, 2*time.Minute) {
		t.Fatalf("a P1 counts every hour, keeping what was used: %+v", p1)
	}
	backdate(t, pool, tk.ID, "resolve", time.Hour)
	if c := decodeBody[slaTicket](t, agent.do(h, "GET", ticket, "")).clock(t, "resolve"); !near(time.Duration(c.UsedMS)*time.Millisecond, 3*time.Hour, 2*time.Minute) {
		t.Errorf("every hour counts now: %+v", c)
	}
	if c := decodeBody[slaTicket](t, agent.do(h, "GET", ticket, "")).clock(t, "respond"); c.Calendar != "business-hours" {
		t.Errorf("respond keeps its own hours: %+v", c)
	}

	hist := decodeBody[struct {
		Items []struct {
			Label   string `json:"label"`
			OldText string `json:"old_text"`
			NewText string `json:"new_text"`
		}
	}](t, agent.do(h, "GET", ticket+"/history", ""))
	var switches []string
	for _, e := range hist.Items {
		if e.Label == "Resolve hours" {
			switches = append(switches, e.OldText+" → "+e.NewText)
		}
	}
	if strings.Join(switches, "; ") != "Business hours → Tiny; Tiny → Every hour (24x7)" {
		t.Errorf("the history says when the hours changed: %v", switches)
	}

	refusal(t, admin.do(h, "DELETE", "/api/v1/business-calendars/"+tiny.ID, ""), 409, "a group works by this calendar", "deleting a group's hours")

	back := decodeBody[slaDefBody](t, admin.do(h, "DELETE", path+"/edits", ""))
	if back.Edited || len(back.Calendar) != 1 || back.Calendar[0].Use != "business-hours" {
		t.Errorf("undone: %+v", back)
	}
	expect(t, admin.do(h, "DELETE", path+"/edits", ""), 404, "undoing twice")
	reload()
	if c := decodeBody[slaTicket](t, agent.do(h, "PATCH", ticket, `{"title":"Switch is down again"}`)).clock(t, "resolve"); c.Calendar != "" {
		t.Errorf("a running clock keeps the definition it started with: %+v", c)
	}
	if !strings.HasPrefix(resolve.TargetRule, "record.priority") {
		t.Errorf("target rule: %q", resolve.TargetRule)
	}
}
