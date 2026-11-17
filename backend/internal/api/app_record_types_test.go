package api_test

import (
	"context"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

// tylerDir is the sample app that defines its own record type (Things, TYL1). It is for tests only.
func tylerDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "apps", "testdata", "tyler")
}

func tylerBundle(t *testing.T) *apps.Bundle {
	t.Helper()
	b, err := apps.Load(tylerDir())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// installBundle compiles and installs an app (approving its permissions) and reloads the registry.
func installBundle(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry, b *apps.Bundle) error {
	t.Helper()
	c, err := b.Compile()
	if err != nil {
		return err
	}
	if _, err := apps.Install(context.Background(), pool, c, apps.InstallOptions{ApprovePermissions: true}); err != nil {
		return err
	}
	return reg.Load(context.Background())
}

func installTyler(t *testing.T, pool *pgxpool.Pool, reg *apps.Registry) {
	t.Helper()
	if err := installBundle(t, pool, reg, tylerBundle(t)); err != nil {
		t.Fatal(err)
	}
}

type thingDTO struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Number    int64          `json:"number"`
	Prefix    string         `json:"prefix"`
	Reference string         `json:"reference"`
	Status    string         `json:"status"`
	Priority  int            `json:"priority"`
	Fields    map[string]any `json:"fields"`
	Moves     []move         `json:"moves"`
	SLAs      []struct {
		Label string `json:"label"`
	} `json:"slas"`
}

type recordTypeRow struct {
	Type    string `json:"type"`
	Prefix  string `json:"prefix"`
	Label   string `json:"label"`
	Plural  string `json:"plural"`
	App     string `json:"app"`
	CanFile bool   `json:"can_file"`
}

func recordTypes(t *testing.T, h http.Handler, a actor) map[string]recordTypeRow {
	t.Helper()
	out := map[string]recordTypeRow{}
	for _, r := range decodeBody[struct{ Items []recordTypeRow }](t, a.do(h, "GET", "/api/v1/record-types", "")).Items {
		out[r.Type] = r
	}
	return out
}

func permissionNames(t *testing.T, h http.Handler, a actor) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, p := range decodeBody[struct {
		Items []struct{ Name string }
	}](t, a.do(h, "GET", "/api/v1/permissions", "")).Items {
		out[p.Name] = true
	}
	return out
}

