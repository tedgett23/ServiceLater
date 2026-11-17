package api_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

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

// A change made to fix a problem is listed on it; when the change finishes, the problem is resolved, and with it the
// incidents behind it. A problem not ready to resolve is only told.
func TestAChangeThatFixesAProblemResolvesIt(t *testing.T) {
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	drain := func() {
		t.Helper()
		for range 5 {
			n := runJobs(t, pool, ruleruns.Kind, ruleworker.Handler(pool, reg))
			n += runJobsWith(t, pool, ruleworker.LinkedKind, ruleworker.LinkedHandler(pool, reg), true)
			if n == 0 {
				return
			}
		}
	}
	inc := decodeBody[problemBody](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Mail is slow"}`))
	prb := decodeBody[problemBody](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"problem","title":"Mail store is full","incidents":["`+inc.ID+`"]}`))
	window := `"planned_start":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + `","planned_end":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"`
	w := agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Grow the mail store","problem_id":"`+prb.ID+
		`","change":{"change_type":"standard","implementation_plan":"Grow it.","backout_plan":"Shrink it.",`+window+`}}`)
	expect(t, w, 201, "raising a change from the problem")
	chg := decodeBody[problemBody](t, w)
	if chg.ProblemReference != "PRB1" {
		t.Fatalf("the change's problem: %+v", chg)
	}
	withChanges := decodeBody[struct {
		Changes []struct {
			Reference string `json:"reference"`
		} `json:"changes"`
	}](t, agent.do(h, "GET", "/api/v1/tickets/PRB1", ""))
	if len(withChanges.Changes) != 1 || withChanges.Changes[0].Reference != chg.Reference {
		t.Fatalf("the problem's changes: %+v", withChanges)
	}
	list := decodeBody[struct {
		Items []struct {
			Reference     string `json:"reference"`
			OpenIncidents int    `json:"open_incidents"`
			LinkedChanges int    `json:"linked_changes"`
		} `json:"items"`
	}](t, agent.do(h, "GET", "/api/v1/tickets?type=problem", ""))
	if len(list.Items) != 1 || list.Items[0].OpenIncidents != 1 || list.Items[0].LinkedChanges != 1 {
		t.Errorf("the problems list: %+v", list.Items)
	}

	move := func(ref string, to ...string) {
		t.Helper()
		for _, s := range to {
			expect(t, agent.do(h, "POST", "/api/v1/tickets/"+ref+"/transitions", `{"to":"`+s+`"}`), 200, ref+" to "+s)
		}
	}
	// The problem is still being analysed, so finishing the change only tells it.
	move(chg.Reference, "assess", "scheduled", "implement", "review")
	drain()
	if got := decodeBody[problemBody](t, agent.do(h, "GET", "/api/v1/tickets/PRB1", "")); got.Status != "new" {
		t.Errorf("a problem not ready was moved: %s", got.Status)
	}
	if cs := decodeBody[commentList](t, agent.do(h, "GET", "/api/v1/tickets/PRB1/comments", "")).Items; len(cs) != 1 || !strings.Contains(cs[0].Body, chg.Reference) {
		t.Errorf("the problem was not told: %+v", cs)
	}

	// A second change, once the problem is in Fix in progress with its analysis written, resolves it and its incident.
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/PRB1", `{"fields":{"root_cause":"No retention policy.","fix":"Retention policy and a bigger store."}}`), 200, "the analysis")
	move("PRB1", "assess", "root_cause_analysis", "fix_in_progress")
	chg2 := decodeBody[problemBody](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Add a retention policy","problem_id":"`+prb.ID+
		`","change":{"change_type":"standard","implementation_plan":"Set it.","backout_plan":"Unset it.",`+window+`}}`))
	move(chg2.Reference, "assess", "scheduled", "implement", "review")
	drain()
	if got := decodeBody[problemBody](t, agent.do(h, "GET", "/api/v1/tickets/PRB1", "")); got.Status != "resolved" {
		t.Errorf("the problem after its change: %s", got.Status)
	}
	if got := decodeBody[problemBody](t, agent.do(h, "GET", "/api/v1/tickets/"+inc.Reference, "")); got.Status != "resolved" {
		t.Errorf("the incident after its problem was resolved: %s", got.Status)
	}
	refusal(t, agent.do(h, "PATCH", "/api/v1/tickets/"+chg.Reference, `{"problem_id":"`+chg2.ID+`"}`), 422, "no problem with that id", "a change fixing a change")
}

// The queue filters by extra field: known errors are the problems whose known_error is true.
func TestTheQueueFiltersByExtraField(t *testing.T) {
	h, pool, _ := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	for _, title := range []string{"Known one", "Unknown one"} {
		decodeBody[problemBody](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"problem","title":"`+title+`"}`))
	}
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/PRB1", `{"fields":{"known_error":true,"workaround":"Restart."}}`), 200, "a known error")
	got := decodeBody[struct {
		Items []struct {
			Title string `json:"title"`
		} `json:"items"`
	}](t, agent.do(h, "GET", "/api/v1/tickets?type=problem&fields.known_error=true", ""))
	if len(got.Items) != 1 || got.Items[0].Title != "Known one" {
		t.Errorf("known errors: %+v", got.Items)
	}
	expect(t, agent.do(h, "GET", "/api/v1/tickets?fields.Bad-Name=x", ""), 400, "a bad field name")
}

type suggestionList struct {
	Items []struct {
		Reference string         `json:"reference"`
		Why       string         `json:"why"`
		Fields    map[string]any `json:"fields"`
	} `json:"items"`
}

func suggested(s suggestionList) []string {
	out := []string{}
	for _, i := range s.Items {
		out = append(out, i.Reference+" "+i.Why)
	}
	return out
}

// Incidents and problems suggest each other: by a similar title, or by an affected item they share, which counts for
// more. Settled problems, linked incidents and unrelated titles are not suggested; nothing is suggested to someone
// who cannot see it.
func TestIncidentsAndProblemsSuggestEachOther(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	file := func(body string) problemBody {
		t.Helper()
		w := agent.do(h, "POST", "/api/v1/tickets", body)
		expect(t, w, 201, "filing "+body)
		return decodeBody[problemBody](t, w)
	}
	vpn := file(`{"type":"problem","title":"VPN concentrator drops idle sessions"}`)
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+vpn.ID, `{"fields":{"known_error":true,"workaround":"Use the backup profile."}}`), 200, "a known error")
	file(`{"type":"problem","title":"Printers lose their queue"}`)
	batteries := file(`{"type":"problem","title":"Batch 7 batteries swell"}`)
	file(`{"type":"problem","title":"VPN certificate renewals fail"}`) // one word of five in common with the incident: not alike enough
	old := file(`{"type":"problem","title":"VPN certificate expired"}`)
	expect(t, agent.do(h, "POST", "/api/v1/tickets/"+old.ID+"/transitions", `{"to":"cancelled"}`), 200, "settling an old problem")

	inc := file(`{"title":"VPN drops every 20 minutes"}`)
	got := decodeBody[suggestionList](t, agent.do(h, "GET", "/api/v1/tickets/"+inc.ID+"/suggestions", ""))
	if r := suggested(got); len(r) != 1 || r[0] != vpn.Reference+" Similar title" || got.Items[0].Fields["workaround"] != "Use the backup profile." {
		t.Errorf("problems for a VPN incident: %v %+v", r, got.Items)
	}

	// An incident with nothing in common with the title, but on the same laptop as a problem.
	laptop := decodeBody[struct {
		ID string `json:"id"`
	}](t, admin.do(h, "POST", "/api/v1/cis", `{"class":"other","name":"LT-0042"}`))
	dead := file(`{"title":"Will not switch on","cis":["` + laptop.ID + `"]}`)
	expect(t, agent.do(h, "PUT", "/api/v1/tickets/"+batteries.ID+"/cis/"+laptop.ID, ""), 200, "the problem's item")
	if r := suggested(decodeBody[suggestionList](t, agent.do(h, "GET", "/api/v1/tickets/"+dead.ID+"/suggestions", ""))); len(r) != 1 || r[0] != batteries.Reference+" Same affected item" {
		t.Errorf("problems for an incident on the same item: %v", r)
	}

	// The problem suggests the open incidents not linked yet; a linked one drops out.
	if r := suggested(decodeBody[suggestionList](t, agent.do(h, "GET", "/api/v1/tickets/"+vpn.ID+"/suggestions", ""))); len(r) != 1 || r[0] != inc.Reference+" Similar title" {
		t.Errorf("incidents for the VPN problem: %v", r)
	}
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+inc.ID, `{"problem_id":"`+vpn.ID+`"}`), 200, "linking it")
	if r := suggested(decodeBody[suggestionList](t, agent.do(h, "GET", "/api/v1/tickets/"+vpn.ID+"/suggestions", ""))); len(r) != 0 {
		t.Errorf("a linked incident is still suggested: %v", r)
	}
	if r := suggested(decodeBody[suggestionList](t, agent.do(h, "GET", "/api/v1/tickets/"+inc.ID+"/suggestions", ""))); len(r) != 0 {
		t.Errorf("a linked incident is still offered problems: %v", r)
	}

	// While filing: the words typed so far.
	// Ranked: two words of three in common first, then the problem sharing only "VPN".
	if r := suggested(decodeBody[suggestionList](t, agent.do(h, "GET", "/api/v1/tickets/suggest?type=problem&q=vpn+keeps+dropping", ""))); len(r) != 2 || r[0] != vpn.Reference+" Similar title" {
		t.Errorf("problems while filing: %v", r)
	}
	// One distinctive word in three is enough to be offered (synonyms such as disconnects and drops are not known).
	if r := suggested(decodeBody[suggestionList](t, agent.do(h, "GET", "/api/v1/tickets/suggest?type=problem&q=VPN+disconnects+all+the+time", ""))); !slices.Contains(r, vpn.Reference+" Similar title") {
		t.Errorf("one word in three in common: %v", r)
	}
	if r := suggested(decodeBody[suggestionList](t, agent.do(h, "GET", "/api/v1/tickets/suggest?type=problem&q=coffee+machine", ""))); len(r) != 0 {
		t.Errorf("an unrelated text: %v", r)
	}
	if r := suggested(decodeBody[suggestionList](t, lena.do(h, "GET", "/api/v1/tickets/suggest?type=problem&q=vpn+keeps+dropping", ""))); len(r) != 0 {
		t.Errorf("an end user is shown problems: %v", r)
	}
	expect(t, agent.do(h, "GET", "/api/v1/tickets/suggest?type=problem&q=vp", ""), 400, "too short")
	expect(t, agent.do(h, "GET", "/api/v1/tickets/suggest?type=spaceship&q=vpn", ""), 400, "an unknown type")
}
