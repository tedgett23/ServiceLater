package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/expr"
	"github.com/servicelater/servicelater/backend/internal/records"
)

type freezeBody struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Reason        string    `json:"reason"`
	StartsAt      time.Time `json:"starts_at"`
	EndsAt        time.Time `json:"ends_at"`
	CreatedByName *string   `json:"created_by_name"`
}

func freezeJSON(name string, start, end time.Time) string {
	return `{"name":"` + name + `","reason":"Quiet period.","starts_at":"` + start.Format(time.RFC3339) + `","ends_at":"` + end.Format(time.RFC3339) + `"}`
}

func window(start, end time.Time) string {
	return `"planned_start":"` + start.Format(time.RFC3339) + `","planned_end":"` + end.Format(time.RFC3339) + `"`
}

// Admins and change managers declare, change and lift freezes; everyone signed in can read them.
func TestFreezesAreManagedByThoseAllowed(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	manager := newActor(t, h, pool, "cm@x.test", "end_user")
	expect(t, admin.do(h, "POST", "/api/v1/roles/change-manager/grants", `{"user_id":"`+manager.id+`"}`), 201, "a change manager")

	start := time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(14 * 24 * time.Hour)
	f := decodeBody[freezeBody](t, admin.do(h, "POST", "/api/v1/freeze-windows", freezeJSON("Year end", start, end)))
	if f.Name != "Year end" || !f.StartsAt.Equal(start) || f.CreatedByName == nil || *f.CreatedByName == "" {
		t.Fatalf("declared: %+v", f)
	}
	expect(t, manager.do(h, "POST", "/api/v1/freeze-windows", freezeJSON("Launch", start, start.Add(time.Hour))), 201, "a change manager declaring one")
	expect(t, agent.do(h, "POST", "/api/v1/freeze-windows", freezeJSON("Mine", start, end)), 403, "an agent declaring one")
	expect(t, lena.do(h, "PATCH", "/api/v1/freeze-windows/"+f.ID, `{"name":"Hers"}`), 403, "an end user changing one")
	expect(t, agent.do(h, "DELETE", "/api/v1/freeze-windows/"+f.ID, ""), 403, "an agent lifting one")

	for body, want := range map[string]string{
		freezeJSON(" ", start, end):         "name must be 1 to 200 characters",
		freezeJSON("Backwards", end, start): "a freeze must end after it starts",
		`{"name":"No times"}`:               "starts_at and ends_at are required",
		freezeJSON("Instant", start, start): "a freeze must end after it starts",
	} {
		refusal(t, admin.do(h, "POST", "/api/v1/freeze-windows", body), 422, want, body)
	}

	list := func(who actor) []freezeBody {
		return decodeBody[struct{ Items []freezeBody }](t, who.do(h, "GET",
			"/api/v1/freeze-windows?from=2026-12-01T00:00:00Z&to=2027-01-31T00:00:00Z", "")).Items
	}
	if got := list(lena); len(got) != 2 || got[0].Name != "Launch" && got[0].Name != "Year end" {
		t.Errorf("an end user reads them: %+v", got)
	}
	expect(t, lena.do(h, "GET", "/api/v1/freeze-windows?from=2026-12-01T00:00:00Z", ""), 400, "a range is required")

	moved := decodeBody[freezeBody](t, manager.do(h, "PATCH", "/api/v1/freeze-windows/"+f.ID, `{"ends_at":"`+end.Add(24*time.Hour).Format(time.RFC3339)+`"}`))
	if !moved.EndsAt.Equal(end.Add(24*time.Hour)) || moved.Name != "Year end" || moved.Reason != "Quiet period." {
		t.Errorf("a patch changes what it names: %+v", moved)
	}
	refusal(t, manager.do(h, "PATCH", "/api/v1/freeze-windows/"+f.ID, `{"starts_at":"2027-02-01T00:00:00Z"}`), 422,
		"a freeze must end after it starts", "moving the start past the end")
	expect(t, manager.do(h, "DELETE", "/api/v1/freeze-windows/"+f.ID, ""), 204, "lifting it")
	expect(t, admin.do(h, "GET", "/api/v1/freeze-windows/"+f.ID, ""), 404, "after it is lifted")
	expect(t, admin.do(h, "DELETE", "/api/v1/freeze-windows/"+f.ID, ""), 404, "lifting it twice")
}

