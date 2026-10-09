package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

type catalogList struct {
	Items []struct {
		Name     string `json:"name"`
		Label    string `json:"label"`
		Category string `json:"category"`
	} `json:"items"`
}

func names(l catalogList) string {
	var out []string
	for _, it := range l.Items {
		out = append(out, it.Name)
	}
	return strings.Join(out, " ")
}

func makeGroup(t *testing.T, h http.Handler, admin actor, name string, members ...actor) string {
	t.Helper()
	g := decodeBody[struct {
		ID string `json:"id"`
	}](t, admin.do(h, "POST", "/api/v1/groups", fmt.Sprintf(`{"name":%q}`, name)))
	for _, m := range members {
		expect(t, admin.do(h, "PUT", "/api/v1/groups/"+g.ID+"/members/"+m.id, ""), 204, "adding a member")
	}
	return g.ID
}

// The catalog shows each person the items open to them, and nobody can order one they may not see; ordering checks
// the answers against the item's questions and names the request after them.
func TestTheCatalogShowsWhatEachPersonMayOrder(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	mia := newActor(t, h, pool, "mia@x.test", "end_user")
	makeGroup(t, h, admin, "People managers", mia)

	if got := names(decodeBody[catalogList](t, lena.do(h, "GET", "/api/v1/catalog", ""))); got != "something-else new-laptop software-access" {
		t.Errorf("lena's catalog: %s", got)
	}
	if got := names(decodeBody[catalogList](t, mia.do(h, "GET", "/api/v1/catalog", ""))); !strings.Contains(got, "new-starter") {
		t.Errorf("a people manager's catalog: %s", got)
	}
	if got := names(decodeBody[catalogList](t, agent.do(h, "GET", "/api/v1/catalog", ""))); !strings.Contains(got, "new-starter") {
		t.Errorf("an agent's catalog: %s", got)
	}
	expect(t, lena.do(h, "GET", "/api/v1/catalog/new-starter", ""), 404, "an item lena may not order")
	expect(t, lena.do(h, "POST", "/api/v1/catalog/new-starter/request", `{"fields":{"starter_name":"Sam","start_date":"2026-11-02"}}`), 404, "ordering it anyway")
	item := decodeBody[struct {
		Form struct {
			Create []string `json:"create"`
		} `json:"form"`
		CanOrderForOthers bool `json:"can_order_for_others"`
	}](t, lena.do(h, "GET", "/api/v1/catalog/new-laptop", ""))
	if strings.Join(item.Form.Create, ",") != "model,replacing,old_asset_tag,needed_by,reason" || item.CanOrderForOthers {
		t.Errorf("the laptop item: %+v", item)
	}

	// The answers are checked like any form: required ones, conditions, options.
	path := "/api/v1/catalog/new-laptop/request"
	refusal(t, lena.do(h, "POST", path, `{"fields":{"model":"MacBook"}}`), 422, "What do you need it for?", "a required answer missing")
	refusal(t, lena.do(h, "POST", path, `{"fields":{"model":"Tablet","reason":"x"}}`), 422, "Model", "an option the item does not offer")
	refusal(t, lena.do(h, "POST", path, `{"fields":{"model":"MacBook","reason":"x","replacing":true}}`), 422, "Asset tag", "a condition's required answer")
	states := decodeBody[struct {
		States map[string]struct {
			Hidden bool `json:"hidden"`
		} `json:"states"`
	}](t, lena.do(h, "POST", "/api/v1/catalog/new-laptop/states", `{"fields":{"replacing":true}}`))
	if states.States["old_asset_tag"].Hidden {
		t.Errorf("the asset tag is hidden while replacing: %+v", states)
	}
	refusal(t, lena.do(h, "POST", path, `{"fields":{"model":"MacBook","reason":"x"},"requester_id":"`+mia.id+`"}`), 403,
		"records:update:request", "ordering for someone else")

	w := lena.do(h, "POST", path, `{"fields":{"model":"MacBook","reason":"Design work"}}`)
	expect(t, w, 201, "ordering a laptop")
	req := decodeBody[struct {
		Reference   string         `json:"reference"`
		Title       string         `json:"title"`
		CatalogItem string         `json:"catalog_item"`
		Fields      map[string]any `json:"fields"`
		Requester   string         `json:"requester_id"`
	}](t, w)
	if req.Reference != "REQ1" || req.Title != "New laptop: MacBook" || req.CatalogItem != "new-laptop" || req.Fields["reason"] != "Design work" || req.Requester != lena.id {
		t.Errorf("the request: %+v", req)
	}
	// Its answers are laid out by the item's questions, and it ran the item's workflow.
	if f := decodeBody[struct {
		Create []string `json:"create"`
	}](t, lena.do(h, "GET", "/api/v1/forms/request?item=new-laptop", "")); len(f.Create) != 5 {
		t.Errorf("the request's form: %+v", f)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM workflow_executions WHERE workflow = 'laptop'`); n != 1 {
		t.Errorf("%d laptop workflows", n)
	}
	// An agent orders for someone else, and nobody files a request outside the catalog without records:create:request.
	expect(t, agent.do(h, "POST", "/api/v1/catalog/something-else/request", `{"fields":{"what":"A desk lamp"},"description":"For the reading room.","requester_id":"`+lena.id+`"}`),
		201, "an agent ordering for lena")
	refusal(t, lena.do(h, "POST", "/api/v1/tickets", `{"type":"request","title":"A pony"}`), 403, "outside the catalog", "a request with no item")
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"request","title":"Filed by hand"}`), 201, "staff filing one by hand")
}

