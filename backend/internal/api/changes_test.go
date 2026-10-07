package api_test

import (
	"strings"
	"testing"
	"time"
)

type changeBody struct {
	ChangeType         string     `json:"change_type"`
	Risk               string     `json:"risk"`
	Impact             string     `json:"impact"`
	PlannedStart       *time.Time `json:"planned_start"`
	PlannedEnd         *time.Time `json:"planned_end"`
	ImplementationPlan string     `json:"implementation_plan"`
	BackoutPlan        string     `json:"backout_plan"`
	TestPlan           string     `json:"test_plan"`
}

type changeTicket struct {
	ID     string      `json:"id"`
	Type   string      `json:"type"`
	Change *changeBody `json:"change"`
}

// A change has fields of its own: filled in when it is filed or later, checked like any other field, read by
// everyone who can see the change and written only by those who may update changes.
func TestChangeFields(t *testing.T) {
	h, pool := setup(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")

	c := decodeBody[changeTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Swap the core switch",
		"requester_id":"`+lena.id+`","change":{"risk":"medium","planned_start":"2026-11-01T22:00:00Z","planned_end":"2026-11-02T02:00:00Z",
		"backout_plan":"Put the old switch back."}}`))
	if c.Change == nil || c.Change.ChangeType != "normal" || c.Change.Risk != "medium" || c.Change.Impact != "" ||
		c.Change.PlannedStart == nil || !c.Change.PlannedEnd.Equal(time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)) ||
		c.Change.BackoutPlan != "Put the old switch back." {
		t.Fatalf("filed with its fields: %+v", c.Change)
	}
	if inc := agent.do(h, "POST", "/api/v1/tickets", `{"title":"Printer"}`); strings.Contains(inc.Body.String(), `"change"`) {
		t.Errorf("an incident has no change fields: %s", inc.Body)
	}

	edited := decodeBody[changeTicket](t, agent.do(h, "PATCH", "/api/v1/tickets/"+c.ID,
		`{"change":{"change_type":"emergency","impact":"high","planned_start":null,"test_plan":"Ping every floor."}}`))
	if e := edited.Change; e.ChangeType != "emergency" || e.Impact != "high" || e.Risk != "medium" || e.PlannedStart != nil ||
		e.PlannedEnd == nil || e.TestPlan != "Ping every floor." || e.BackoutPlan != "Put the old switch back." {
		t.Errorf("a patch changes what it names and leaves the rest: %+v", e)
	}

	for body, want := range map[string]string{
		`{"change":{"risk":"extreme"}}`:                       "risk must be low, medium or high",
		`{"change":{"change_type":"minor"}}`:                  "change_type must be standard, normal or emergency",
		`{"change":{"planned_start":"2026-11-03T00:00:00Z"}}`: "the planned end must be after the planned start",
	} {
		refusal(t, agent.do(h, "PATCH", "/api/v1/tickets/"+c.ID, body), 422, want, body)
	}
	refusal(t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Printer","change":{"risk":"low"}}`), 422,
		"only a change has change fields", "change fields on a new incident")
	inc := decodeBody[changeTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Scanner"}`))
	refusal(t, agent.do(h, "PATCH", "/api/v1/tickets/"+inc.ID, `{"change":{"risk":"low"}}`), 422,
		"only a change has change fields", "change fields on an incident")

	// The requester reads the change's fields, and cannot write them.
	seen := decodeBody[changeTicket](t, lena.do(h, "GET", "/api/v1/tickets/"+c.ID, ""))
	if seen.Change == nil || seen.Change.TestPlan != "Ping every floor." {
		t.Errorf("the requester's view: %+v", seen.Change)
	}
	expect(t, lena.do(h, "PATCH", "/api/v1/tickets/"+c.ID, `{"change":{"risk":"low"}}`), 403, "the requester editing a change")
}

// Rules for changes can rate a change: risk and impact are settable, checked against their lists at install and
// at the save, and only rules for changes may set them.
func TestRulesCanRateAChange(t *testing.T) {
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	installRules(t, pool, reg, map[string]string{
		"rate": "kind: rule\nname: rate\nrecord_type: change\ntiming: before\non: [create, update]\n" +
			"when: record.impact == 'high' && record.change_type == 'emergency'\n" +
			"run:\n  - action: set\n    field: risk\n    value: high\n",
	})
	c := decodeBody[changeTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Hotfix","change":{"change_type":"emergency"}}`))
	if c.Change.Risk != "" {
		t.Fatalf("not rated yet: %+v", c.Change)
	}
	c = decodeBody[changeTicket](t, agent.do(h, "PATCH", "/api/v1/tickets/"+c.ID, `{"change":{"impact":"high"}}`))
	if c.Change.Risk != "high" {
		t.Errorf("the rule rates an emergency change with high impact as high risk: %+v", c.Change)
	}
}