// change-management keeps windows out of freezes: planning into one is refused, unless the change is an emergency.
// Edges may touch, and a change planned before the freeze was declared can still be edited.
func TestAChangeCannotBePlannedIntoAFreeze(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	start := time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(14 * 24 * time.Hour)

	// Planned before the freeze is declared.
	early := decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Early","change":{`+
		window(start.Add(time.Hour), start.Add(3*time.Hour))+`}}`))
	expect(t, admin.do(h, "POST", "/api/v1/freeze-windows", freezeJSON("Year end", start, end)), 201, "declaring the freeze")

	const refused = "The planned window overlaps a change freeze that applies to it."
	refusal(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Inside","change":{`+
		window(start.Add(48*time.Hour), start.Add(50*time.Hour))+`}}`), 422, refused, "filing into the freeze")
	refusal(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Across","change":{`+
		window(start.Add(-time.Hour), start.Add(time.Hour))+`}}`), 422, refused, "a window that runs into it")
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Before","change":{`+
		window(start.Add(-2*time.Hour), start)+`}}`), 201, "a window that ends as the freeze starts")
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"After","change":{`+
		window(end, end.Add(time.Hour))+`}}`), 201, "a window that starts as the freeze ends")
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Unplanned"}`), 201, "a change with no window yet")

	emergency := decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Hotfix","change":{"change_type":"emergency",`+
		window(start.Add(48*time.Hour), start.Add(50*time.Hour))+`}}`))
	if emergency.ID == "" {
		t.Fatal("an emergency change is planned into the freeze")
	}
	refusal(t, agent.do(h, "PATCH", "/api/v1/tickets/"+emergency.ID, `{"change":{"change_type":"normal"}}`), 422, refused,
		"turning it into a normal change while it sits in the freeze")

	// The early change is not stuck: other edits pass, and moving its window is checked.
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+early.ID, `{"change":{"test_plan":"Ping it."}}`), 200, "editing a change the freeze now covers")
	refusal(t, agent.do(h, "PATCH", "/api/v1/tickets/"+early.ID, `{"change":{`+window(start.Add(5*time.Hour), start.Add(6*time.Hour))+`}}`), 422,
		refused, "moving it within the freeze")
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+early.ID, `{"change":{`+window(end.Add(time.Hour), end.Add(2*time.Hour))+`}}`), 200,
		"moving it out")
}

// A freeze declared after a change was scheduled holds its work back; an emergency change can still start, and
// lifting the freeze lets the work start.
func TestWorkCannotStartDuringAFreeze(t *testing.T) {
	w := newChangeProcess(t)
	scheduled := func(kind string) typedTicket {
		c := w.file(t, kind)
		w.assess(t, c)
		if kind == "standard" {
			expect(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+c.ID+"/transitions", `{"to":"scheduled"}`), 200, "scheduling it")
		} else {
			expect(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+c.ID+"/transitions", `{"to":"authorize"}`), 200, "requesting authorization")
			expect(t, w.ecab.do(w.h, "POST", "/api/v1/approvals/"+w.pending(t, w.ecab, c).ID+"/decide", `{"decision":"approved"}`), 200, "approving")
			w.waitStatus(t, c, "scheduled")
		}
		return c
	}
	standard, emergency := scheduled("standard"), scheduled("emergency")

	now := time.Now().UTC()
	f := decodeBody[freezeBody](t, w.admin.do(w.h, "POST", "/api/v1/freeze-windows", freezeJSON("Incident freeze", now.Add(-time.Minute), now.Add(time.Hour))))
	const held = "A change freeze is on. Only an emergency change can start during a freeze."
	if m := moveTo(w.get(t, standard).Moves, "implement"); m == nil || m.Blocked != held {
		t.Errorf("the move is offered as blocked, saying why: %+v", m)
	}
	refusal(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+standard.ID+"/transitions", `{"to":"implement"}`), 422, held, "starting work in a freeze")
	expect(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+emergency.ID+"/transitions", `{"to":"implement"}`), 200, "an emergency change starting")

	expect(t, w.admin.do(w.h, "DELETE", "/api/v1/freeze-windows/"+f.ID, ""), 204, "lifting the freeze")
	expect(t, w.agent.do(w.h, "POST", "/api/v1/tickets/"+standard.ID+"/transitions", `{"to":"implement"}`), 200, "starting work after it")
}

