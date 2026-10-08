package api_test

import (
	"strings"
	"testing"
)

type importRow struct {
	Row     int
	Name    string
	Class   string
	Action  string
	ID      string
	Changes []string
	Problem string
}

type importReport struct {
	Applied bool
	Counts  map[string]int
	Rows    []importRow
}

func rowNamed(rep importReport, name string) importRow {
	for _, r := range rep.Rows {
		if r.Name == name {
			return r
		}
	}
	return importRow{}
}

// An items file is previewed first: each row says whether it adds, changes or leaves an item, or what is wrong with
// it, and nothing is written. Applying refuses a file with problems unless told to skip them. Cells take labels as
// well as values, blank cells leave values alone, and an export uploads again unchanged.
func TestCMDBImportsItemsFromCSV(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	const path = "/api/v1/cmdb/csv/items/server"

	tpl := agent.do(h, "GET", path+"?template=true", "")
	expect(t, tpl, 200, "an agent downloading the template")
	if body := strings.TrimPrefix(tpl.Body.String(), "\uFEFF"); !strings.HasPrefix(body, "name,status,description,owner_group,support_group,manufacturer,") ||
		!strings.Contains(body, ",hostname,") || strings.Count(body, "\n") != 1 || !strings.Contains(tpl.Header().Get("Content-Disposition"), "server-template.csv") {
		t.Errorf("template: %q %v", body, tpl.Header())
	}
	expect(t, lena.do(h, "GET", path+"?template=true", ""), 403, "an end user")

	team := decodeBody[struct{ ID string }](t, admin.do(h, "POST", "/api/v1/groups", `{"name":"Web team"}`))
	old := decodeBody[struct{ ID string }](t, admin.do(h, "POST", "/api/v1/cis", `{"class":"server","name":"web-02","fields":{"environment":"test"}}`))

	file := "Name;Status;Owner group;hostname;Environment;virtual;warranty_ends\n" + // semicolons, as some spreadsheets save
		"web-01;active;web team;web-01.example.com;Production;yes;2027-03-01\n" +
		"WEB-02;;;;staging;no;\n" +
		"\n" +
		"web-01;;;;;;\n" +
		"web-05;bogus;;;;;\n" +
		"web-06;;No such team;;;;\n" +
		"web-07;;;;;;03/01/2027\n" +
		"web-08;;;;Moon;;\n"
	preview := decodeBody[importReport](t, admin.do(h, "POST", path, file))
	if preview.Applied || preview.Counts["create"] != 1 || preview.Counts["update"] != 1 || preview.Counts["error"] != 5 {
		t.Fatalf("preview: %+v", preview)
	}
	if r := rowNamed(preview, "WEB-02"); r.Action != "update" || r.ID != old.ID || strings.Join(r.Changes, ",") != "name,environment,virtual" {
		t.Errorf("an update by name, case aside: %+v", r)
	}
	for line, want := range map[int]string{5: "row 2 has the same name", 6: "status must be", 7: "no group called No such team",
		8: "Warranty ends takes a date as YYYY-MM-DD", 9: "Environment must be one of Production, Staging"} {
		if got := preview.Rows[line-3]; got.Row != line || !strings.Contains(got.Problem, want) {
			t.Errorf("line %d: %+v, want %q", line, got, want)
		}
	}
	if items := decodeBody[struct{ Items []any }](t, admin.do(h, "GET", "/api/v1/cis?class=server", "")).Items; len(items) != 1 {
		t.Fatalf("a preview writes nothing: %d servers", len(items))
	}
	refused := admin.do(h, "POST", path+"?apply=true", file)
	expect(t, refused, 422, "applying a file with problems")
	if items := decodeBody[struct{ Items []any }](t, admin.do(h, "GET", "/api/v1/cis?class=server", "")).Items; len(items) != 1 {
		t.Fatalf("a refused import writes nothing: %d servers", len(items))
	}
	expect(t, agent.do(h, "POST", path+"?apply=true", file), 403, "an agent importing")

	done := decodeBody[importReport](t, admin.do(h, "POST", path+"?apply=true&skip_errors=true", file))
	created := rowNamed(done, "web-01")
	if !done.Applied || created.ID == "" {
		t.Fatalf("applied, skipping the problems: %+v", done)
	}
	type ci struct {
		Name, Status   string
		OwnerGroupName *string `json:"owner_group_name"`
		Fields         map[string]any
	}
	got := decodeBody[ci](t, admin.do(h, "GET", "/api/v1/cis/"+created.ID, ""))
	if got.OwnerGroupName == nil || *got.OwnerGroupName != "Web team" || got.Fields["environment"] != "production" || got.Fields["virtual"] != true ||
		got.Fields["warranty_ends"] != "2027-03-01" || got.Fields["hostname"] != "web-01.example.com" {
		t.Errorf("created: %+v", got)
	}
	if got := decodeBody[ci](t, admin.do(h, "GET", "/api/v1/cis/"+old.ID, "")); got.Name != "WEB-02" || got.Fields["environment"] != "staging" {
		t.Errorf("updated: %+v", got)
	}
	_ = team

	// What comes out goes back in unchanged, values written as the import reads them.
	export := admin.do(h, "GET", path, "")
	expect(t, export, 200, "exporting")
	if !strings.Contains(export.Body.String(), "web-01,active,,Web team,,") || !strings.Contains(export.Body.String(), ",production,yes") {
		t.Errorf("export: %s", export.Body)
	}
	again := decodeBody[importReport](t, admin.do(h, "POST", path+"?apply=true", export.Body.String()))
	if again.Counts["unchanged"] != 2 || again.Counts["create"]+again.Counts["update"]+again.Counts["error"] != 0 {
		t.Errorf("an export imports unchanged: %+v", again)
	}

	if w := admin.do(h, "POST", path, "name,colour\nx,red\n"); w.Code != 422 || !strings.Contains(w.Body.String(), `no column \"colour\"`) {
		t.Errorf("an unknown column: %d %s", w.Code, w.Body)
	}
	expect(t, admin.do(h, "POST", "/api/v1/cmdb/csv/items/spaceship", "name\nx\n"), 404, "an unknown class")
}

