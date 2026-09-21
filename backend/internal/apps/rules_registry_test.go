package apps_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
)

func ruleApp(t *testing.T, name string, rules map[string]string) *apps.Compiled {
	t.Helper()
	files := map[string][]byte{"app.yaml": fmt.Appendf(nil, "apiVersion: 1\nname: %s\nversion: 1.0.0\nauthor: t\ndescription: \"Test app.\"\n", name)}
	for file, src := range rules {
		files["default/rules/"+file+".yaml"] = []byte(src)
	}
	c, err := (&apps.Bundle{Files: files}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func rule(name, recordType, timing string, order int) string {
	return fmt.Sprintf("kind: rule\nname: %s\nrecord_type: %s\ntiming: %s\non: [create, update]\norder: %d\nrun:\n  - action: abort\n    message: no\n", name, recordType, timing, order)
}

func ruleNames(rs []*apps.Rule) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

func TestRegistryOrdersRules(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	for _, c := range []*apps.Compiled{
		ruleApp(t, "alpha", map[string]string{
			"late":  rule("late", "incident", "before", 50),
			"early": rule("early", "incident", "before", 10),
			"base":  rule("base", "task", "before", 90),
			"after": rule("after-one", "incident", "after", 0),
		}),
		// Same order as alpha's "early": the app name breaks the tie, then the rule name.
		ruleApp(t, "beta", map[string]string{"tie": rule("tie", "incident", "before", 10)}),
	} {
		if _, err := apps.Install(ctx, pool, c, apps.InstallOptions{InstalledBy: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	r := loaded(t, pool)

	// Base `task` rules run first, whatever their order number, then the type's own by order, app, name.
	got := ruleNames(r.Rules("incident", "before", "update"))
	if want := []string{"base", "early", "tie", "late"}; !slices.Equal(got, want) {
		t.Errorf("before rules = %v, want %v", got, want)
	}
	if got := ruleNames(r.Rules("incident", "after", "create")); !slices.Equal(got, []string{"after-one"}) {
		t.Errorf("after rules = %v", got)
	}
	// Asking for `task` itself lists its rules once, and other types get only the base.
	if got := ruleNames(r.Rules("task", "before", "create")); !slices.Equal(got, []string{"base"}) {
		t.Errorf("task rules = %v", got)
	}
	if got := ruleNames(r.Rules("change", "before", "create")); !slices.Equal(got, []string{"base"}) {
		t.Errorf("change rules = %v", got)
	}
	if got := r.Rules("incident", "before", "delete"); len(got) != 0 {
		t.Errorf("delete rules = %v", ruleNames(got))
	}
}
