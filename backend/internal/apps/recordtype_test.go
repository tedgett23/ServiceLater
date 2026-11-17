package apps

import (
	"strings"
	"testing"
)

const thingType = "kind: record_type\nname: demo_thing\nprefix: DMO\nlabel: Thing\n"

func TestARecordTypeIsCheckedWhenCompiled(t *testing.T) {
	c, p := compile(t, map[string]string{"app.yaml": goodManifest, "default/types/demo_thing.yaml": thingType})
	if p != nil {
		t.Fatal(p)
	}
	rt := c.Definitions[0].RecordType
	if rt == nil || rt.Name != "demo_thing" || rt.Prefix != "DMO" || rt.Plural != "Things" {
		t.Fatalf("compiled %+v", rt)
	}

	for name, tc := range map[string]struct{ src, want string }{
		"core name":      {strings.Replace(thingType, "demo_thing", "incident", 1), `"incident" is a core record type`},
		"bad name":       {strings.Replace(thingType, "demo_thing", "Demo-Thing", 1), `name "Demo-Thing" must be`},
		"bad prefix":     {strings.Replace(thingType, "DMO", "dm1", 1), `prefix "dm1" must be 2-5 capital letters`},
		"padding":        {thingType + "number_width: 3\n", "field number_width not found"},
		"no label":       {strings.Replace(thingType, "label: Thing\n", "", 1), "label is required"},
		"unknown option": {thingType + "colour: red\n", "field colour not found"},
	} {
		_, p := compile(t, map[string]string{"app.yaml": goodManifest, "default/types/demo_thing.yaml": tc.src})
		if !strings.Contains(p.Error(), tc.want) {
			t.Errorf("%s: want %q in %v", name, tc.want, p)
		}
	}

	_, p = compile(t, map[string]string{"app.yaml": goodManifest, "default/types/demo_thing.yaml": thingType,
		"default/types/demo_other.yaml": strings.Replace(thingType, "demo_thing", "demo_other", 1)})
	if !strings.Contains(p.Error(), "is already the prefix of demo_") {
		t.Errorf("two types of one app with one prefix: %v", p)
	}
}

// Definitions may name the app's own types and its dependencies' (by the dependency's name), and nothing else.
func TestDefinitionsNameTypesTheAppCanSee(t *testing.T) {
	rule := func(typ string) string {
		return "kind: rule\nname: r1\nrecord_type: " + typ + "\ntiming: before\non: [create]\nrun:\n  - action: abort\n    message: no\n"
	}
	own := map[string]string{"app.yaml": strings.Replace(goodManifest, "records:read:*", "records:read:demo_thing", 1),
		"default/types/demo_thing.yaml": thingType, "default/rules/r1.yaml": rule("demo_thing"),
		"default/forms/demo_thing.yaml": "kind: form\nrecord_type: demo_thing\nfields:\n  size: { type: text, label: Size }\nlayout:\n  create: [title, size]\n"}
	if _, p := compile(t, own); p != nil {
		t.Fatalf("the app's own type: %v", p)
	}

	c, p := compile(t, map[string]string{"app.yaml": goodManifest, "default/rules/r1.yaml": rule("core_itsm_widget")})
	if p != nil || len(c.ForeignTypes) != 1 || c.ForeignTypes[0] != "core_itsm_widget" {
		t.Fatalf("a dependency's type is left for the install to check: %+v %v", c, p)
	}

	for name, files := range map[string]map[string]string{
		"rule":       {"default/rules/r1.yaml": rule("widget")},
		"permission": {"default/roles/r.yaml": "kind: role\nname: odd\nlabel: Odd\npermissions: [records:read:widget]\n"},
		"lookup": {"default/rules/r1.yaml": "kind: rule\nname: r1\nrecord_type: incident\ntiming: before\non: [create]\n" +
			"lookups:\n  n:\n    count: widget\n    where:\n      status: { eq: x }\nwhen: lookups.n > 0\nrun:\n  - action: abort\n    message: no\n"},
	} {
		files["app.yaml"] = goodManifest
		if _, p := compile(t, files); !strings.Contains(p.Error(), `"widget" is neither a core type`) {
			t.Errorf("%s naming an unknown type: %v", name, p)
		}
	}
}