// The calendar lists the freezes in its range, and marks each change whose window one overlaps.
func TestTheCalendarShowsFreezes(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	start := time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC)
	c := decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Swap","change":{`+
		window(start.Add(time.Hour), start.Add(2*time.Hour))+`}}`))
	other := decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Later","change":{`+
		window(start.Add(30*24*time.Hour), start.Add(31*24*time.Hour))+`}}`))
	f := decodeBody[freezeBody](t, admin.do(h, "POST", "/api/v1/freeze-windows", freezeJSON("Year end", start, start.Add(14*24*time.Hour))))

	got := decodeBody[struct {
		Items []struct {
			Reference string   `json:"reference"`
			Freezes   []string `json:"freezes"`
		} `json:"items"`
		Freezes []freezeBody `json:"freezes"`
	}](t, agent.do(h, "GET", "/api/v1/calendar?from=2026-12-01T00:00:00Z&to=2027-01-31T00:00:00Z", ""))
	if len(got.Freezes) != 1 || got.Freezes[0].ID != f.ID {
		t.Errorf("the calendar lists the freeze: %+v", got.Freezes)
	}
	for _, it := range got.Items {
		want := 0
		if it.Reference == c.Reference {
			want = 1
		}
		if it.Reference != c.Reference && it.Reference != other.Reference {
			continue
		}
		if len(it.Freezes) != want {
			t.Errorf("%s freezes: %v", it.Reference, it.Freezes)
		}
	}
}

