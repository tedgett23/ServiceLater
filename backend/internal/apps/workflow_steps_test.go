package apps

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const stepsManifest = "apiVersion: 1\nname: test-app\nversion: 1.0.0\nauthor: t\ndescription: \"Test app.\"\n" +
	"permissions: [records:read:*, \"http:api.example.com\", \"http:*.example.org\"]\nconfig:\n  - { key: token, label: API token, type: secret }\n"

const childWorkflow = "kind: workflow\nname: child\nrecord_type: incident\nstart: e\nsteps:\n  e:\n    type: end\n"

func stepsApp(workflow string, more ...string) map[string]string {
	files := map[string]string{"app.yaml": stepsManifest, "default/workflows/child.yaml": childWorkflow, "default/workflows/main.yaml": workflow}
	for i := 0; i+1 < len(more); i += 2 {
		files[more[i]] = more[i+1]
	}
	return files
}

func flow(steps string, header ...string) string {
	return "kind: workflow\nname: main\nrecord_type: incident\nstart: s\n" + strings.Join(header, "") + "steps:\n" + steps + "  done:\n    type: end\n"
}

const everyStep = "  s:\n    type: query\n    find:\n      type: incident\n      where:\n        state_category: { eq: open }\n        created_at: { lt: =now }\n      limit: 50\n    next: each\n" +
	"  each:\n    type: foreach\n    workflow: child\n    items: =vars.s_ids\n    each: record\n    parallel: 3\n    keep_going: true\n    inputs:\n      why: sweep\n    timeout: 2h\n    on_timeout: done\n    next: one\n" +
	"  one:\n    type: subflow\n    workflow: child\n    record: =record.id\n    next: call\n    on_error: done\n" +
	"  call:\n    type: http.request\n    method: POST\n    url: https://api.example.com/v1/items\n    headers:\n      X-Source: servicelater\n" +
	"    body: '={\"title\": record.title}'\n    secret: token\n    auth: header\n    auth_header: X-Api-Key\n    retries: 2\n    timeout: 15s\n    next: file\n    on_error: done\n" +
	"  file:\n    type: record.create\n    record_type: work_task\n    title: Follow up\n    group: Service Desk\n    parent: =record.id\n    fields:\n      note: =record.title\n    next: tell\n" +
	"  tell:\n    type: notify\n    template: heads-up\n    recipients: [requester, \"=[record.requester_id]\"]\n    next: done\n"

// Every new step, and a schedule, compile; written back out they read the same.
func TestTheNewStepsCompileAndWriteBack(t *testing.T) {
	c, p := compile(t, stepsApp(flow(everyStep, "schedule:\n  cron: \"0 8 * * 1-5\"\n  timezone: Europe/London\n  for_each:\n    type: incident\n    where:\n      state_category: { eq: open }\n"),
		"default/notifications/heads-up.yaml", "kind: notification\nname: heads-up\ntitle: Look\n"))
	if p != nil {
		t.Fatal(p)
	}
	for _, d := range c.Definitions {
		if d.Workflow == nil || d.Workflow.Name != "main" {
			continue
		}
		for _, st := range d.Workflow.Steps {
			if err := st.Err(); err != nil {
				t.Errorf("step %s: %v", st.ID, err)
			}
		}
		if got := d.Workflow.Schedule.Next(mustTime("2026-11-20T09:00:00Z")); got.UTC().Format("2006-01-02T15:04") != "2026-11-23T08:00" {
			t.Errorf("the next run after Friday 9:00: %v", got)
		}
		data, err := WorkflowYAML(d.Workflow, false)
		if err != nil {
			t.Fatal(err)
		}
		var p Problems
		again := parseWorkflow(d.Path, data, formSet{}, nil, &p)
		if again == nil {
			t.Fatalf("does not read back: %v\n%s", p, data)
		}
		again.Fields = d.Workflow.Fields
		want, _ := json.Marshal(d.Workflow)
		got, _ := json.Marshal(again)
		if string(want) != string(got) {
			t.Errorf("changed on the way back:\nwant %s\n got %s", want, got)
		}
	}
}