// A relationships file relates items already there, by class and name; one already there is left alone.
func TestCMDBImportsRelationshipsFromCSV(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	for _, body := range []string{`{"class":"server","name":"web-01"}`, `{"class":"database","name":"pg-01"}`, `{"class":"application","name":"Payroll"}`} {
		expect(t, admin.do(h, "POST", "/api/v1/cis", body), 201, "an item")
	}
	const path = "/api/v1/cmdb/csv/relationships"
	file := "from_class,from_name,type,to_class,to_name\n" +
		"Application,payroll,depends on,database,pg-01\n" +
		"application,Payroll,runs_on,Server,web-01\n" +
		"application,Payroll,depends_on,database,PG-01\n" +
		"server,web-01,runs_on,server,web-01\n" +
		"server,nope,runs_on,server,web-01\n" +
		"server,web-01,likes,database,pg-01\n"
	preview := decodeBody[importReport](t, admin.do(h, "POST", path, file))
	if preview.Counts["create"] != 2 || preview.Counts["unchanged"] != 1 || preview.Counts["error"] != 3 {
		t.Fatalf("preview: %+v", preview)
	}
	for i, want := range []string{"related to itself", "no server called nope", `no relationship type "likes"`} {
		if !strings.Contains(preview.Rows[3+i].Problem, want) {
			t.Errorf("row %d: %+v, want %q", preview.Rows[3+i].Row, preview.Rows[3+i], want)
		}
	}
	done := decodeBody[importReport](t, admin.do(h, "POST", path+"?apply=true&skip_errors=true", file))
	if !done.Applied {
		t.Fatalf("applied: %+v", done)
	}
	export := admin.do(h, "GET", path, "")
	if body := export.Body.String(); !strings.Contains(body, "application,Payroll,depends_on,database,pg-01") || !strings.Contains(body, "application,Payroll,runs_on,server,web-01") {
		t.Errorf("export: %s", body)
	}
	again := decodeBody[importReport](t, admin.do(h, "POST", path+"?apply=true", export.Body.String()))
	if again.Counts["unchanged"] != 2 || again.Counts["create"] != 0 {
		t.Errorf("an export imports unchanged: %+v", again)
	}
}

// A class that others extend has one file for the whole family: a class column says which class a row is, the
// columns are every field of the family, and an export includes the items of every class in it. A new name that
// another class of the family already has is refused rather than made a second item.
func TestCMDBImportsAClassFamilyInOneFile(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	const path = "/api/v1/cmdb/csv/items/hardware"
	expect(t, admin.do(h, "POST", "/api/v1/cis", `{"class":"server","name":"web-01"}`), 201, "a server")

	tpl := strings.TrimPrefix(admin.do(h, "GET", path+"?template=true", "").Body.String(), "\uFEFF")
	if !strings.HasPrefix(tpl, "name,class,status,") || !strings.Contains(tpl, ",hostname,") {
		t.Errorf("the family's template: %q", tpl)
	}
	if leaf := admin.do(h, "GET", "/api/v1/cmdb/csv/items/server?template=true", "").Body.String(); strings.Contains(leaf, ",class,") {
		t.Errorf("a class nothing extends has no class column: %q", leaf)
	}

	file := "name,class,hostname,serial_number\n" +
		"printer-01,,,SN-1\n" + // the file's class
		"WEB-01,,,\n" + // web-01 is a server: not a second item
		"web-01,Server,web-01.example.com,SN-2\n" +
		"printer-02,,printer-02.example.com,\n" + // hardware has no hostname
		"pad-01,Spaceship,,\n"
	rep := decodeBody[importReport](t, admin.do(h, "POST", path, file))
	want := []struct{ action, class, problem string }{
		{"create", "hardware", ""},
		{"error", "hardware", "there is already a server called web-01: set class to Server to update it"},
		{"update", "server", ""},
		{"error", "hardware", "Hardware items have no Hostname"},
		{"error", "", "class must be Hardware, "},
	}
	for i, w := range want {
		got := rep.Rows[i]
		if got.Action != w.action || (w.class != "" && got.Class != w.class) || !strings.Contains(got.Problem, w.problem) {
			t.Errorf("row %d: %+v, want %+v", got.Row, got, w)
		}
	}
	applied := admin.do(h, "POST", path+"?apply=true&skip_errors=true", file)
	expect(t, applied, 200, "importing")

	export := strings.TrimPrefix(admin.do(h, "GET", path, "").Body.String(), "\uFEFF")
	if !strings.Contains(export, "printer-01,hardware,") || !strings.Contains(export, "web-01,server,") || !strings.Contains(export, "web-01.example.com") {
		t.Errorf("the export has the whole family: %s", export)
	}
	again := decodeBody[importReport](t, admin.do(h, "POST", path, export))
	if again.Counts["unchanged"] != 2 || len(again.Rows) != 2 {
		t.Errorf("an export imports unchanged: %+v", again)
	}
}
