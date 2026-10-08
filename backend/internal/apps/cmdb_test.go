package apps

import (
	"strings"
	"testing"
)

const hardwareClass = `kind: class
name: hardware
label: Hardware
fields:
  serial_number: { type: text, label: Serial number, max_length: 50 }
  warranty_ends: { type: date, label: Warranty ends }
`

const serverClass = `kind: class
name: server
label: Server
extends: hardware
fields:
  hostname: { type: text, label: Hostname, required: true }
  environment:
    type: choice
    label: Environment
    options: [production, test]
    default: production
`

const dependsOn = `kind: relationship
name: depends_on
label: Depends on
reverse: Used by
impact: true
`

func cmdbApp(more ...string) map[string]string {
	files := map[string]string{"app.yaml": goodManifest, "default/classes/hardware.yaml": hardwareClass,
		"default/classes/server.yaml": serverClass, "default/relationships/depends_on.yaml": dependsOn}
	for i := 0; i+1 < len(more); i += 2 {
		files[more[i]] = more[i+1]
	}
	return files
}

// A class has the fields of the classes it extends before its own, and counts as each of them.
func TestAClassBuildsOnTheOneItExtends(t *testing.T) {
	c, p := compile(t, cmdbApp())
	if p != nil {
		t.Fatal(p)
	}
	classes := map[string]*Class{}
	var rel *Relationship
	for _, d := range c.Definitions {
		if d.Class != nil {
			classes[d.Name] = d.Class
		}
		if d.Relationship != nil {
			rel = d.Relationship
		}
	}
	resolveClasses(classes)
	srv := classes["server"]
	var fields []string
	for _, fd := range srv.All {
		fields = append(fields, fd.Name)
	}
	if strings.Join(fields, ",") != "serial_number,warranty_ends,hostname,environment" || strings.Join(srv.Ancestors, ",") != "hardware" {
		t.Errorf("server: fields %v, ancestors %v", fields, srv.Ancestors)
	}
	if d := srv.Defaults(); len(d) != 1 || d["environment"] != "production" {
		t.Errorf("defaults: %v", d)
	}
	if rel == nil || !rel.Impact || rel.Reverse != "Used by" {
		t.Errorf("relationship: %+v", rel)
	}
}

func TestClassesAndRelationshipsAreCheckedAtInstall(t *testing.T) {
	with := func(old, new string) string { return strings.Replace(serverClass, old, new, 1) }
	for name, tc := range map[string]struct {
		file, src, want string
	}{
		"missing parent":   {"default/classes/server.yaml", with("extends: hardware", "extends: hardwear"), `extends: there is no class "hardwear" in this app`},
		"self":             {"default/classes/server.yaml", with("extends: hardware", "extends: server"), "a class cannot extend itself"},
		"inherited twice":  {"default/classes/server.yaml", with("  hostname:", "  serial_number: { type: text, label: Serial }\n  hostname:"), "hardware already has a field called serial_number"},
		"own column":       {"default/classes/server.yaml", with("  hostname:", "  status:"), "configuration items have a field of their own called status"},
		"condition":        {"default/classes/server.yaml", with("required: true", "required_when: record.priority < 2"), "a class's fields have no conditions"},
		"bad name":         {"default/classes/server.yaml", with("name: server", "name: Server"), `name "Server" must be 2-40 characters`},
		"no label":         {"default/classes/server.yaml", with("label: Server\n", ""), "label is required"},
		"bad field type":   {"default/classes/server.yaml", with("type: text, label: Hostname", "type: ip, label: Hostname"), `type must be one of text`},
		"relation reverse": {"default/relationships/depends_on.yaml", strings.Replace(dependsOn, "reverse: Used by\n", "", 1), "reverse is required"},
		"relation kind":    {"default/relationships/depends_on.yaml", strings.Replace(dependsOn, "kind: relationship", "kind: class", 1), "kind must be relationship"},
	} {
		_, p := compile(t, cmdbApp(tc.file, tc.src))
		if p == nil || !strings.Contains(p.Error(), tc.want) {
			t.Errorf("%s: want %q in %v", name, tc.want, p)
		}
	}
	loop := strings.Replace(hardwareClass, "label: Hardware\n", "label: Hardware\nextends: server\n", 1)
	if _, p := compile(t, cmdbApp("default/classes/hardware.yaml", loop)); p == nil || !strings.Contains(p.Error(), "never in a loop") {
		t.Errorf("a loop: %v", p)
	}
}

// An operator adds a class of their own, and a field to a shipped one, in the local layer.
func TestALocalLayerExtendsTheShippedClasses(t *testing.T) {
	c, p := compile(t, cmdbApp(
		"local/classes/load_balancer.yaml", "kind: class\nname: load_balancer\nlabel: Load balancer\nextends: server\nfields:\n  vip: { type: text, label: Virtual IP }\n",
		"local/classes/hardware.yaml", "fields:\n  rack: { type: text, label: Rack }\n",
	))
	if p != nil {
		t.Fatal(p)
	}
	classes := map[string]*Class{}
	for _, d := range c.Definitions {
		if d.Class != nil {
			classes[d.Name] = d.Class
		}
	}
	resolveClasses(classes)
	lb := classes["load_balancer"]
	var fields []string
	for _, fd := range lb.All {
		fields = append(fields, fd.Name)
	}
	if strings.Join(fields, ",") != "serial_number,warranty_ends,rack,hostname,environment,vip" || strings.Join(lb.Ancestors, ",") != "server,hardware" {
		t.Errorf("load balancer: fields %v, ancestors %v", fields, lb.Ancestors)
	}
}