func TestNewStepMistakesFailTheInstall(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"no such workflow":      {stepsApp(flow("  s:\n    type: subflow\n    workflow: nope\n    next: done\n")), `there is no workflow "nope" in this app`},
		"calls itself":          {stepsApp(flow("  s:\n    type: subflow\n    workflow: main\n    next: done\n")), "runs itself through its subflows"},
		"foreach without items": {stepsApp(flow("  s:\n    type: foreach\n    workflow: child\n    next: done\n")), "foreach needs items"},
		"too parallel":          {stepsApp(flow("  s:\n    type: foreach\n    workflow: child\n    items: '[1]'\n    parallel: 50\n    next: done\n")), "parallel must be 1 to 10"},
		"unlisted host":         {stepsApp(flow("  s:\n    type: http.request\n    method: GET\n    url: https://evil.example.com/x\n    next: done\n")), "evil.example.com is not a host this app may call"},
		"undeclared secret":     {stepsApp(flow("  s:\n    type: http.request\n    method: GET\n    url: https://api.example.com/x\n    secret: other\n    auth: bearer\n    next: done\n")), `declares no secret "other"`},
		"bad method":            {stepsApp(flow("  s:\n    type: http.request\n    method: FETCH\n    url: https://api.example.com/x\n    next: done\n")), "method must be one of"},
		"body on GET":           {stepsApp(flow("  s:\n    type: http.request\n    method: GET\n    url: https://api.example.com/x\n    body: hi\n    next: done\n")), "http.request does not take body"},
		"long timeout":          {stepsApp(flow("  s:\n    type: http.request\n    method: GET\n    url: https://api.example.com/x\n    timeout: 2m\n    next: done\n")), "timeout must be between 1s and 30s"},
		"query field":           {stepsApp(flow("  s:\n    type: query\n    find:\n      type: incident\n      where:\n        title: { eq: x }\n    next: done\n")), `cannot be filtered by "title"`},
		"query type":            {stepsApp(flow("  s:\n    type: query\n    find:\n      type: freeze_window\n    next: done\n")), "is not a record type that can be found"},
		"no template":           {stepsApp(flow("  s:\n    type: notify\n    template: nope\n    recipients: [assignee]\n    next: done\n")), `no notification template "nope"`},
		"bad recipient":         {stepsApp(flow("  s:\n    type: notify\n    template: x\n    recipients: [everyone]\n    next: done\n")), `"everyone" is not a recipient`},
		"bad cron":              {stepsApp(flow("  s:\n    type: end\n", "schedule:\n  cron: \"every day\"\n")), "schedule: cron"},
		"bad zone":              {stepsApp(flow("  s:\n    type: end\n", "schedule:\n  cron: \"0 8 * * *\"\n  timezone: Mars/Olympus\n")), "is not a time zone"},
		"bad host permission":   {map[string]string{"app.yaml": strings.Replace(stepsManifest, "http:api.example.com", "http:https://x", 1)}, "is not a host to allow"},
	} {
		_, p := compile(t, tc.files)
		if p == nil || !strings.Contains(p.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, p)
		}
	}
}

func TestHostsAreMatched(t *testing.T) {
	hosts := HTTPHosts([]string{"records:read:*", "http:api.example.com", "http:*.example.org"})
	for host, want := range map[string]bool{"api.example.com": true, "API.example.com": true, "x.example.com": false, "a.example.org": true,
		"b.a.example.org": true, "example.org": false, "evilexample.org": false} {
		if HostAllowed(hosts, host) != want {
			t.Errorf("%s: want %v", host, want)
		}
	}
	if AppSecretName("change-management", "Api-Token") != "apps.change_management.api_token" {
		t.Error(AppSecretName("change-management", "Api-Token"))
	}
}

func mustTime(s string) time.Time {
	out, _ := time.Parse(time.RFC3339, s)
	return out
}
