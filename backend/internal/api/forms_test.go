package api_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

type formField struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Label   string `json:"label"`
	Help    string `json:"help"`
	Core    bool   `json:"core"`
	Default any    `json:"default"`
	Options []struct {
		Value string `json:"value"`
	} `json:"options"`
}

type form struct {
	RecordType string      `json:"record_type"`
	Fields     []formField `json:"fields"`
	Create     []string    `json:"create"`
	Detail     []struct {
		Label  string   `json:"label"`
		Fields []string `json:"fields"`
	} `json:"detail"`
	Reactive []string `json:"reactive"`
	App      string   `json:"app"`
}

type fieldState struct {
	Hidden   bool     `json:"hidden"`
	Required bool     `json:"required"`
	ReadOnly bool     `json:"read_only"`
	Options  []string `json:"options"`
}

type formTicket struct {
	ID          string                `json:"id"`
	Priority    int                   `json:"priority"`
	Status      string                `json:"status"`
	Fields      map[string]any        `json:"fields"`
	FieldStates map[string]fieldState `json:"field_states"`
}

func (f form) field(name string) *formField {
	for i := range f.Fields {
		if f.Fields[i].Name == name {
			return &f.Fields[i]
		}
	}
	return nil
}

// incident-management's form sorts an incident by category and subcategory, and works out its priority from impact
// and urgency. Everyone who files incidents reads the form; the server holds every save to it.
func TestTheIncidentFormSortsAndPrioritisesIncidents(t *testing.T) {
	h, pool := setup(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")

	f := decodeBody[form](t, lena.do(h, "GET", "/api/v1/forms/incident", ""))
	if f.App != "incident-management" || f.field("category") == nil || len(f.field("category").Options) != 5 || f.field("category").Core {
		t.Fatalf("the shipped form: %+v", f)
	}
	if p := f.field("priority"); p == nil || !p.Core || p.Type != "priority" || p.Help == "" {
		t.Errorf("priority, a core field with the form's help: %+v", p)
	}
	if !slices.Equal(f.Create, []string{"title", "description", "category", "subcategory", "impact", "urgency", "priority"}) {
		t.Errorf("create layout: %v", f.Create)
	}
	if len(f.Detail) != 2 || f.Detail[0].Label != "Classification" || !slices.Equal(f.Detail[1].Fields, []string{"resolution"}) {
		t.Errorf("detail layout: %+v", f.Detail)
	}
	if !slices.Equal(f.Reactive, []string{"category", "impact", "urgency"}) {
		t.Errorf("reactive: %v", f.Reactive)
	}
	expect(t, lena.do(h, "GET", "/api/v1/forms/task", ""), 404, "a form for task")
	expect(t, lena.do(h, "GET", "/api/v1/forms/widget", ""), 404, "a form for no type")
	if cf := decodeBody[form](t, lena.do(h, "GET", "/api/v1/forms/change", "")); cf.App != "change-management" || cf.field("planned_start") == nil {
		t.Errorf("the change form: %+v", cf)
	}

	// While filing: a subcategory is offered once there is a category, and only that category's.
	st := decodeBody[struct {
		States map[string]fieldState `json:"states"`
	}](t, lena.do(h, "POST", "/api/v1/forms/incident/states", `{"title":"x"}`)).States
	if !st["subcategory"].Hidden || st["priority"].ReadOnly {
		t.Errorf("nothing chosen: %+v", st)
	}
	draft := decodeBody[struct {
		States map[string]fieldState `json:"states"`
		Values map[string]any        `json:"values"`
	}](t, lena.do(h, "POST", "/api/v1/forms/incident/states", `{"fields":{"category":"network","impact":"1","urgency":"2"}}`))
	st = draft.States
	if st["subcategory"].Hidden || !slices.Equal(st["subcategory"].Options, []string{"wifi", "vpn", "wired"}) || !st["priority"].ReadOnly {
		t.Errorf("network, impact and urgency chosen: %+v", st)
	}
	// The rules are tried on the draft, so the form shows the priority the save will give it.
	if draft.Values["priority"] != float64(2) || len(draft.Values) != 1 {
		t.Errorf("preview: %v", draft.Values)
	}

	// Filed with impact and urgency, the priority follows them, and people cannot set it while they are set.
	inc := decodeBody[formTicket](t, lena.do(h, "POST", "/api/v1/tickets",
		`{"title":"VPN drops","fields":{"category":"network","subcategory":"vpn","impact":"1","urgency":"2"}}`))
	if inc.Priority != 2 || inc.Fields["subcategory"] != "vpn" || !inc.FieldStates["priority"].ReadOnly {
		t.Fatalf("filed: %+v", inc)
	}
	refusal(t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"x","priority":1,"fields":{"impact":"3","urgency":"3"}}`), 422,
		"Priority cannot be changed now", "a priority as well as impact and urgency")
	refusal(t, agent.do(h, "PATCH", "/api/v1/tickets/"+inc.ID, `{"priority":1}`), 422, "Priority cannot be changed now", "priority while read-only")
	moved := decodeBody[formTicket](t, agent.do(h, "PATCH", "/api/v1/tickets/"+inc.ID, `{"fields":{"urgency":"3"}}`))
	if moved.Priority != 3 || moved.Fields["category"] != "network" {
		t.Errorf("urgency lowered: %+v", moved)
	}
	cleared := decodeBody[formTicket](t, agent.do(h, "PATCH", "/api/v1/tickets/"+inc.ID, `{"fields":{"impact":null}}`))
	if _, ok := cleared.Fields["impact"]; ok || cleared.FieldStates["priority"].ReadOnly {
		t.Errorf("impact cleared: %+v", cleared)
	}
	if set := decodeBody[formTicket](t, agent.do(h, "PATCH", "/api/v1/tickets/"+inc.ID, `{"priority":1}`)); set.Priority != 1 {
		t.Errorf("priority by hand again: %+v", set)
	}

	for body, want := range map[string]string{
		`{"title":"x","fields":{"category":"network","subcategory":"printer"}}`: `Subcategory cannot be \"printer\" here`,
		`{"title":"x","fields":{"category":"plumbing"}}`:                        "Category must be one of",
		`{"title":"x","fields":{"category":3}}`:                                 "Category takes text",
		`{"title":"x","fields":{"colour":"red"}}`:                               `incident records have no field \"colour\"`,
	} {
		refusal(t, lena.do(h, "POST", "/api/v1/tickets", body), 422, want, body)
	}
	// Writing fields is editing the record: the requester reads them and cannot change them.
	seen := decodeBody[formTicket](t, lena.do(h, "GET", "/api/v1/tickets/"+inc.ID, ""))
	if seen.Fields["subcategory"] != "vpn" {
		t.Errorf("the requester's view: %+v", seen.Fields)
	}
	expect(t, lena.do(h, "PATCH", "/api/v1/tickets/"+inc.ID, `{"fields":{"category":"other"}}`), 403, "the requester editing fields")
}

