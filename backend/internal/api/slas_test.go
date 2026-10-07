package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/slaclock"
	"github.com/servicelater/servicelater/backend/internal/slatimer"
)

type slaClock struct {
	Name       string     `json:"name"`
	Label      string     `json:"label"`
	Stage      string     `json:"stage"`
	TargetMS   int64      `json:"target_ms"`
	UsedMS     int64      `json:"used_ms"`
	Calendar   string     `json:"calendar"`
	DueAt      *time.Time `json:"due_at"`
	BreachedAt *time.Time `json:"breached_at"`
	StoppedAt  *time.Time `json:"stopped_at"`
	Problem    string     `json:"problem"`
}

type slaTicket struct {
	ID          string     `json:"id"`
	Reference   string     `json:"reference"`
	Status      string     `json:"status"`
	SLAs        []slaClock `json:"slas"`
	SLADueAt    *time.Time `json:"sla_due_at"`
	SLABreached bool       `json:"sla_breached"`
}

func (tk slaTicket) clock(t *testing.T, name string) slaClock {
	t.Helper()
	for _, c := range tk.SLAs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s has no %s clock: %+v", tk.Reference, name, tk.SLAs)
	return slaClock{}
}

type calendarBody struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Label    string `json:"label"`
	TimeZone string `json:"time_zone"`
	Hours    []struct {
		Day        int
		Start, End string
	} `json:"hours"`
	Holidays []string `json:"holidays"`
}

// allWeek is a calendar body whose working time is every hour, so a test's clock does not depend on the day it runs.
const allWeek = `[{"day":0,"start":"00:00","end":"24:00"},{"day":1,"start":"00:00","end":"24:00"},{"day":2,"start":"00:00","end":"24:00"},
{"day":3,"start":"00:00","end":"24:00"},{"day":4,"start":"00:00","end":"24:00"},{"day":5,"start":"00:00","end":"24:00"},{"day":6,"start":"00:00","end":"24:00"}]`

// everyHour makes the business-hours calendar (which incident-management's SLAs count by) count every hour.
func everyHour(t *testing.T, h http.Handler, admin actor) {
	t.Helper()
	cals := decodeBody[struct{ Items []calendarBody }](t, admin.do(h, "GET", "/api/v1/business-calendars", ""))
	for _, c := range cals.Items {
		if c.Name == "business-hours" {
			expect(t, admin.do(h, "PATCH", "/api/v1/business-calendars/"+c.ID, `{"hours":`+allWeek+`}`), 200, "making business hours every hour")
			return
		}
	}
	t.Fatal("no business-hours calendar")
}