// Every field apps.LookupTargets offers is one app_lookup_count accepts, with every operator, and nothing else is.
func TestLookupTargetsMatchTheDatabase(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	sample := map[string]any{"text": "x", "id": uuid.NewString(), "time": time.Now()}
	err := db.WithUser(context.Background(), pool, uuid.MustParse(admin.id), func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		count := records.Counter(q)
		for target, fields := range apps.LookupTargets {
			for field, kind := range fields {
				for _, op := range apps.LookupOps {
					if kind == "scope" && op != "in" {
						continue // a freeze's items are only asked about with in
					}
					v := sample[kind]
					if op == "in" {
						s := "x"
						if kind == "id" || kind == "scope" {
							s = uuid.NewString()
						}
						if kind == "time" {
							continue // in is for text and ids
						}
						v = []string{s}
					}
					if _, err := count(context.Background(), target, []expr.Filter{{Field: field, Op: op, Value: v}}); err != nil {
						t.Errorf("%s.%s %s: %v", target, field, op, err)
						return nil // the transaction is aborted
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		target string
		f      expr.Filter
	}{
		{"users", expr.Filter{Field: "id", Op: "eq", Value: "x"}},
		{"task", expr.Filter{Field: "id", Op: "eq", Value: uuid.NewString()}},
		{"change", expr.Filter{Field: "title", Op: "eq", Value: "x"}},
		{"change", expr.Filter{Field: "status", Op: "like", Value: "x"}},
		{"incident", expr.Filter{Field: "planned_start", Op: "eq", Value: time.Now()}},
	} {
		err := db.WithUser(context.Background(), pool, uuid.MustParse(admin.id), func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			_, err := records.Counter(q)(context.Background(), bad.target, []expr.Filter{bad.f})
			return err
		})
		if err == nil {
			t.Errorf("%s %+v was counted", bad.target, bad.f)
		}
	}
}

// A lookup counts only what the person acting can see.
func TestALookupCountsUnderRLS(t *testing.T) {
	h, pool, _ := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Not hers"}`), 201, "an incident")
	expect(t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Hers"}`), 201, "her incident")
	counts := map[string]int64{}
	for _, who := range []actor{agent, lena} {
		if err := db.WithUser(context.Background(), pool, uuid.MustParse(who.id), func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			n, err := records.Counter(q)(context.Background(), "incident", []expr.Filter{{Field: "state_category", Op: "in", Value: []string{"open", "waiting"}}})
			counts[who.id] = n
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if counts[agent.id] != 2 || counts[lena.id] != 1 {
		t.Errorf("the agent counts both, Lena only hers: %v", counts)
	}
}

// A freeze limited to configuration items applies only to changes linked to one of them: planning into it, linking
// such an item, and starting work are held back for those changes, and the calendar marks only them.
func TestAFreezeCanBeLimitedToItems(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	item := func(name string) string {
		return decodeBody[ciBody](t, admin.do(h, "POST", "/api/v1/cis", `{"class":"database","name":"`+name+`"}`)).ID
	}
	payroll, crm := item("payroll-db"), item("crm-db")
	start := time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	f := decodeBody[freezeBody](t, admin.do(h, "POST", "/api/v1/freeze-windows",
		strings.Replace(freezeJSON("Payroll run", start, end), `{"name"`, `{"items":["`+payroll+`"],"name"`, 1)))
	scoped := decodeBody[struct {
		ItemIDs []string `json:"item_ids"`
		Items   []ciBody `json:"items"`
	}](t, admin.do(h, "GET", "/api/v1/freeze-windows/"+f.ID, ""))
	if len(scoped.ItemIDs) != 1 || len(scoped.Items) != 1 || scoped.Items[0].Name != "payroll-db" {
		t.Fatalf("the freeze lists its item: %+v", scoped)
	}
	// Everyone reads freezes, and sees that one is limited; the item's name is for CMDB readers.
	seen := decodeBody[struct {
		ItemIDs []string `json:"item_ids"`
		Items   []ciBody `json:"items"`
	}](t, lena.do(h, "GET", "/api/v1/freeze-windows/"+f.ID, ""))
	if len(seen.ItemIDs) != 1 || len(seen.Items) != 0 {
		t.Errorf("an end user's view: %+v", seen)
	}

	inside := window(start.Add(time.Hour), start.Add(2*time.Hour))
	const refused = "The planned window overlaps a change freeze that applies to it."
	refusal(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Payroll patch","cis":["`+payroll+`"],"change":{`+inside+`}}`),
		422, refused, "filing a change to the frozen item")
	crmChange := decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"CRM patch","cis":["`+crm+`"],"change":{`+inside+`}}`))
	if crmChange.ID == "" {
		t.Fatal("a change to another item is not held back")
	}
	refusal(t, agent.do(h, "PUT", "/api/v1/tickets/"+crmChange.ID+"/cis/"+payroll, ""), 422, refused, "linking the frozen item to it")

	got := decodeBody[struct {
		Items []struct {
			Reference string   `json:"reference"`
			Freezes   []string `json:"freezes"`
		} `json:"items"`
	}](t, agent.do(h, "GET", "/api/v1/calendar?from=2026-12-01T00:00:00Z&to=2027-01-31T00:00:00Z", ""))
	for _, it := range got.Items {
		if it.Reference == crmChange.Reference && len(it.Freezes) != 0 {
			t.Errorf("the calendar marks a change the freeze does not apply to: %v", it.Freezes)
		}
	}

	// Lifting the limit makes it apply to every change again.
	expect(t, admin.do(h, "PATCH", "/api/v1/freeze-windows/"+f.ID, `{"items":[]}`), 200, "applying it to everything")
	refusal(t, agent.do(h, "PATCH", "/api/v1/tickets/"+crmChange.ID, `{"change":{`+window(start.Add(3*time.Hour), start.Add(4*time.Hour))+`}}`),
		422, refused, "moving the CRM change within the freeze")
}
