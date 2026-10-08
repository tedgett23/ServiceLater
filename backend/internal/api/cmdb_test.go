package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

type ciBody struct {
	ID               string         `json:"id"`
	Class            string         `json:"class"`
	ClassLabel       string         `json:"class_label"`
	Name             string         `json:"name"`
	Status           string         `json:"status"`
	SupportGroupName *string        `json:"support_group_name"`
	Fields           map[string]any `json:"fields"`
	Relationships    []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Label    string `json:"label"`
		Outgoing bool   `json:"outgoing"`
		Impact   bool   `json:"impact"`
		Item     ciBody `json:"item"`
	} `json:"relationships"`
	Tickets []struct {
		Reference string `json:"reference"`
		Title     string `json:"title"`
	} `json:"tickets"`
}

type impactBody struct {
	Items []struct {
		Name    string `json:"name"`
		Depth   int    `json:"depth"`
		ViaName string `json:"via_name"`
	} `json:"items"`
	Truncated bool `json:"truncated"`
}

func ciNames(items []ciBody) []string {
	out := make([]string, len(items))
	for i, c := range items {
		out[i] = c.Name
	}
	return out
}

// Agents read the CMDB, admins and CMDB managers change it, and end users see none of it.
func TestTheCMDBIsReadByAgentsAndKeptByManagers(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	keeper := newActor(t, h, pool, "keeper@x.test", "end_user")
	expect(t, admin.do(h, "POST", "/api/v1/roles/cmdb-manager/grants", `{"user_id":"`+keeper.id+`"}`), 201, "a CMDB manager")

	model := decodeBody[struct {
		Classes []struct {
			Name      string
			Ancestors []string
			Fields    []struct{ Name string }
		}
		Relationships []struct {
			Name   string
			Impact bool
		}
		Statuses []struct{ Value string }
	}](t, agent.do(h, "GET", "/api/v1/cmdb/model", ""))
	var server []string
	for _, c := range model.Classes {
		if c.Name == "server" {
			for _, f := range c.Fields {
				server = append(server, f.Name)
			}
			if len(c.Ancestors) != 1 || c.Ancestors[0] != "hardware" {
				t.Errorf("a server is hardware: %v", c.Ancestors)
			}
		}
	}
	if len(model.Classes) != 10 || len(server) == 0 || server[0] != "manufacturer" || !strings.Contains(strings.Join(server, ","), "hostname") {
		t.Errorf("the shipped classes, a server with hardware's fields first: %d classes, server %v", len(model.Classes), server)
	}
	if len(model.Relationships) != 4 || len(model.Statuses) != 4 {
		t.Errorf("relationships and statuses: %+v %+v", model.Relationships, model.Statuses)
	}

	group := decodeBody[struct{ ID string }](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Database team"}`))
	db := decodeBody[ciBody](t, keeper.do(h, "POST", "/api/v1/cis",
		`{"class":"database","name":"pg-01","support_group_id":"`+group.ID+`","fields":{"engine":"postgresql","version":"17"}}`))
	if db.Status != "active" || db.ClassLabel != "Database" || db.SupportGroupName == nil || *db.SupportGroupName != "Database team" || db.Fields["engine"] != "postgresql" {
		t.Fatalf("added: %+v", db)
	}
	expect(t, agent.do(h, "POST", "/api/v1/cis", `{"class":"other","name":"Mine"}`), 403, "an agent adding one")
	expect(t, agent.do(h, "PATCH", "/api/v1/cis/"+db.ID, `{"name":"Mine"}`), 403, "an agent changing one")
	expect(t, lena.do(h, "GET", "/api/v1/cis", ""), 403, "an end user listing them")
	expect(t, lena.do(h, "GET", "/api/v1/cis/"+db.ID, ""), 403, "an end user reading one")
	expect(t, lena.do(h, "GET", "/api/v1/cmdb/model", ""), 403, "an end user reading the model")

	for body, want := range map[string]string{
		`{"class":"database","name":"PG-01"}`:                          "there is already a database called PG-01",
		`{"class":"spaceship","name":"x"}`:                             "there is no class",
		`{"class":"database","name":"x","fields":{"engine":"sqlite"}}`: "Engine must be one of",
		`{"class":"database","name":"x","fields":{"hostname":"a"}}`:    "Database items have no field",
		`{"class":"other","name":" "}`:                                 "name must be 1 to 200 characters",
		`{"class":"other","name":"x","status":"broken"}`:               "status must be planned, active, maintenance or retired",
		`{"class":"server","name":"x","fields":{"virtual":"yes"}}`:     "Virtual machine takes true or false",
	} {
		code := http.StatusUnprocessableEntity
		if strings.Contains(want, "already") {
			code = http.StatusConflict
		}
		refusal(t, admin.do(h, "POST", "/api/v1/cis", body), code, want, body)
	}

	// A server is hardware: filtering by hardware finds it, and retired items are left out unless asked for.
	srv := decodeBody[ciBody](t, admin.do(h, "POST", "/api/v1/cis", `{"class":"server","name":"db-host-1","fields":{"serial_number":"SN1","virtual":false}}`))
	decodeBody[ciBody](t, admin.do(h, "POST", "/api/v1/cis", `{"class":"computer","name":"old-laptop","status":"retired"}`))
	list := func(who actor, query string) []ciBody {
		return decodeBody[struct{ Items []ciBody }](t, who.do(h, "GET", "/api/v1/cis"+query, "")).Items
	}
	if got := ciNames(list(agent, "?class=hardware")); strings.Join(got, ",") != "db-host-1" {
		t.Errorf("hardware: %v", got)
	}
	if got := ciNames(list(agent, "?class=hardware&include_retired=true")); strings.Join(got, ",") != "db-host-1,old-laptop" {
		t.Errorf("hardware, retired too: %v", got)
	}
	if got := ciNames(list(agent, "?q=PG")); strings.Join(got, ",") != "pg-01" {
		t.Errorf("by name: %v", got)
	}

	// A patch changes what it names; null clears a field; moving class keeps only the fields the new class has.
	moved := decodeBody[ciBody](t, keeper.do(h, "PATCH", "/api/v1/cis/"+srv.ID, `{"fields":{"serial_number":null,"hostname":"db1"},"status":"maintenance"}`))
	if moved.Status != "maintenance" || moved.Fields["hostname"] != "db1" || moved.Fields["serial_number"] != nil || moved.Fields["virtual"] != false {
		t.Errorf("patched: %+v", moved)
	}
	moved = decodeBody[ciBody](t, keeper.do(h, "PATCH", "/api/v1/cis/"+srv.ID, `{"class":"network_device"}`))
	if moved.Class != "network_device" || moved.Fields["hostname"] != "db1" || moved.Fields["virtual"] != nil {
		t.Errorf("moved to network device: %+v", moved)
	}

	expect(t, agent.do(h, "DELETE", "/api/v1/cis/"+srv.ID, ""), 403, "an agent removing one")
	expect(t, keeper.do(h, "DELETE", "/api/v1/cis/"+srv.ID, ""), 204, "removing one")
	expect(t, agent.do(h, "GET", "/api/v1/cis/"+srv.ID, ""), 404, "after it is removed")

	refusal(t, admin.do(h, "DELETE", "/api/v1/groups/"+group.ID, ""), 409, "configuration items are assigned to this group", "a support group in use")
}

// Impact follows the relationships that carry it backwards, as far as they go, and stops at loops; relationships that
// do not carry impact are shown but not followed.
func TestImpactFollowsDependenciesBackwards(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	add := func(class, name string) string {
		return decodeBody[ciBody](t, admin.do(h, "POST", "/api/v1/cis", `{"class":"`+class+`","name":"`+name+`"}`)).ID
	}
	relate := func(from, typ, to string) {
		expect(t, admin.do(h, "POST", "/api/v1/cis/"+from+"/relationships", `{"type":"`+typ+`","to":"`+to+`"}`), 201, typ)
	}
	payroll := add("business_service", "Payroll")
	app := add("application", "Payroll app")
	db := add("database", "pg-01")
	host := add("server", "host-1")
	rack := add("hardware", "Rack 4")
	sw := add("network_device", "core-switch")
	relate(payroll, "depends_on", app)
	relate(app, "depends_on", db)
	relate(db, "runs_on", host)
	relate(host, "part_of", rack) // grouping: trouble with the rack is not followed
	relate(host, "depends_on", sw)
	relate(sw, "depends_on", payroll) // a loop back to the top

	// Added from the other end: "pg-01 is used by the reporting app".
	reporting := add("application", "Reporting")
	w := admin.do(h, "POST", "/api/v1/cis/"+db+"/relationships", `{"type":"depends_on","from":"`+reporting+`"}`)
	expect(t, w, 201, "relating from the other end")
	refusal(t, admin.do(h, "POST", "/api/v1/cis/"+db+"/relationships", `{"type":"depends_on","from":"`+reporting+`"}`), 409, "already related", "the same twice")
	refusal(t, admin.do(h, "POST", "/api/v1/cis/"+db+"/relationships", `{"type":"likes","to":"`+host+`"}`), 422, "no relationship type", "an unknown type")
	refusal(t, admin.do(h, "POST", "/api/v1/cis/"+db+"/relationships", `{"type":"runs_on","to":"`+db+`"}`), 422, "itself", "itself")
	expect(t, agent.do(h, "POST", "/api/v1/cis/"+db+"/relationships", `{"type":"runs_on","to":"`+rack+`"}`), 403, "an agent relating items")

	got := decodeBody[ciBody](t, agent.do(h, "GET", "/api/v1/cis/"+db, ""))
	var read []string
	for _, r := range got.Relationships {
		read = append(read, r.Label+" "+r.Item.Name)
	}
	if strings.Join(read, "; ") != "Used by Payroll app; Used by Reporting; Runs on host-1" {
		t.Errorf("pg-01's relationships read from it: %v", read)
	}

	impact := decodeBody[impactBody](t, agent.do(h, "GET", "/api/v1/cis/"+sw+"/impact", ""))
	var reached []string
	for _, a := range impact.Items {
		reached = append(reached, fmt.Sprintf("%s@%d<%s", a.Name, a.Depth, a.ViaName))
	}
	want := "host-1@1<core-switch,pg-01@2<host-1,Payroll app@3<pg-01,Reporting@3<pg-01,Payroll@4<Payroll app"
	if strings.Join(reached, ",") != want || impact.Truncated {
		t.Errorf("what a switch outage reaches:\n got %s\nwant %s", strings.Join(reached, ","), want)
	}
	if rackImpact := decodeBody[impactBody](t, agent.do(h, "GET", "/api/v1/cis/"+rack+"/impact", "")); len(rackImpact.Items) != 0 {
		t.Errorf("part of carries no impact: %+v", rackImpact.Items)
	}

	// Removing a relationship stops impact along it.
	for _, r := range decodeBody[ciBody](t, admin.do(h, "GET", "/api/v1/cis/"+host, "")).Relationships {
		if r.Type == "depends_on" {
			expect(t, admin.do(h, "DELETE", "/api/v1/cis/"+host+"/relationships/"+r.ID, ""), 204, "removing host-1 depends on core-switch")
		}
	}
	if left := decodeBody[impactBody](t, agent.do(h, "GET", "/api/v1/cis/"+sw+"/impact", "")); len(left.Items) != 0 {
		t.Errorf("after removing it: %+v", left.Items)
	}
}

// Agents link items to the records they work on, and see what those items' trouble reaches; the requester's view of
// their own record carries none of it, and an item linked to a record cannot be removed.
func TestRecordsLinkToTheItemsTheyAffect(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	add := func(class, name string) string {
		return decodeBody[ciBody](t, admin.do(h, "POST", "/api/v1/cis", `{"class":"`+class+`","name":"`+name+`"}`)).ID
	}
	email := add("business_service", "Email")
	mx := add("server", "mx-1")
	expect(t, admin.do(h, "POST", "/api/v1/cis/"+email+"/relationships", `{"type":"depends_on","to":"`+mx+`"}`), 201, "email depends on mx-1")

	ticket := decodeBody[struct{ ID, Reference string }](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Mail is not arriving"}`))
	linked := decodeBody[struct{ Items []ciBody }](t, agent.do(h, "PUT", "/api/v1/tickets/"+ticket.ID+"/cis/"+mx, ""))
	if len(linked.Items) != 1 || linked.Items[0].Name != "mx-1" || linked.Items[0].ClassLabel != "Server" {
		t.Fatalf("linked: %+v", linked)
	}
	expect(t, agent.do(h, "PUT", "/api/v1/tickets/"+ticket.Reference+"/cis/"+mx, ""), 200, "linking it again, by reference")
	expect(t, lena.do(h, "PUT", "/api/v1/tickets/"+ticket.ID+"/cis/"+email, ""), 403, "the requester linking one")
	refusal(t, agent.do(h, "PUT", "/api/v1/tickets/"+ticket.ID+"/cis/"+ticket.ID, ""), 422, "no item with that id", "a missing item")

	var seen struct {
		CIs []ciBody `json:"cis"`
	}
	_ = json.Unmarshal(agent.do(h, "GET", "/api/v1/tickets/"+ticket.ID, "").Body.Bytes(), &seen)
	if len(seen.CIs) != 1 {
		t.Errorf("the agent sees the linked item on the ticket: %+v", seen)
	}
	raw := lena.do(h, "GET", "/api/v1/tickets/"+ticket.ID, "").Body.String()
	if strings.Contains(raw, `"cis"`) || strings.Contains(raw, "mx-1") {
		t.Errorf("the requester's view carries no items: %s", raw)
	}
	expect(t, lena.do(h, "GET", "/api/v1/tickets/"+ticket.ID+"/impact", ""), 403, "the requester asking for impact")

	impact := decodeBody[impactBody](t, agent.do(h, "GET", "/api/v1/tickets/"+ticket.ID+"/impact", ""))
	if len(impact.Items) != 1 || impact.Items[0].Name != "Email" || impact.Items[0].ViaName != "mx-1" {
		t.Errorf("the ticket's impact: %+v", impact)
	}
	item := decodeBody[ciBody](t, agent.do(h, "GET", "/api/v1/cis/"+mx, ""))
	if len(item.Tickets) != 1 || item.Tickets[0].Reference != ticket.Reference {
		t.Errorf("the item lists the ticket: %+v", item.Tickets)
	}

	refusal(t, admin.do(h, "DELETE", "/api/v1/cis/"+mx, ""), 409, "retire it instead", "removing a linked item")
	expect(t, agent.do(h, "DELETE", "/api/v1/tickets/"+ticket.ID+"/cis/"+mx, ""), 200, "unlinking it")
	expect(t, agent.do(h, "DELETE", "/api/v1/tickets/"+ticket.ID+"/cis/"+mx, ""), 404, "unlinking it again")
	expect(t, admin.do(h, "DELETE", "/api/v1/cis/"+mx, ""), 204, "removing it once nothing links to it")
}