// backdate makes a running clock look as if it started d earlier, as the passing of time would, and asks the timer
// to look at it.
func backdate(t *testing.T, pool *pgxpool.Pool, ticketID, sla string, d time.Duration) {
	t.Helper()
	if err := db.WithSystem(context.Background(), pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE task_slas SET running_since = running_since - $3::interval, started_at = started_at - $3::interval,
			next_alert_at = now() WHERE task_id = $1 AND sla = $2 AND stage = 'running'`, ticketID, sla, d.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func near(got, want, slack time.Duration) bool { return got >= want-slack && got <= want+slack }

// incident-management's respond and resolve SLAs follow an incident through its life: they start with it, respond is
// met by assigning it, resolve stands still while it is on hold, stops when it is resolved and picks up again when it
// is reopened, and a change of priority changes the target but keeps the time used.
func TestSLAClocksFollowTheIncident(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	everyHour(t, h, admin)

	tk := decodeBody[slaTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Printer on fire","priority":2}`))
	respond, resolve := tk.clock(t, "respond"), tk.clock(t, "resolve")
	if respond.Label != "Respond" || respond.Stage != "running" || respond.TargetMS != time.Hour.Milliseconds() || respond.Calendar != "business-hours" {
		t.Errorf("respond: %+v", respond)
	}
	if resolve.Stage != "running" || resolve.TargetMS != (8*time.Hour).Milliseconds() || resolve.DueAt == nil ||
		!near(time.Until(*resolve.DueAt), 8*time.Hour, time.Minute) || resolve.Problem != "" {
		t.Errorf("resolve: %+v", resolve)
	}
	path := "/api/v1/tickets/" + tk.ID
	get := func() slaTicket { return decodeBody[slaTicket](t, agent.do(h, "GET", path, "")) }
	move := func(to string) slaTicket {
		t.Helper()
		return decodeBody[slaTicket](t, agent.do(h, "POST", path+"/transitions", `{"to":"`+to+`"}`))
	}

	backdate(t, pool, tk.ID, "resolve", 2*time.Hour)
	if c := get().clock(t, "resolve"); !near(time.Duration(c.UsedMS)*time.Millisecond, 2*time.Hour, time.Minute) {
		t.Errorf("two hours on: %+v", c)
	}
	// Raising the priority shortens the target to 4h; the two hours used stay used.
	c := decodeBody[slaTicket](t, agent.do(h, "PATCH", path, `{"priority":1}`)).clock(t, "resolve")
	if c.TargetMS != (4*time.Hour).Milliseconds() || c.DueAt == nil || !near(time.Until(*c.DueAt), 2*time.Hour, time.Minute) {
		t.Errorf("priority 1: %+v (due in %v)", c, time.Until(*c.DueAt))
	}

	// Assigning it meets the respond SLA.
	tk = decodeBody[slaTicket](t, agent.do(h, "PATCH", path, `{"assignee_id":"`+agent.id+`"}`))
	if r := tk.clock(t, "respond"); r.Stage != "stopped" || r.BreachedAt != nil || r.StoppedAt == nil || r.DueAt != nil {
		t.Errorf("respond after assigning: %+v", r)
	}

	// On hold, the resolve clock waits; resumed, it runs on from where it was.
	held := move("on_hold").clock(t, "resolve")
	if held.Stage != "paused" || held.DueAt != nil {
		t.Errorf("on hold: %+v", held)
	}
	backdate(t, pool, tk.ID, "resolve", 5*time.Hour) // nothing runs while paused
	if c := get().clock(t, "resolve"); c.UsedMS != held.UsedMS {
		t.Errorf("time passed while paused: %v, then %v", held.UsedMS, c.UsedMS)
	}
	if c := move("in_progress").clock(t, "resolve"); c.Stage != "running" || c.DueAt == nil || !near(time.Until(*c.DueAt), 2*time.Hour, time.Minute) {
		t.Errorf("resumed: %+v", c)
	}

	// Resolved, it stops and is met; reopened, it runs again with what it had used.
	done := move("resolved").clock(t, "resolve")
	if done.Stage != "stopped" || done.BreachedAt != nil || done.DueAt != nil {
		t.Errorf("resolved: %+v", done)
	}
	if c := move("in_progress").clock(t, "resolve"); c.Stage != "running" || !near(time.Duration(c.UsedMS)*time.Millisecond, 2*time.Hour, time.Minute) {
		t.Errorf("reopened: %+v", c)
	}

	// The queue carries each ticket's soonest due time and can sort by it.
	other := decodeBody[slaTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Low","priority":4}`))
	list := decodeBody[struct{ Items []slaTicket }](t, agent.do(h, "GET", "/api/v1/tickets?sort=due", "")).Items
	if len(list) != 2 || list[0].ID != tk.ID || list[0].SLADueAt == nil || list[1].ID != other.ID || list[0].SLABreached {
		t.Errorf("sorted by due: %+v", list)
	}
	expect(t, agent.do(h, "GET", "/api/v1/tickets?sort=sideways", ""), 400, "an unknown sort")
}