// An app adds a record type of its own: a prefix and numbering, its own lifecycle, form, rule, SLA and permissions,
// all through the same API as incidents. References look the same for every type: the prefix and the number.
func TestAnAppDefinesItsOwnRecordType(t *testing.T) {
	h, pool, reg := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	chris := newActor(t, h, pool, "chris@x.test", "end_user")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	installTyler(t, pool, reg)
	expect(t, admin.do(h, "POST", "/api/v1/roles/thing-keeper/grants", `{"user_id":"`+chris.id+`"}`), 201, "granting thing-keeper")

	types := recordTypes(t, h, chris)
	if got := types["tyler_thing"]; got.Prefix != "TYL" || got.Label != "Thing" || got.Plural != "Things" || got.App != "tyler" || !got.CanFile {
		t.Fatalf("the keeper's record types: %+v", types)
	}
	if recordTypes(t, h, lena)["tyler_thing"].CanFile {
		t.Error("an end user without the role may not file a Thing")
	}
	if types["incident"].Plural != "Incidents" || types["incident"].App != "" {
		t.Errorf("core types have plurals and no app: %+v", types["incident"])
	}
	if !permissionNames(t, h, admin)["records:create:tyler_thing"] {
		t.Error("the type's record permissions are in the catalog")
	}
	expect(t, chris.do(h, "GET", "/api/v1/lifecycles/tyler_thing", ""), 200, "the type's lifecycle")
	expect(t, chris.do(h, "GET", "/api/v1/forms/tyler_thing", ""), 200, "the type's form")

	// Filing: numbered from 1, like every type; the app's rule reads the form's field.
	first := decodeBody[thingDTO](t, chris.do(h, "POST", "/api/v1/tickets", `{"type":"tyler_thing","title":"Desk lamp","fields":{"size":"large"}}`))
	if first.Reference != "TYL1" || first.Prefix != "TYL" || first.Number != 1 || first.Status != "new" {
		t.Fatalf("the first Thing: %+v", first)
	}
	if first.Priority != 2 {
		t.Errorf("large-things-matter sets priority 2 on a large Thing, got %d", first.Priority)
	}
	if len(first.SLAs) != 1 || first.SLAs[0].Label != "Check" {
		t.Errorf("the app's SLA runs on its type: %+v", first.SLAs)
	}
	second := decodeBody[thingDTO](t, chris.do(h, "POST", "/api/v1/tickets", `{"type":"tyler_thing","title":"Chair"}`))
	if second.Reference != "TYL2" {
		t.Errorf("the second Thing is TYL2, got %s", second.Reference)
	}
	expect(t, chris.do(h, "POST", "/api/v1/tickets", `{"type":"tyler_thing","title":"Odd","fields":{"colour":"red"}}`), 422,
		"a field the form does not declare")
	incident := decodeBody[thingDTO](t, chris.do(h, "POST", "/api/v1/tickets", `{"title":"Printer jam"}`))
	if incident.Reference != "INC1" || incident.Prefix != "INC" {
		t.Errorf("an incident beside it: %+v", incident)
	}

	// Found by its reference in any case, or by id; every list carries the same reference.
	for _, ref := range []string{"TYL1", "tyl1", first.ID} {
		if got := decodeBody[thingDTO](t, chris.do(h, "GET", "/api/v1/tickets/"+ref, "")); got.ID != first.ID || got.Reference != "TYL1" {
			t.Errorf("GET /tickets/%s: %+v", ref, got)
		}
	}
	list := decodeBody[struct{ Items []thingDTO }](t, chris.do(h, "GET", "/api/v1/tickets?type=tyler_thing", "")).Items
	if len(list) != 2 || list[0].Reference != "TYL2" || list[1].Reference != "TYL1" || list[0].Prefix != "TYL" {
		t.Errorf("the queue of Things: %+v", list)
	}

	// Its lifecycle and guards: a Thing in use needs a serial number before it goes.
	expect(t, chris.do(h, "POST", "/api/v1/tickets/TYL1/transitions", `{"to":"in_use"}`), 200, "starting to use it")
	refusal(t, chris.do(h, "POST", "/api/v1/tickets/TYL1/transitions", `{"to":"gone"}`), 422, "serial number", "getting rid of it without a serial")
	expect(t, chris.do(h, "PATCH", "/api/v1/tickets/TYL1", `{"fields":{"serial":"SN-1"}}`), 200, "noting the serial")
	expect(t, chris.do(h, "POST", "/api/v1/tickets/TYL1/transitions", `{"to":"gone"}`), 200, "getting rid of it")

	// Permissions are per type, like any other.
	refusal(t, lena.do(h, "POST", "/api/v1/tickets", `{"type":"tyler_thing","title":"Mine"}`), 403, "records:create:tyler_thing", "an end user filing a Thing")
	expect(t, lena.do(h, "GET", "/api/v1/tickets/TYL2", ""), 404, "an end user reading someone's Thing")
	expect(t, lena.do(h, "GET", "/api/v1/tickets?type=tyler_thing", ""), 200, "an end user listing Things")
	if got := decodeBody[struct{ Items []thingDTO }](t, lena.do(h, "GET", "/api/v1/tickets?type=tyler_thing", "")).Items; len(got) != 0 {
		t.Errorf("an end user sees no one else's Things: %+v", got)
	}
}

