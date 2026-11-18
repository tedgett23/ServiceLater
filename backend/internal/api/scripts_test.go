package api_test

import (
	"encoding/json"
	"strings"
	"testing"
)

type scriptTestBody struct {
	OK      bool `json:"ok"`
	Problem *struct {
		Line    int    `json:"line"`
		Message string `json:"message"`
	} `json:"problem"`
	Aborted string `json:"aborted"`
	Changes []struct {
		Reference string         `json:"reference"`
		Fields    map[string]any `json:"fields"`
		Status    string         `json:"status"`
		Comment   string         `json:"comment"`
	} `json:"changes"`
	Vars    map[string]any `json:"vars"`
	Printed []string       `json:"printed"`
}

func scriptBody(src, record, where string) string {
	b, _ := json.Marshal(map[string]any{"script": src, "record": record, "where": where})
	return string(b)
}

// Scripts are checked as they are typed and tried against a record, by whoever may change them; a test run says
// what the script would change and changes nothing.
func TestScriptsAreCheckedAndTried(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	expect(t, agent.do(h, "POST", "/api/v1/scripts/test", scriptBody("pass", "", "")), 403, "an agent trying a script")

	check := decodeBody[struct {
		Problems []struct {
			Line    int    `json:"line"`
			Message string `json:"message"`
		} `json:"problems"`
	}](t, admin.do(h, "POST", "/api/v1/scripts/check", `{"script":"x = 1\nupate(record, {})"}`))
	if len(check.Problems) != 1 || check.Problems[0].Line != 2 || !strings.Contains(check.Problems[0].Message, "upate") {
		t.Errorf("check: %+v", check)
	}

	tk := decodeBody[problemBody](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Printer on fire","priority":3}`))
	src := "print(record.reference)\nupdate(record, {\"priority\": 1}, comment=\"Raised\")\nreturn {\"vars\": {\"seen\": record.title}}"
	res := decodeBody[scriptTestBody](t, admin.do(h, "POST", "/api/v1/scripts/test", scriptBody(src, tk.Reference, "workflow")))
	if !res.OK || len(res.Changes) != 1 || res.Changes[0].Reference != tk.Reference || res.Changes[0].Fields["priority"] != float64(1) ||
		res.Changes[0].Comment != "Raised" || res.Vars["seen"] != "Printer on fire" || res.Printed[0] != tk.Reference {
		t.Errorf("test run: %+v", res)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM tasks WHERE id = $1 AND priority = 3", tk.ID); n != 1 {
		t.Error("a test run changed the record")
	}
	res = decodeBody[scriptTestBody](t, admin.do(h, "POST", "/api/v1/scripts/test", scriptBody(`comment(record, "x")`, tk.Reference, "before")))
	if res.OK || res.Problem == nil || !strings.Contains(res.Problem.Message, "not available here") {
		t.Errorf("a before rule's script commenting: %+v", res)
	}
	res = decodeBody[scriptTestBody](t, admin.do(h, "POST", "/api/v1/scripts/test", scriptBody(`abort("Not today")`, tk.Reference, "before")))
	if res.OK || res.Aborted != "Not today" {
		t.Errorf("abort: %+v", res)
	}
}

// Workflows are not scripts: someone who may edit workflows but not scripts can save a workflow, but not a new or
// changed script in it.
func TestChangingAScriptNeedsScriptsEdit(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	wes := newActor(t, h, pool, "wes@x.test", "agent")
	customRole(t, h, pool, admin, "flow-keeper", wes, "workflows:edit")
	const path = "/api/v1/workflows/core-itsm/raise"
	withScript := func(src string) string {
		b, _ := json.Marshal(map[string]any{"record_type": "incident", "start": "s", "steps": []any{
			map[string]any{"id": "s", "type": "script", "script": src, "next": "done"},
			map[string]any{"id": "done", "type": "end"}}})
		return string(b)
	}
	refusal(t, wes.do(h, "PUT", path, withScript(`update(record, {"priority": 1})`)), 403, "scripts:edit", "adding a script without scripts:edit")
	expect(t, admin.do(h, "PUT", path, withScript(`update(record, {"priority": 1})`)), 200, "an admin adding it")
	// The same script, with only the layout moved, saves; a changed script does not.
	b, _ := json.Marshal(map[string]any{"record_type": "incident", "start": "s", "layout": map[string]any{"s": map[string]int{"x": 10, "y": 20}},
		"steps": []any{map[string]any{"id": "s", "type": "script", "script": `update(record, {"priority": 1})`, "next": "done"}, map[string]any{"id": "done", "type": "end"}}})
	expect(t, wes.do(h, "PUT", path, string(b)), 200, "moving a script step")
	refusal(t, wes.do(h, "PUT", path, withScript(`update(record, {"priority": 2})`)), 403, "scripts:edit", "changing the script")
	refusal(t, admin.do(h, "PUT", path, withScript(`update(record, {"priority": 2}`)), 422, "script", "a script with a mistake")
}

type scriptFileBody struct {
	Name    string   `json:"name"`
	Source  string   `json:"source"`
	Edited  bool     `json:"edited"`
	Shipped bool     `json:"shipped"`
	UsedBy  []string `json:"used_by"`
}

// An app's script files are added and changed here like its workflows, and checked with everything that loads them:
// a change that breaks a workflow is refused, as is removing a file a workflow still loads.
func TestScriptFilesAreEditedAndCheckedWithWhatLoadsThem(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	const file = "/api/v1/scripts/core-itsm/helpers.star"
	refusal(t, agent.do(h, "PUT", file, `{"source":"def f():\n    pass\n"}`), 403, "", "an agent adding a script file")
	w := admin.do(h, "PUT", file, `{"source":"def raise_it(record):\n    update(record, {\"priority\": 1})\n"}`)
	expect(t, w, 200, "adding a script file")
	saved := decodeBody[scriptFileBody](t, w)
	if !saved.Edited || saved.Shipped || len(saved.UsedBy) != 0 {
		t.Fatalf("saved: %+v", saved)
	}
	refusal(t, admin.do(h, "PUT", "/api/v1/scripts/core-itsm/Bad.star", `{"source":"x = 1"}`), 422, "not a script file name", "a bad file name")

	flow, _ := json.Marshal(map[string]any{"record_type": "incident", "start": "s", "steps": []any{
		map[string]any{"id": "s", "type": "script", "script": "load(\"helpers.star\", \"raise_it\")\nraise_it(record)", "next": "done"},
		map[string]any{"id": "done", "type": "end"}}})
	expect(t, admin.do(h, "PUT", "/api/v1/workflows/core-itsm/raise", string(flow)), 200, "a workflow loading it")
	got := decodeBody[scriptFileBody](t, agent.do(h, "GET", file, ""))
	if strings.Join(got.UsedBy, ",") != "workflow:raise" || !strings.Contains(got.Source, "raise_it") {
		t.Errorf("the file: %+v", got)
	}
	list := decodeBody[struct{ Items []scriptFileBody }](t, agent.do(h, "GET", "/api/v1/scripts", ""))
	if len(list.Items) != 1 || list.Items[0].Source != "" {
		t.Errorf("list: %+v", list.Items)
	}

	refusal(t, admin.do(h, "PUT", file, `{"source":"def raise_other(record):\n    pass\n"}`), 422, "helpers.star defines no raise_it", "breaking the workflow that loads it")
	refusal(t, admin.do(h, "DELETE", file+"/edits", ""), 422, "this app has no script file helpers.star", "removing a file a workflow loads")
	expect(t, admin.do(h, "DELETE", "/api/v1/workflows/core-itsm/raise/edits", ""), 204, "removing the workflow")
	expect(t, admin.do(h, "DELETE", file+"/edits", ""), 204, "then the file")
	expect(t, admin.do(h, "GET", file, ""), 404, "the file is gone")
}