// pendingAlerts lists the SLA alert jobs not yet handled.
func pendingAlerts(t *testing.T, pool *pgxpool.Pool) []jobs.Job {
	t.Helper()
	var out []jobs.Job
	if err := db.WithSystem(context.Background(), pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT id, kind, payload FROM jobs WHERE kind = 'sla.alert' AND state = 'pending' ORDER BY id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			j := jobs.Job{Attempt: 1, MaxAttempts: 5}
			if err := rows.Scan(&j.ID, &j.Kind, &j.Payload); err != nil {
				return err
			}
			out = append(out, j)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = tx.Exec(context.Background(), `UPDATE jobs SET state = 'done' WHERE kind = 'sla.alert' AND state = 'pending'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func alertOf(t *testing.T, j jobs.Job) slaclock.Alert {
	t.Helper()
	var a slaclock.Alert
	if err := j.Decode(&a); err != nil {
		t.Fatal(err)
	}
	return a
}

func titles(ns []sentNotification) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.Title)
	}
	return out
}

// With nobody saving, the timer finds the clock that has used 75% of its target and queues its warning, then the one
// that has used all of it and queues the breach, each once. The alerts tell people what the SLA says, as the app.
func TestTheTimerWarnsThenBreaches(t *testing.T) {
	h, pool, reg := setupApps(t)
	ctx := context.Background()
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	fixer := newActor(t, h, pool, "fixer@x.test", "agent")
	everyHour(t, h, admin)
	handle := slatimer.Handler(pool, reg)

	tk := decodeBody[slaTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Slow","priority":2}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignee_id":"`+fixer.id+`"}`), 200, "assigning")
	if n, err := slatimer.Tick(ctx, pool, time.Now()); err != nil || n != 0 {
		t.Fatalf("nothing is due yet: %d %v", n, err)
	}

	backdate(t, pool, tk.ID, "resolve", 6*time.Hour+time.Minute) // 75% of 8h is 6h
	if n, err := slatimer.Tick(ctx, pool, time.Now()); err != nil || n != 1 {
		t.Fatalf("the warning: %d %v", n, err)
	}
	alerts := pendingAlerts(t, pool)
	if len(alerts) != 1 || alertOf(t, alerts[0]).Warning != 0 {
		t.Fatalf("one warning queued: %+v", alerts)
	}
	if err := handle(ctx, alerts[0]); err != nil {
		t.Fatal(err)
	}
	if got := titles(about(t, h, fixer, tk.ID)); len(got) != 2 || got[0] != tk.Reference+" is close to its resolution target" {
		t.Errorf("the assignee is warned: %v", got)
	}
	// Looked at again, it owes nothing more until it is due.
	if _, err := slatimer.Tick(ctx, pool, time.Now()); err != nil || len(pendingAlerts(t, pool)) != 0 {
		t.Errorf("a second warning: %v", err)
	}

	backdate(t, pool, tk.ID, "resolve", 2*time.Hour)
	if _, err := slatimer.Tick(ctx, pool, time.Now()); err != nil {
		t.Fatal(err)
	}
	alerts = pendingAlerts(t, pool)
	if len(alerts) != 1 || alertOf(t, alerts[0]).Warning != slaclock.Breach {
		t.Fatalf("the breach queued: %+v", alerts)
	}
	got := decodeBody[slaTicket](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID, ""))
	c := got.clock(t, "resolve")
	if c.BreachedAt == nil || c.Stage != "running" || time.Since(*c.BreachedAt) < 30*time.Second {
		t.Errorf("breached when the target ran out, about a minute ago: %+v", c)
	}
	if err := handle(ctx, alerts[0]); err != nil {
		t.Fatal(err)
	}
	if got := titles(about(t, h, fixer, tk.ID)); len(got) != 3 || got[0] != tk.Reference+" missed its resolution target" {
		t.Errorf("the assignee hears of the breach: %v", got)
	}
	list := decodeBody[struct{ Items []slaTicket }](t, agent.do(h, "GET", "/api/v1/tickets?breached=true", "")).Items
	if len(list) != 1 || list[0].ID != tk.ID || !list[0].SLABreached || list[0].SLADueAt != nil {
		t.Errorf("the breached filter: %+v", list)
	}
	// The due-time order puts what has breached first.
	fresh := decodeBody[slaTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Fresh","priority":1}`))
	if sorted := decodeBody[struct{ Items []slaTicket }](t, agent.do(h, "GET", "/api/v1/tickets?sort=due", "")).Items; len(sorted) != 2 ||
		sorted[0].ID != tk.ID || sorted[1].ID != fresh.ID {
		t.Errorf("breached first: %+v", sorted)
	}
	// Resolving it late stops the clock, still breached.
	c = decodeBody[slaTicket](t, agent.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/transitions", `{"to":"resolved"}`)).clock(t, "resolve")
	if c.Stage != "stopped" || c.BreachedAt == nil {
		t.Errorf("resolved late: %+v", c)
	}

	var audit int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM action_runs WHERE timing = 'sla' AND outcome = 'ok' AND app = 'incident-management' AND rule = 'resolve'`).Scan(&audit); err != nil || audit != 2 {
		t.Errorf("the alerts are audited: %d %v", audit, err)
	}
}

// A clock that passes its target between two saves is breached by the next save itself, so a ticket that is saved
// before the timer looks never misses its breach.
func TestASaveCatchesABreachTheTimerHasNotSeen(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	everyHour(t, h, admin)
	tk := decodeBody[slaTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Quick","priority":1}`))
	backdate(t, pool, tk.ID, "respond", time.Hour)
	got := decodeBody[slaTicket](t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"assignee_id":"`+agent.id+`"}`))
	if c := got.clock(t, "respond"); c.Stage != "stopped" || c.BreachedAt == nil {
		t.Errorf("assigned after the target: %+v", c)
	}
	if n := len(pendingAlerts(t, pool)); n != 1 {
		t.Errorf("the breach is queued by the save: %d", n)
	}
}

// Admins manage business calendars; everyone reads them. A calendar is checked as it is saved, and one that running
// clocks count by cannot be deleted. Changing one has its clocks rescheduled.
func TestCalendarsAreManagedByThoseAllowed(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")

	body := `{"name":"uk-office","label":"UK office","time_zone":"Europe/London","hours":[{"day":1,"start":"08:30","end":"17:30"}],"holidays":["2026-12-25","2026-12-25"]}`
	cal := decodeBody[calendarBody](t, admin.do(h, "POST", "/api/v1/business-calendars", body))
	if cal.Name != "uk-office" || cal.TimeZone != "Europe/London" || len(cal.Hours) != 1 || cal.Hours[0].Start != "08:30" || len(cal.Holidays) != 1 {
		t.Fatalf("created: %+v", cal)
	}
	expect(t, agent.do(h, "POST", "/api/v1/business-calendars", strings.Replace(body, "uk-office", "mine", 1)), 403, "an agent adding one")
	expect(t, lena.do(h, "PATCH", "/api/v1/business-calendars/"+cal.ID, `{"label":"Hers"}`), 403, "an end user changing one")
	refusal(t, admin.do(h, "POST", "/api/v1/business-calendars", body), 409, "a calendar named uk-office already exists", "a second of one name")
	for b, want := range map[string]string{
		strings.Replace(body, "Europe/London", "Mars/Olympus", 1):                   "is not a time zone",
		strings.Replace(body, "uk-office", "UK Office", 1):                          "name must be",
		strings.Replace(body, `"17:30"`, `"07:00"`, 1):                              "must start before it ends",
		strings.Replace(body, `"17:30"`, `"25:00"`, 1):                              "is not a time of day",
		strings.Replace(body, `"2026-12-25","2026-12-25"`, `"25/12/2026"`, 1):       "is not a date",
		strings.Replace(body, `[{"day":1,"start":"08:30","end":"17:30"}]`, `[]`, 1): "at least one stretch",
	} {
		refusal(t, admin.do(h, "POST", "/api/v1/business-calendars", strings.Replace(b, "uk-office", "other", 1)), 422, want, b)
	}
	if got := decodeBody[struct{ Items []calendarBody }](t, lena.do(h, "GET", "/api/v1/business-calendars", "")).Items; len(got) != 2 {
		t.Errorf("an end user reads them: %+v", got)
	}
	renamed := decodeBody[calendarBody](t, admin.do(h, "PATCH", "/api/v1/business-calendars/"+cal.ID, `{"label":"London"}`))
	if renamed.Label != "London" || renamed.TimeZone != "Europe/London" || len(renamed.Holidays) != 1 {
		t.Errorf("a patch changes what it names: %+v", renamed)
	}
	expect(t, admin.do(h, "DELETE", "/api/v1/business-calendars/"+cal.ID, ""), 204, "deleting an unused one")

	// business-hours is in use once an incident is filed; changing it asks the timer to look at its clocks now.
	tk := decodeBody[slaTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Counting"}`))
	var hours calendarBody
	for _, c := range decodeBody[struct{ Items []calendarBody }](t, admin.do(h, "GET", "/api/v1/business-calendars", "")).Items {
		if c.Name == "business-hours" {
			hours = c
		}
	}
	refusal(t, admin.do(h, "DELETE", "/api/v1/business-calendars/"+hours.ID, ""), 409, "SLA clocks still count by this calendar", "deleting one in use")
	if err := db.WithSystem(context.Background(), pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE task_slas SET next_alert_at = now() + interval '1 day' WHERE task_id = $1`, tk.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	expect(t, admin.do(h, "PATCH", "/api/v1/business-calendars/"+hours.ID, `{"holidays":["2030-01-01"]}`), 200, "adding a holiday")
	var due int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM task_slas WHERE task_id = $1 AND stage = 'running' AND next_alert_at <= now()`, tk.ID).Scan(&due); err != nil || due != 2 {
		t.Errorf("both clocks are rechecked: %d %v", due, err)
	}
}