// Ordering a laptop runs its workflow end to end: the manager approves, then Procurement, Desktop Support and the
// Service Desk each get a task in turn, and the request is fulfilled when the last is done, telling the requester.
func TestALaptopIsApprovedOrderedSetUpAndHandedOver(t *testing.T) {
	h, pool, reg := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	mia := newActor(t, h, pool, "mia@x.test", "end_user")
	for _, g := range []string{"Procurement", "Desktop Support", "Service Desk"} {
		makeGroup(t, h, admin, g, agent)
	}
	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+lena.id, `{"manager_id":"`+mia.id+`"}`), 200, "setting lena's manager")
	startFlowWorker(t, pool, reg)

	req := decodeBody[requestBody](t, lena.do(h, "POST", "/api/v1/catalog/new-laptop/request",
		`{"fields":{"model":"Standard 14-inch","reason":"Mine broke","replacing":true,"old_asset_tag":"LT-0042"}}`))
	waitFor(t, "the request to wait for approval", func() bool {
		return decodeBody[requestBody](t, lena.do(h, "GET", "/api/v1/tickets/"+req.ID, "")).Status == "approval"
	})
	type approval struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	var mine []approval
	waitFor(t, "mia to be asked", func() bool {
		mine = decodeBody[struct{ Items []approval }](t, mia.do(h, "GET", "/api/v1/approvals", "")).Items
		return len(mine) == 1
	})
	expect(t, mia.do(h, "POST", "/api/v1/approvals/"+mine[0].ID+"/decide", `{"decision":"approved","comment":"Go ahead"}`), 200, "mia approving")

	for i, want := range []struct{ title, group string }{
		{"Order a Standard 14-inch laptop for lena@x.test", "Procurement"},
		{"Set up the laptop for lena@x.test", "Desktop Support"},
		{"Hand the laptop over to lena@x.test", "Service Desk"},
	} {
		var got requestBody
		waitFor(t, "task "+want.title, func() bool {
			got = decodeBody[requestBody](t, agent.do(h, "GET", "/api/v1/tickets/"+req.ID, ""))
			return len(got.Tasks) == i+1
		})
		task := got.Tasks[i]
		if task.Title != want.title || task.AssignmentGroupName == nil || *task.AssignmentGroupName != want.group || got.Status != "in_progress" {
			t.Fatalf("task %d: %+v (request %s)", i+1, task, got.Status)
		}
		expect(t, agent.do(h, "POST", "/api/v1/tickets/"+task.Reference+"/transitions", `{"to":"done"}`), 200, "finishing "+task.Reference)
	}
	waitFor(t, "the request to be fulfilled", func() bool {
		return decodeBody[requestBody](t, lena.do(h, "GET", "/api/v1/tickets/"+req.ID, "")).Status == "fulfilled"
	})
	if got := lena.do(h, "GET", "/api/v1/notifications", "").Body.String(); !strings.Contains(got, req.Reference+" is fulfilled") {
		t.Errorf("lena is not told: %s", got)
	}
	if got := agent.do(h, "GET", "/api/v1/tickets/TASK3", "").Body.String(); !strings.Contains(got, "asset tag LT-0042") {
		t.Errorf("the hand-over task does not say what to collect: %s", got)
	}
}

// Someone with no manager skips that approval; refusing one rejects the request and tells the requester why.
func TestAManagerCanSayNo(t *testing.T) {
	h, pool, reg := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	mia := newActor(t, h, pool, "mia@x.test", "end_user")
	omar := newActor(t, h, pool, "omar@x.test", "end_user")
	expect(t, admin.do(h, "PATCH", "/api/v1/users/"+lena.id, `{"manager_id":"`+mia.id+`"}`), 200, "setting lena's manager")
	startFlowWorker(t, pool, reg)

	refused := decodeBody[requestBody](t, lena.do(h, "POST", "/api/v1/catalog/software-access/request",
		`{"fields":{"application":"Figma","access_level":"standard","reason":"Mock-ups"}}`))
	type approval struct {
		ID string `json:"id"`
	}
	var mine []approval
	waitFor(t, "mia to be asked", func() bool {
		mine = decodeBody[struct{ Items []approval }](t, mia.do(h, "GET", "/api/v1/approvals", "")).Items
		return len(mine) == 1
	})
	expect(t, mia.do(h, "POST", "/api/v1/approvals/"+mine[0].ID+"/decide", `{"decision":"rejected","comment":"Use the shared licence"}`), 200, "mia refusing")
	waitFor(t, "the request to be rejected", func() bool {
		return decodeBody[requestBody](t, lena.do(h, "GET", "/api/v1/tickets/"+refused.ID, "")).Status == "rejected"
	})
	if got := lena.do(h, "GET", "/api/v1/notifications", "").Body.String(); !strings.Contains(got, refused.Reference+" was not approved") {
		t.Errorf("lena is not told: %s", got)
	}

	// Omar has no manager: his request goes straight to work, a task for the Service Desk (unrouted: no such group).
	direct := decodeBody[requestBody](t, omar.do(h, "POST", "/api/v1/catalog/software-access/request",
		`{"fields":{"application":"Figma","access_level":"read-only","reason":"Reviews"}}`))
	waitFor(t, "omar's task", func() bool {
		got := decodeBody[requestBody](t, omar.do(h, "GET", "/api/v1/tickets/"+direct.ID, ""))
		return got.Status == "in_progress" && len(got.Tasks) == 1
	})
}
