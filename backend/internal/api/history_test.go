package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
)

type historyBody struct {
	Items []struct {
		ActorName      *string `json:"actor_name"`
		ActorApp       *string `json:"actor_app"`
		OnBehalfOfName *string `json:"on_behalf_of_name"`
		Action         string  `json:"action"`
		Field          string  `json:"field"`
		Label          string  `json:"label"`
		OldText        string  `json:"old_text"`
		NewText        string  `json:"new_text"`
		Kind           string  `json:"kind"`
	} `json:"items"`
}

func (b historyBody) lines() []string {
	var out []string
	for _, e := range b.Items {
		who := "system"
		if e.ActorName != nil {
			who = *e.ActorName
		}
		if e.OnBehalfOfName != nil {
			who += " for " + *e.OnBehalfOfName
		}
		out = append(out, fmt.Sprintf("%s %s %s: %s -> %s", who, e.Action, e.Label, e.OldText, e.NewText))
	}
	return out
}

func has(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// Everything that happens to an incident is in its history, field by field, whoever made it: a person, an app acting
// for someone. Its requester reads it; nobody can change it.
func TestAnIncidentsHistorySaysWhoChangedWhat(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	omar := newActor(t, h, pool, "omar@x.test", "end_user")
	group := decodeBody[struct{ ID string }](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Service desk"}`))
	ci := decodeBody[ciBody](t, admin.do(h, "POST", "/api/v1/cis", `{"class":"server","name":"print-01"}`))

	expect(t, admin.do(h, "PUT", "/api/v1/groups/"+group.ID+"/members/"+agent.id, ""), 204, "the agent joins the desk")
	tk := decodeBody[typedTicket](t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Printer on fire"}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"priority":1,"assignee_id":"`+agent.id+`","assignment_group_id":"`+group.ID+`","fields":{"category":"hardware"}}`), 200, "triage")
	expect(t, agent.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/transitions", `{"to":"in_progress"}`), 200, "starting work")
	expect(t, agent.do(h, "PUT", "/api/v1/tickets/"+tk.ID+"/cis/"+ci.ID, ""), 200, "linking print-01")
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"resolution":"Put it out."}`), 200, "resolution notes")

	// An app acting for someone (a workflow step, an async rule) is named as the app, for that person.
	ctx := context.Background()
	var principal uuid.UUID
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT app_principal('incident-management')").Scan(&principal)
	}); err != nil {
		t.Fatal(err)
	}
	// As db.WithPrincipal sets it up (that app may only read incidents, so the change is made past RLS here).
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_user_id', $1, true), set_config('app.on_behalf_of', $2, true)",
			principal.String(), agent.id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE tasks SET priority = 2 WHERE id = $1", tk.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	got := decodeBody[historyBody](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/history", ""))
	lines := got.lines()
	for _, want := range []string{
		"lena@x.test created :  -> Printer on fire",
		"agent@x.test changed Priority: Medium -> Critical",
		"agent@x.test changed Assignee:  -> agent@x.test",
		"agent@x.test changed Assignment group:  -> Service desk",
		"agent@x.test changed Category:  -> Hardware",
		"agent@x.test changed Status: New -> In progress",
		"agent@x.test linked Affected item:  -> print-01",
		"agent@x.test changed Resolution notes:  -> Put it out.",
		"incident-management for agent@x.test changed Priority: Critical -> High",
	} {
		if !has(lines, want) {
			t.Errorf("no %q in\n%s", want, strings.Join(lines, "\n"))
		}
	}
	for _, e := range got.Items {
		if e.Field == "resolution" && e.Kind != "long" {
			t.Errorf("resolution notes are long text: %+v", e)
		}
		if e.Label == "Priority" && strings.Contains(e.OldText+e.NewText, "incident-management") {
			t.Errorf("%+v", e)
		}
	}
	if last := got.Items[len(got.Items)-1]; last.ActorApp == nil || *last.ActorApp != "incident-management" {
		t.Errorf("the app is named as an app: %+v", last)
	}

	// The requester reads her own record's history, without the item she may not see; nobody else's.
	hers := decodeBody[historyBody](t, lena.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/history", "")).lines()
	if !has(hers, "agent@x.test changed Status: New -> In progress") || !has(hers, "agent@x.test linked Affected item:  -> an item") {
		t.Errorf("the requester's view:\n%s", strings.Join(hers, "\n"))
	}
	expect(t, omar.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/history", ""), 404, "another end user")

	// Nobody can write or rewrite it, admins included.
	for _, stmt := range []string{"DELETE FROM record_history", "UPDATE record_history SET field = 'x'",
		"INSERT INTO record_history (subject, subject_id, action) VALUES ('task', gen_random_uuid(), 'created')"} {
		err := db.WithUser(ctx, pool, uuid.MustParse(admin.id), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %v", stmt, err)
		}
	}
}

// A configuration item's history: its fields, its relationships (read from it) and the records linked to it.
func TestAnItemsHistory(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	db1 := decodeBody[ciBody](t, admin.do(h, "POST", "/api/v1/cis", `{"class":"database","name":"pg-01","fields":{"engine":"postgresql"}}`))
	app := decodeBody[ciBody](t, admin.do(h, "POST", "/api/v1/cis", `{"class":"application","name":"Payroll"}`))
	expect(t, admin.do(h, "PATCH", "/api/v1/cis/"+db1.ID, `{"status":"maintenance","fields":{"engine":"mysql"}}`), 200, "changing it")
	expect(t, admin.do(h, "POST", "/api/v1/cis/"+app.ID+"/relationships", `{"type":"depends_on","to":"`+db1.ID+`"}`), 201, "relating")
	tk := decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Slow queries"}`))
	expect(t, agent.do(h, "PUT", "/api/v1/tickets/"+tk.ID+"/cis/"+db1.ID, ""), 200, "linking")

	lines := decodeBody[historyBody](t, agent.do(h, "GET", "/api/v1/cis/"+db1.ID+"/history", "")).lines()
	for _, want := range []string{
		"boss@x.test created :  -> pg-01",
		"boss@x.test changed Status: Active -> In maintenance",
		"boss@x.test changed Engine: PostgreSQL -> MySQL",
		"boss@x.test linked Relationship:  -> Used by Payroll",
		"agent@x.test linked Ticket:  -> " + tk.Reference + " Slow queries",
	} {
		if !has(lines, want) {
			t.Errorf("no %q in\n%s", want, strings.Join(lines, "\n"))
		}
	}
	if theirs := decodeBody[historyBody](t, agent.do(h, "GET", "/api/v1/cis/"+app.ID+"/history", "")).lines(); !has(theirs, "boss@x.test linked Relationship:  -> Depends on pg-01") {
		t.Errorf("read from the other end:\n%s", strings.Join(theirs, "\n"))
	}
	expect(t, lena.do(h, "GET", "/api/v1/cis/"+db1.ID+"/history", ""), 403, "an end user")
}

// What is recorded field by field is chosen per record type and for configuration items; the basics are recorded
// whatever is chosen; an app (or an operator's local override) leaves a field out with history: false; and every
// change to the choice is itself recorded.
func TestWhatHistoryRecordsIsChosen(t *testing.T) {
	h, pool, reg := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	expect(t, agent.do(h, "GET", "/api/v1/settings/history", ""), 403, "an agent reading the history settings")
	type settingsBody struct {
		RecordTypes []string `json:"record_types"`
		CMDB        bool     `json:"cmdb"`
		Exclusions  []struct{ Kind, Field, Label string }
		Log         []struct {
			Setting   string  `json:"setting"`
			ActorName *string `json:"actor_name"`
		}
	}
	def := decodeBody[settingsBody](t, admin.do(h, "GET", "/api/v1/settings/history", ""))
	if strings.Join(def.RecordTypes, ",") != "*" || !def.CMDB {
		t.Errorf("everything by default: %+v", def)
	}
	refusal(t, admin.do(h, "PUT", "/api/v1/settings/history", `{"record_types":["incidents"],"cmdb":true}`), 422, "is not a record type", "a typo")
	got := decodeBody[settingsBody](t, admin.do(h, "PUT", "/api/v1/settings/history", `{"record_types":["change"],"cmdb":false}`))
	if strings.Join(got.RecordTypes, ",") != "change" || got.CMDB || len(got.Log) == 0 || *got.Log[0].ActorName != "boss@x.test" {
		t.Fatalf("saved, and the change logged: %+v", got)
	}

	// Incidents: only the basics now.
	tk := decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Quiet one"}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+tk.ID, `{"priority":1,"title":"Renamed","assignee_id":"`+agent.id+`"}`), 200, "editing it")
	expect(t, agent.do(h, "POST", "/api/v1/tickets/"+tk.ID+"/transitions", `{"to":"in_progress"}`), 200, "starting work")
	lines := strings.Join(decodeBody[historyBody](t, agent.do(h, "GET", "/api/v1/tickets/"+tk.ID+"/history", "")).lines(), "\n")
	for _, want := range []string{"created", "Status: New -> In progress", "Assignee:  -> agent@x.test"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the basics are always recorded: no %q in\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "Priority") || strings.Contains(lines, "Summary") {
		t.Errorf("an incident's other fields are not recorded:\n%s", lines)
	}
	// Changes: everything.
	ch := decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Patch"}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+ch.ID, `{"priority":2}`), 200, "editing the change")
	if l := strings.Join(decodeBody[historyBody](t, agent.do(h, "GET", "/api/v1/tickets/"+ch.ID+"/history", "")).lines(), "\n"); !strings.Contains(l, "Priority") {
		t.Errorf("a change's fields are recorded:\n%s", l)
	}
	// Items: the basics only.
	ci := decodeBody[ciBody](t, admin.do(h, "POST", "/api/v1/cis", `{"class":"server","name":"quiet-01"}`))
	expect(t, admin.do(h, "PATCH", "/api/v1/cis/"+ci.ID, `{"name":"quiet-02","status":"retired"}`), 200, "changing the item")
	if l := strings.Join(decodeBody[historyBody](t, agent.do(h, "GET", "/api/v1/cis/"+ci.ID+"/history", "")).lines(), "\n"); strings.Contains(l, "Name") || !strings.Contains(l, "Status: Active -> Retired") {
		t.Errorf("an item's basics only:\n%s", l)
	}

	// A field left out with history: false, here by an operator's local override of the shipped form.
	expect(t, admin.do(h, "PUT", "/api/v1/settings/history", `{"record_types":["*"],"cmdb":true}`), 200, "everything again")
	if _, _, err := apps.UpdateStudio(context.Background(), pool, apps.StudioUpdate{App: "incident-management",
		Files: map[string][]byte{"forms/incident.yaml": []byte("fields:\n  urgency:\n    history: false\n")}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	ex := decodeBody[settingsBody](t, admin.do(h, "GET", "/api/v1/settings/history", "")).Exclusions
	if len(ex) != 1 || ex[0].Kind != "incident" || ex[0].Field != "fields.urgency" || ex[0].Label != "Urgency" {
		t.Errorf("the exclusion is listed: %+v", ex)
	}
	second := decodeBody[typedTicket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Partly quiet"}`))
	expect(t, agent.do(h, "PATCH", "/api/v1/tickets/"+second.ID, `{"fields":{"urgency":"3","impact":"3"}}`), 200, "impact and urgency")
	if l := strings.Join(decodeBody[historyBody](t, agent.do(h, "GET", "/api/v1/tickets/"+second.ID+"/history", "")).lines(), "\n"); strings.Contains(l, "Urgency") || !strings.Contains(l, "Impact") {
		t.Errorf("urgency left out, impact kept:\n%s", l)
	}
}