// A requester sees the clocks on their own incident; another end user sees none of them, even by asking the
// database directly.
func TestSLAClocksAreVisibleWithTheirRecord(t *testing.T) {
	h, pool, _ := setupApps(t)
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	omar := newActor(t, h, pool, "omar@x.test", "end_user")
	tk := decodeBody[slaTicket](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"My laptop"}`))
	if got := decodeBody[slaTicket](t, lena.do(h, "GET", "/api/v1/tickets/"+tk.ID, "")); len(got.SLAs) != 2 {
		t.Errorf("the requester sees the clocks: %+v", got.SLAs)
	}
	expect(t, omar.do(h, "GET", "/api/v1/tickets/"+tk.ID, ""), 404, "another end user")
	var n int
	if err := db.WithUser(context.Background(), pool, uuid.MustParse(omar.id), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM task_slas`).Scan(&n)
	}); err != nil || n != 0 {
		t.Errorf("another end user reads %d clocks (%v)", n, err)
	}
}

// An SLA that cannot be worked out never blocks the save. A clock that cannot tell whether it starts is not started;
// one already running keeps its stage and says what is wrong; both are audited.
func TestABrokenSLANeverBlocksASave(t *testing.T) {
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	installNotifyApp(t, pool, reg, map[string]string{"slas/fragile.yaml": `kind: sla
name: fragile
label: Fragile
record_type: incident
start: 'record.title != "never"'
target: '=duration(record.description)'
`})
	tk := decodeBody[slaTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Fine","description":"2h"}`))
	if c := tk.clock(t, "fragile"); c.Stage != "running" || c.TargetMS != (2*time.Hour).Milliseconds() {
		t.Fatalf("started: %+v", c)
	}
	got := decodeBody[slaTicket](t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"description":"whenever"}`))
	if c := got.clock(t, "fragile"); c.Stage != "running" || c.TargetMS != (2*time.Hour).Milliseconds() || !strings.Contains(c.Problem, "target") {
		t.Errorf("a target that cannot be worked out: %+v", c)
	}
	broken := decodeBody[slaTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Vague","description":"soon"}`))
	if broken.ID == "" {
		t.Fatal("the save was blocked")
	}
	for _, c := range broken.SLAs {
		if c.Name == "fragile" {
			t.Errorf("a clock that could not be worked out was started: %+v", c)
		}
	}
	var failed int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM action_runs WHERE timing = 'sla' AND outcome = 'failed' AND rule = 'fragile'`).Scan(&failed); err != nil || failed != 2 {
		t.Errorf("both failures are audited: %d %v", failed, err)
	}

	// Removing the app cancels its clocks on the next save; they no longer show.
	if _, err := apps.Uninstall(context.Background(), pool, "house-notify"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	got = decodeBody[slaTicket](t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"description":"3h"}`))
	for _, c := range got.SLAs {
		if c.Name == "fragile" {
			t.Errorf("the removed SLA's clock is still shown: %+v", c)
		}
	}
	var stage string
	if err := pool.QueryRow(context.Background(), `SELECT stage FROM task_slas WHERE task_id = $1 AND sla = 'fragile'`, tk.ID).Scan(&stage); err != nil || stage != "cancelled" {
		t.Errorf("the clock: %q %v", stage, err)
	}
}

// Several workers' timers may look at once: each clock is claimed by one of them, and each alert is queued once.
func TestTimersOnSeveralWorkersQueueEachAlertOnce(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	everyHour(t, h, admin)
	const n = 12
	for i := range n {
		tk := decodeBody[slaTicket](t, agent.do(h, "POST", "/api/v1/tickets", fmt.Sprintf(`{"title":"Late %d","priority":2}`, i)))
		backdate(t, pool, tk.ID, "resolve", 9*time.Hour)
		backdate(t, pool, tk.ID, "respond", 9*time.Hour)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := slatimer.Tick(context.Background(), pool, time.Now())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var breaches, warnings int
	for _, j := range pendingAlerts(t, pool) {
		if alertOf(t, j).Warning == slaclock.Breach {
			breaches++
		} else {
			warnings++
		}
	}
	// Every clock breached; the warnings it passed on the way are overtaken by the breach.
	if breaches != 2*n || warnings != 0 {
		t.Errorf("%d breaches and %d warnings queued, want %d and 0", breaches, warnings, 2*n)
	}
}
