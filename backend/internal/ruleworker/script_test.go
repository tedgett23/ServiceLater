package ruleworker_test

import (
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/ruleruns"
)

// An async rule's script makes its changes through the save path as the app, for the person, in the job's
// transaction: a field change with its comment, and an internal note. A mistake in the script is not retried.
func TestAnAsyncScriptActionMakesItsChanges(t *testing.T) {
	w := setup(t, "")
	w.installApp(t, "house", map[string]string{"triage": asyncRule("triage", "update",
		"run:\n  - action: script\n    script: |\n"+
			"      if \"printer\" in record.title.lower():\n"+
			"          update(record, {\"priority\": 1}, comment=\"Raised: \" + record.title)\n"+
			"      comment(record, \"Looked at for \" + user.name, internal=True)\n")})
	if err := w.runJob(w.jobFor("house", "triage", w.agent, ruleruns.Cause{})); err != nil {
		t.Fatal(err)
	}
	if r := w.row(t); r.Priority != 1 {
		t.Errorf("priority: %+v", r)
	}
	var bodies []string
	for _, c := range w.comments(t) {
		bodies = append(bodies, c.TaskComment.Body)
	}
	if got := strings.Join(bodies, " | "); !strings.Contains(got, "Raised: Printer") || !strings.Contains(got, "Looked at for Ada") {
		t.Errorf("comments: %s", got)
	}

	w.installApp(t, "house", map[string]string{"triage": asyncRule("triage", "update", "run:\n  - action: script\n    script: |\n      x = record.title + 1\n")})
	err := w.runJob(w.jobFor("house", "triage", w.agent, ruleruns.Cause{}))
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("a broken script: %v", err)
	}
	if runs := w.audit(t); runs[0].Outcome != ruleruns.Failed || !strings.Contains(runs[0].Detail, "the script failed at line 1") {
		t.Errorf("audit: %+v", runs[0])
	}
}