// siteForm is a form with one of each kind of field and condition, for an incident app of a test's own.
const siteForm = `kind: form
record_type: incident
fields:
  site: { type: text, label: Site, required: true, max_length: 20 }
  seats: { type: number, label: Seats, min: 1, max: 500, default: 1 }
  on_site: { type: boolean, label: Someone on site }
  visit_on:
    type: date
    label: Visit on
    visible_when: has(record.fields.on_site) && record.fields.on_site
    required_when: has(record.fields.on_site) && record.fields.on_site
  resolution:
    label: How it was fixed
    required_when: record.state_category == "done"
layout:
  create: [title, description, site, seats, on_site, visit_on]
  detail:
    - fields: [resolution]
validate:
  - when: '!has(record.fields.visit_on) || record.fields.visit_on > now - duration("24h")'
    error: The visit cannot be in the past
`

// siteRule fills the site in for headquarters' incidents, before the form is checked.
const siteRule = `kind: rule
name: hq-site
record_type: incident
timing: before
on: [create]
when: record.title.startsWith("HQ") && !has(record.fields.site)
run:
  - action: set
    field: fields.site
    value: Headquarters
`

// installFormApp replaces incident-management with an app that has its lifecycle and the given form and rules.
func installFormApp(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry, files map[string]string) {
	t.Helper()
	ctx := context.Background()
	if _, err := apps.Uninstall(ctx, pool, "incident-management"); err != nil {
		t.Fatal(err)
	}
	lc, err := os.ReadFile("../../../apps/incident-management/default/lifecycles/incident.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b := map[string][]byte{
		"app.yaml": []byte("apiVersion: 1\nname: sites\nversion: 1.0.0\nauthor: t\ndescription: \"Incidents by site.\"\n" +
			"dependencies:\n  - core-itsm: \">=0.1.0\"\npermissions: [records:read:incident, records:update:incident]\n"),
		"default/lifecycles/incident.yaml": lc,
	}
	for name, src := range files {
		b[name] = []byte(src)
	}
	c, err := (&apps.Bundle{Files: b}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Install(ctx, pool, c, apps.InstallOptions{ApprovePermissions: true}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
}

// A form's rules hold on every save, whoever makes it: required fields, values of the field's type within its limits,
// fields shown and required only when they apply, and checks on the whole record. A rule can fill a field in first.
func TestEverySaveIsHeldToTheForm(t *testing.T) {
	h, pool, reg := setupApps(t)
	installFormApp(t, pool, reg, map[string]string{"default/forms/incident.yaml": siteForm, "default/rules/hq-site.yaml": siteRule})
	agent := newActor(t, h, pool, "agent@x.test", "agent")

	for _, c := range []struct{ body, want string }{
		{`{"title":"Leak"}`, "Site is required"},
		{`{"title":"Leak","fields":{"site":"Leeds","seats":0}}`, "Seats must be at least 1"},
		{`{"title":"Leak","fields":{"site":"Leeds","seats":2.5}}`, "Seats takes a whole number"},
		{`{"title":"Leak","fields":{"site":"Llanfairpwllgwyngyll station"}}`, "Site is longer than 20 characters"},
		{`{"title":"Leak","fields":{"site":"Leeds","on_site":true}}`, "Visit on is required"},
		{`{"title":"Leak","fields":{"site":"Leeds","on_site":true,"visit_on":"2020-01-01"}}`, "The visit cannot be in the past"},
		{`{"title":"Leak","fields":{"site":"Leeds","on_site":true,"visit_on":"next week"}}`, "Visit on takes a date"},
		{`{"title":"Leak","fields":{"site":"Leeds","on_site":"yes"}}`, "Someone on site takes true or false"},
	} {
		refusal(t, agent.do(h, "POST", "/api/v1/tickets", c.body), 422, c.want, c.body)
	}

	inc := decodeBody[formTicket](t, agent.do(h, "POST", "/api/v1/tickets",
		`{"title":"Leak","fields":{"site":"Leeds","on_site":true,"visit_on":"2099-03-04"}}`))
	if inc.Fields["seats"] != float64(1) || inc.Fields["visit_on"] != "2099-03-04" || inc.FieldStates["visit_on"].Hidden {
		t.Fatalf("filed, with the default: %+v", inc)
	}
	if hq := decodeBody[formTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"HQ lift stuck"}`)); hq.Fields["site"] != "Headquarters" {
		t.Errorf("a before rule filled the site in: %+v", hq.Fields)
	}

	// Hidden again, a field is not required; clearing a required one is refused.
	off := decodeBody[formTicket](t, agent.do(h, "PATCH", "/api/v1/tickets/"+inc.ID, `{"fields":{"on_site":false,"visit_on":null}}`))
	if !off.FieldStates["visit_on"].Hidden || off.FieldStates["visit_on"].Required {
		t.Errorf("nobody on site: %+v", off.FieldStates)
	}
	refusal(t, agent.do(h, "PATCH", "/api/v1/tickets/"+inc.ID, `{"fields":{"site":null}}`), 422, "Site is required", "clearing the site")

	// Moves are saves too: resolving needs the resolution notes, under the label the form gives them.
	refusal(t, agent.do(h, "POST", "/api/v1/tickets/"+inc.ID+"/transitions", `{"to":"resolved"}`), 422, "How it was fixed is required", "resolving bare")
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+inc.ID, `{"resolution":"Tightened the valve."}`), 200, "notes")
	if done := decodeBody[formTicket](t, agent.do(h, "POST", "/api/v1/tickets/"+inc.ID+"/transitions", `{"to":"resolved"}`)); done.Status != "resolved" {
		t.Errorf("resolved with notes: %+v", done)
	}
}

// A form's condition that cannot be evaluated refuses the save and says so, as a guard that cannot be checked
// blocks its move; reading the record still works.
func TestABrokenFormConditionRefusesTheSave(t *testing.T) {
	h, pool, reg := setupApps(t)
	broken := strings.Replace(siteForm, "required_when: has(record.fields.on_site) && record.fields.on_site",
		"required_when: record.fields.on_site", 1)
	installFormApp(t, pool, reg, map[string]string{"default/forms/incident.yaml": broken})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	refusal(t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Leak","fields":{"site":"Leeds"}}`), 422,
		"form has a condition that could not be checked", "on_site not set")
	inc := decodeBody[formTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Leak","fields":{"site":"Leeds","on_site":false}}`))
	if inc.ID == "" {
		t.Fatal("filed with on_site set")
	}
}

// An async rule changes extra fields like any other, through the same save, so the form holds for it too.
func TestAnAsyncRuleUpdatesExtraFields(t *testing.T) {
	h, pool, reg := setupApps(t)
	installFormApp(t, pool, reg, map[string]string{"default/forms/incident.yaml": siteForm, "default/rules/count.yaml": `kind: rule
name: big-site
record_type: incident
timing: async
on: [create]
when: record.fields.site == "Leeds"
run:
  - action: record.update
    field: fields.seats
    value: =record.fields.seats + 40
`})
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	startWorker(t, pool, reg)
	inc := decodeBody[formTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Leak","fields":{"site":"Leeds"}}`))
	waitFor(t, "the rule's update", func() bool {
		return decodeBody[formTicket](t, agent.do(h, "GET", "/api/v1/tickets/"+inc.ID, "")).Fields["seats"] == float64(41)
	})
	waitFor(t, "the audit", func() bool {
		return countRows(t, pool, `SELECT count(*) FROM action_runs WHERE rule = 'big-site' AND outcome = 'ok'`) == 1
	})
}