// Removing the app retires its type: its records are kept and open by reference, but no list, filter, picker or
// permission catalog shows them and nobody files one. Installing it again brings it all back, numbering carried on.
func TestRemovingAnAppRetiresItsRecordType(t *testing.T) {
	h, pool, reg := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	installTyler(t, pool, reg)
	thing := decodeBody[thingDTO](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"tyler_thing","title":"Desk lamp"}`))
	if thing.Reference != "TYL1" {
		t.Fatalf("filed %+v", thing)
	}

	if _, err := apps.Uninstall(context.Background(), pool, "tyler"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := recordTypes(t, h, agent)["tyler_thing"]; ok {
		t.Error("a retired type is not offered")
	}
	if permissionNames(t, h, admin)["records:read:tyler_thing"] {
		t.Error("a retired type's permissions leave the catalog")
	}
	for _, tk := range decodeBody[struct{ Items []thingDTO }](t, agent.do(h, "GET", "/api/v1/tickets", "")).Items {
		if tk.Type == "tyler_thing" {
			t.Errorf("a retired type's record is in the queue: %+v", tk)
		}
	}
	expect(t, agent.do(h, "GET", "/api/v1/tickets?type=tyler_thing", ""), 400, "filtering by a retired type")
	// Its record still opens, so the page can still ask for its labels and form.
	expect(t, agent.do(h, "GET", "/api/v1/lifecycles/tyler_thing", ""), 200, "a retired type's lifecycle")
	expect(t, agent.do(h, "GET", "/api/v1/forms/tyler_thing", ""), 200, "a retired type's form")
	expect(t, agent.do(h, "GET", "/api/v1/lifecycles/no_such_type", ""), 404, "a type that never was")
	refusal(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"tyler_thing","title":"Another"}`), 422, "not a record type in use", "filing a retired type")
	if got := decodeBody[thingDTO](t, agent.do(h, "GET", "/api/v1/tickets/TYL1", "")); got.ID != thing.ID || got.Reference != "TYL1" {
		t.Errorf("a retired type's record still opens by reference: %+v", got)
	}

	installTyler(t, pool, reg)
	if _, ok := recordTypes(t, h, agent)["tyler_thing"]; !ok {
		t.Error("installing the app again brings the type back")
	}
	if again := decodeBody[thingDTO](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"tyler_thing","title":"Chair"}`)); again.Reference != "TYL2" {
		t.Errorf("numbering carries on after a reinstall: %+v", again)
	}
}

// withFile returns a copy of a bundle with one file replaced (or added).
func withFile(b *apps.Bundle, name, content string) *apps.Bundle {
	files := map[string][]byte{}
	for k, v := range b.Files {
		files[k] = v
	}
	files[name] = []byte(content)
	return &apps.Bundle{Files: files}
}

func smallApp(name string, files map[string]string) *apps.Bundle {
	out := map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: " + name + "\nversion: 1.0.0\nauthor: t\ndescription: \"A test app.\"\n" +
		"dependencies:\n  - tyler: \">=1.0.0\"\n")}
	for k, v := range files {
		out[k] = []byte(v)
	}
	return &apps.Bundle{Files: out}
}

func wantInstallError(t *testing.T, err error, want, what string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: want an error mentioning %q, got %v", what, want, err)
	}
}

// Names and prefixes are the instance's: two apps cannot share one, a core one cannot be taken, and a type's prefix
// stays while it has records, because it is part of their references. An app may build on a dependency's type.
func TestRecordTypeNamesAndPrefixesAreProtected(t *testing.T) {
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	installTyler(t, pool, reg)
	const typeFile = "default/types/tyler_thing.yaml"
	original := string(tylerBundle(t).Files[typeFile])

	theirs := "kind: record_type\nname: tyler_thing\nprefix: OTH\nlabel: Other\n"
	wantInstallError(t, installBundle(t, pool, reg, smallApp("other", map[string]string{"default/types/tyler_thing.yaml": theirs})),
		"already provided by tyler", "another app defining the same type")
	sameprefix := "kind: record_type\nname: other_thing\nprefix: INC\nlabel: Other\n"
	wantInstallError(t, installBundle(t, pool, reg, smallApp("other", map[string]string{"default/types/other_thing.yaml": sameprefix})),
		"already the prefix of incident", "a prefix a core type has")
	core := "kind: record_type\nname: incident\nprefix: OTH\nlabel: Other\n"
	wantInstallError(t, installBundle(t, pool, reg, smallApp("other", map[string]string{"default/types/incident.yaml": core})),
		"is a core record type", "an app defining a core type")
	typo := "kind: rule\nname: r1\nrecord_type: tyler_thingz\ntiming: before\non: [create]\nrun:\n  - action: abort\n    message: no\n"
	wantInstallError(t, installBundle(t, pool, reg, smallApp("other", map[string]string{"default/rules/r1.yaml": typo})),
		"tyler_thingz", "a dependency's type misspelled")

	// A dependent app's rule on tyler's type is fine, and runs.
	rule := "kind: rule\nname: no-ghosts\nrecord_type: tyler_thing\ntiming: before\non: [create]\nwhen: record.title == \"Ghost\"\nrun:\n  - action: abort\n    message: Ghosts are not things.\n"
	if err := installBundle(t, pool, reg, smallApp("other", map[string]string{"default/rules/no-ghosts.yaml": rule})); err != nil {
		t.Fatalf("an app building on its dependency's type: %v", err)
	}
	refusal(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"tyler_thing","title":"Ghost"}`), 422, "Ghosts are not things", "the dependent app's rule")
	if _, err := apps.Uninstall(context.Background(), pool, "other"); err != nil {
		t.Fatal(err)
	}

	// Its prefix may change while it has no records, and not after.
	renamed := withFile(tylerBundle(t), typeFile, strings.Replace(original, "prefix: TYL", "prefix: THG", 1))
	if err := installBundle(t, pool, reg, renamed); err != nil {
		t.Fatalf("changing the prefix of a type with no records: %v", err)
	}
	if got := decodeBody[thingDTO](t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"tyler_thing","title":"Lamp"}`)); got.Reference != "THG1" {
		t.Fatalf("filed %+v", got)
	}
	wantInstallError(t, installBundle(t, pool, reg, tylerBundle(t)), "has 1 records, so its prefix stays THG", "changing the prefix once there are records")
}
