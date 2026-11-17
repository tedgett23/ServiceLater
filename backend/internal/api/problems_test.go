package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"

	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/ruleworker"
)

type problemBody struct {
	ID               string `json:"id"`
	Reference        string `json:"reference"`
	Status           string `json:"status"`
	Resolution       string `json:"resolution"`
	ProblemID        string `json:"problem_id"`
	ProblemReference string `json:"problem_reference"`
	ProblemTitle     string `json:"problem_title"`
	OpenIncidents    int    `json:"open_incidents"`
	Incidents        []struct {
		Reference     string `json:"reference"`
		StateCategory string `json:"state_category"`
	} `json:"incidents"`
}

// Incidents are linked to the problem behind them: the problem lists them, a known error tells them the workaround,
// and resolving the problem resolves those still open, each through its own save.
func TestAProblemResolvesItsIncidents(t *testing.T) {
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	jobsRun := func() {
		t.Helper()
		runJobs(t, pool, ruleruns.Kind, ruleworker.Handler(pool, reg))
		runJobs(t, pool, ruleworker.LinkedKind, ruleworker.LinkedHandler(pool, reg))
	}
	file := func(who actor, title string) problemBody {
		t.Helper()
		w := who.do(h, "POST", "/api/v1/tickets", `{"title":"`+title+`"}`)
		expect(t, w, 201, "filing "+title)
		return decodeBody[problemBody](t, w)
	}
	a, b, c := file(lena, "VPN drops"), file(agent, "VPN drops again"), file(agent, "Printer jam")

	refusal(t, lena.do(h, "POST", "/api/v1/tickets", `{"type":"problem","title":"Mine"}`), 403, "records:create:problem", "an end user filing a problem")
	refusal(t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"x","incidents":["`+a.ID+`"]}`), 422, "only a problem", "incidents on an incident")
	w := agent.do(h, "POST", "/api/v1/tickets", `{"type":"problem","title":"VPN concentrator drops sessions","incidents":["`+a.ID+`","`+b.ID+`"]}`)
	expect(t, w, 201, "raising a problem from two incidents")
	prb := decodeBody[problemBody](t, w)
	if prb.Reference != "PRB1" || prb.Status != "new" || len(prb.Incidents) != 2 || prb.OpenIncidents != 2 {
		t.Fatalf("the problem: %+v", prb)
	}
	got := decodeBody[problemBody](t, agent.do(h, "GET", "/api/v1/tickets/"+a.Reference, ""))
	if got.ProblemID != prb.ID || got.ProblemReference != "PRB1" || got.ProblemTitle != "VPN concentrator drops sessions" {
		t.Errorf("the incident's problem: %+v", got)
	}
	// The requester sees her incident is linked, but not the problem itself.
	if got := decodeBody[problemBody](t, lena.do(h, "GET", "/api/v1/tickets/"+a.Reference, "")); got.ProblemID != prb.ID || got.ProblemReference != "" {
		t.Errorf("what the requester sees of the link: %+v", got)
	}

	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+c.ID, `{"problem_id":"`+prb.ID+`"}`), 200, "linking a third")
	if got := decodeBody[problemBody](t, agent.do(h, "GET", "/api/v1/tickets/PRB1", "")); len(got.Incidents) != 3 {
		t.Errorf("after linking: %+v", got.Incidents)
	}
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+c.ID, `{"problem_id":null}`), 200, "unlinking it")
	if body := agent.do(h, "GET", "/api/v1/tickets/"+c.ID+"/history", "").Body.String(); !strings.Contains(body, `"Problem"`) ||
		!strings.Contains(body, "PRB1 VPN concentrator drops sessions") {
		t.Errorf("the link is not in the incident's history by reference: %s", body)
	}
	refusal(t, agent.do(h, "PATCH", "/api/v1/tickets/"+c.ID, `{"problem_id":"`+a.ID+`"}`), 422, "no problem with that id", "linking to an incident")
	refusal(t, agent.do(h, "PATCH", "/api/v1/tickets/PRB1", `{"problem_id":"`+prb.ID+`"}`), 422, "only an incident", "linking a problem")
	refusal(t, lena.do(h, "PATCH", "/api/v1/tickets/"+a.ID, `{"problem_id":null}`), 403, "", "the requester unlinking")

	// A known error needs a workaround, and tells the open incidents what it is.
	refusal(t, agent.do(h, "PATCH", "/api/v1/tickets/PRB1", `{"fields":{"known_error":true}}`), 422, "needs a workaround", "a known error without one")
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/PRB1", `{"fields":{"known_error":true,"workaround":"Reconnect with the backup profile."}}`), 200, "marking a known error")
	jobsRun()
	for _, ref := range []string{a.Reference, b.Reference} {
		cs := decodeBody[commentList](t, agent.do(h, "GET", "/api/v1/tickets/"+ref+"/comments", "")).Items
		if len(cs) != 1 || !strings.Contains(cs[0].Body, "Reconnect with the backup profile.") {
			t.Errorf("%s was not told the workaround: %+v", ref, cs)
		}
	}
	if cs := decodeBody[commentList](t, lena.do(h, "GET", "/api/v1/tickets/"+a.Reference+"/comments", "")).Items; len(cs) != 1 {
		t.Errorf("the requester does not see the workaround: %+v", cs)
	}

	// B is resolved by hand first, so resolving the problem leaves it as it is.
	expect(t, agent.do(h, "POST", "/api/v1/tickets/"+b.ID+"/transitions", `{"to":"resolved"}`), 200, "resolving B")
	for _, to := range []string{"assess", "root_cause_analysis"} {
		expect(t, agent.do(h, "POST", "/api/v1/tickets/PRB1/transitions", `{"to":"`+to+`"}`), 200, "moving to "+to)
	}
	refusal(t, agent.do(h, "POST", "/api/v1/tickets/PRB1/transitions", `{"to":"fix_in_progress"}`), 422, "root cause first", "fixing with no root cause")
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/PRB1", `{"fields":{"root_cause":"A firmware bug drops idle sessions."}}`), 200, "the root cause")
	expect(t, agent.do(h, "POST", "/api/v1/tickets/PRB1/transitions", `{"to":"fix_in_progress"}`), 200, "starting the fix")
	refusal(t, agent.do(h, "POST", "/api/v1/tickets/PRB1/transitions", `{"to":"resolved"}`), 422, "what the fix was", "resolving with no fix")
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/PRB1", `{"fields":{"fix":"Firmware 9.2 on both concentrators."}}`), 200, "the fix")
	expect(t, agent.do(h, "POST", "/api/v1/tickets/PRB1/transitions", `{"to":"resolved"}`), 200, "resolving the problem")
	jobsRun()

	got = decodeBody[problemBody](t, agent.do(h, "GET", "/api/v1/tickets/"+a.Reference, ""))
	if got.Status != "resolved" || got.Resolution != "Fixed by PRB1: Firmware 9.2 on both concentrators." {
		t.Errorf("A after the problem was resolved: %+v", got)
	}
	cs := decodeBody[commentList](t, agent.do(h, "GET", "/api/v1/tickets/"+a.Reference+"/comments", "")).Items
	if len(cs) != 2 || !strings.Contains(cs[1].Body, "Resolved with problem PRB1") {
		t.Errorf("A's comments: %+v", cs)
	}
	if got := decodeBody[problemBody](t, agent.do(h, "GET", "/api/v1/tickets/"+b.Reference, "")); got.Resolution != "" {
		t.Errorf("B, already resolved, was changed: %+v", got)
	}
	if got := decodeBody[problemBody](t, agent.do(h, "GET", "/api/v1/tickets/PRB1", "")); got.OpenIncidents != 0 || len(got.Incidents) != 2 {
		t.Errorf("the problem afterwards: %+v", got)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM action_runs WHERE rule = 'resolve-linked-incidents' AND outcome = 'ok'"); n != 2 {
		t.Errorf("%d ok runs of resolve-linked-incidents, want 2 (the problem's and A's)", n)
	}
	// A problem its incidents point at cannot be deleted from under them.
	if n := countRows(t, pool, "SELECT count(*) FROM incidents WHERE problem_id = $1", prb.ID); n != 2 {
		t.Errorf("%d incidents still linked", n)
	}
}

// One incident that refuses the change does not hold back the others: it stays open, and the refusal is recorded.
func TestALinkedIncidentThatRefusesStaysOpen(t *testing.T) {
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	installRules(t, pool, reg, map[string]string{"keep-vip": "kind: rule\nname: keep-vip\nrecord_type: incident\ntiming: before\non: [update]\n" +
		"when: record.title.startsWith(\"VIP\") && record.status == \"resolved\"\nrun:\n  - action: abort\n    message: VIP incidents are resolved by hand.\n"})
	ids := []string{}
	for _, title := range []string{"VIP laptop dead", "Laptop dead"} {
		ids = append(ids, decodeBody[problemBody](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"`+title+`"}`)).ID)
	}
	prb := decodeBody[problemBody](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"problem","title":"Bad batch","incidents":["`+ids[0]+`","`+ids[1]+`"]}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+prb.ID, `{"fields":{"root_cause":"Batch 7 batteries.","fix":"Recalled batch 7."}}`), 200, "the analysis")
	for _, to := range []string{"assess", "root_cause_analysis", "fix_in_progress", "resolved"} {
		expect(t, agent.do(h, "POST", "/api/v1/tickets/"+prb.ID+"/transitions", `{"to":"`+to+`"}`), 200, "moving to "+to)
	}
	runJobs(t, pool, ruleruns.Kind, ruleworker.Handler(pool, reg))
	runJobsWith(t, pool, ruleworker.LinkedKind, ruleworker.LinkedHandler(pool, reg), true)

	vip := decodeBody[problemBody](t, agent.do(h, "GET", "/api/v1/tickets/"+ids[0], ""))
	other := decodeBody[problemBody](t, agent.do(h, "GET", "/api/v1/tickets/"+ids[1], ""))
	if vip.Status != "new" || vip.Resolution != "" || other.Status != "resolved" {
		t.Errorf("VIP %s %q, other %s", vip.Status, vip.Resolution, other.Status)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM action_runs WHERE rule = 'resolve-linked-incidents' AND outcome = 'failed' AND detail LIKE '%resolved by hand%'"); n != 1 {
		t.Errorf("%d failed runs recorded, want 1", n)
	}
}

// withoutProblemManagement uninstalls problem-management, for tests that need a registered type no app provides
// (problem was that type until the app shipped).
func withoutProblemManagement(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry) {
	t.Helper()
	if _, err := apps.Uninstall(context.Background(), pool, "problem-management"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
}
