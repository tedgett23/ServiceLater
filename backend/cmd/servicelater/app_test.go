package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
)

func cli(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runApp(context.Background(), args, &out)
	return out.String(), err
}

func firstParty(name string) string { return filepath.Join(dbtest.AppsDir(), name) }

func scratch(t *testing.T) {
	t.Helper()
	dbtest.Pool(t)                             // migrated and emptied
	t.Setenv("SL_DATABASE_URL", dbtest.DSN(t)) // the CLI reads its database from the environment
}

func TestValidate(t *testing.T) {
	out, err := cli(t, "validate", firstParty("incident-management"), firstParty("core-itsm"))
	if err != nil || strings.Count(out, ": ok (") != 2 {
		t.Fatalf("valid apps: %v\n%s", err, out)
	}

	bad := t.TempDir()
	_ = os.WriteFile(filepath.Join(bad, "app.yaml"), []byte("apiVersion: 1\nname: Bad_Name\nversion: 1\ndescription: x\n"), 0o600)
	out, err = cli(t, "validate", firstParty("core-itsm"), bad)
	if err == nil || !strings.Contains(err.Error(), "1 app failed validation") {
		t.Errorf("want a failure count, got %v", err)
	}
	if !strings.Contains(out, "core-itsm: ok") || !strings.Contains(out, "invalid") || !strings.Contains(out, "Bad_Name") {
		t.Errorf("output should show the good app and explain the bad one:\n%s", out)
	}
}

func TestPack(t *testing.T) {
	dir := t.TempDir()
	out, err := cli(t, "pack", "-o", dir, firstParty("incident-management"))
	if err != nil || !strings.Contains(out, "incident-management-0.1.0.swapp") {
		t.Fatalf("pack: %v\n%s", err, out)
	}
	if _, err := cli(t, "validate", filepath.Join(dir, "incident-management-0.1.0.swapp")); err != nil {
		t.Errorf("a packed app must validate: %v", err)
	}
}

func TestInstallOrdersDependenciesAndReportsThePlan(t *testing.T) {
	scratch(t)
	// Given in the wrong order on purpose.
	out, err := cli(t, "install", firstParty("incident-management"), firstParty("core-itsm"))
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if strings.Index(out, "core-itsm") > strings.Index(out, "incident-management") {
		t.Errorf("core-itsm must be installed first:\n%s", out)
	}
	if !strings.Contains(out, "lifecycle incident") {
		t.Errorf("the plan should list the lifecycle:\n%s", out)
	}

	out, _ = cli(t, "list")
	if !strings.Contains(out, "core-itsm") || !strings.Contains(out, "incident-management") || !strings.Contains(out, "1 definition") {
		t.Errorf("list:\n%s", out)
	}

	out, err = cli(t, "install", firstParty("core-itsm"), firstParty("incident-management"))
	if err != nil || strings.Count(out, "(no changes)") != 2 {
		t.Errorf("reinstalling should change nothing: %v\n%s", err, out)
	}
}

func TestInstallValidatesEverythingBeforeWritingAnything(t *testing.T) {
	scratch(t)
	bad := t.TempDir()
	_ = os.WriteFile(filepath.Join(bad, "app.yaml"), []byte("nonsense"), 0o600)
	if _, err := cli(t, "install", firstParty("core-itsm"), bad); err == nil {
		t.Fatal("an invalid app was installed")
	}
	out, _ := cli(t, "list")
	if !strings.Contains(out, "no apps installed") {
		t.Errorf("the valid app was installed even though another one was not:\n%s", out)
	}
}

// A batch is one transaction: if a later app cannot be installed, an earlier one is not left behind.
func TestABatchInstallIsAllOrNothing(t *testing.T) {
	scratch(t)
	orphan := t.TempDir()
	_ = os.WriteFile(filepath.Join(orphan, "app.yaml"),
		[]byte("apiVersion: 1\nname: orphan\nversion: 1.0.0\ndescription: \"Needs something absent.\"\ndependencies:\n  - not-installed: \">=1.0.0\"\n"), 0o600)
	if _, err := cli(t, "install", firstParty("core-itsm"), orphan); err == nil || !strings.Contains(err.Error(), "not-installed") {
		t.Fatalf("want the missing dependency reported, got %v", err)
	}
	if out, _ := cli(t, "list"); !strings.Contains(out, "no apps installed") {
		t.Errorf("core-itsm was left installed although the batch failed:\n%s", out)
	}
}

func TestInstallNeedsItsDependencies(t *testing.T) {
	scratch(t)
	_, err := cli(t, "install", firstParty("incident-management"))
	if err == nil || !strings.Contains(err.Error(), "core-itsm") || !strings.Contains(err.Error(), "install it first") {
		t.Errorf("want a hint about core-itsm, got %v", err)
	}
}

func TestDryRun(t *testing.T) {
	scratch(t)
	out, err := cli(t, "install", "-dry-run", firstParty("core-itsm"))
	if err != nil || !strings.Contains(out, "dry run: nothing was changed") || !strings.Contains(out, "core-itsm 0.1.0 (new)") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if out, _ := cli(t, "list"); !strings.Contains(out, "no apps installed") {
		t.Errorf("a dry run installed something:\n%s", out)
	}
}

func TestUninstall(t *testing.T) {
	scratch(t)
	_, _ = cli(t, "install", firstParty("core-itsm"), firstParty("incident-management"))

	if _, err := cli(t, "uninstall", "core-itsm"); err == nil || !strings.Contains(err.Error(), "incident-management depends on it") {
		t.Errorf("uninstalling a dependency: %v", err)
	}
	out, err := cli(t, "uninstall", "incident-management", "core-itsm")
	if err != nil || !strings.Contains(out, "records kept") {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if out, _ := cli(t, "list"); !strings.Contains(out, "no apps installed") {
		t.Errorf("list:\n%s", out)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}, {"validate"}, {"pack"}, {"install"}, {"uninstall"}} {
		if _, err := cli(t, args...); err == nil {
			t.Errorf("%v should print usage", args)
		}
	}
}

func TestOrderByDependenciesRejectsLoopsAndDuplicates(t *testing.T) {
	mk := func(name, dep string) *apps.Compiled {
		c := &apps.Compiled{}
		c.Manifest.Name = name
		if dep != "" {
			c.Manifest.Deps = []apps.Dependency{{Name: dep, Constraint: ">=0.0.1"}}
		}
		return c
	}
	if _, err := apps.OrderByDependencies([]*apps.Compiled{mk("a", "b"), mk("b", "a")}); err == nil || !strings.Contains(err.Error(), "loop") {
		t.Errorf("loop: %v", err)
	}
	if _, err := apps.OrderByDependencies([]*apps.Compiled{mk("a", ""), mk("a", "")}); err == nil {
		t.Error("duplicate accepted")
	}
	got, err := apps.OrderByDependencies([]*apps.Compiled{mk("c", "b"), mk("b", "a"), mk("a", "")})
	if err != nil || got[0].Manifest.Name != "a" || got[2].Manifest.Name != "c" {
		t.Errorf("order: %v %v", got, err)
	}
}

func TestSyncInstallsTheBundledApps(t *testing.T) {
	scratch(t)
	out, err := cli(t, "sync")
	if err != nil || !strings.Contains(out, "core-itsm 0.1.0 (new)") || !strings.Contains(out, "incident-management 0.1.0 (new)") {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	out, _ = cli(t, "sync")
	if strings.Count(out, "(no changes)") != 2 {
		t.Errorf("a second sync should change nothing:\n%s", out)
	}

	t.Setenv("SL_BUNDLED_APPS", "none")
	if out, err := cli(t, "sync"); err != nil || strings.Contains(out, "core-itsm") {
		t.Errorf("none means none: %v\n%s", err, out)
	}
	t.Setenv("SL_BUNDLED_APPS", "no-such-app")
	if _, err := cli(t, "sync"); err == nil || !strings.Contains(err.Error(), "not bundled") {
		t.Errorf("an unknown bundled app should be an error: %v", err)
	}
}

func TestSyncDryRunWritesNothing(t *testing.T) {
	scratch(t)
	out, err := cli(t, "sync", "-dry-run")
	if err != nil || !strings.Contains(out, "dry run: nothing was changed") || !strings.Contains(out, "core-itsm 0.1.0 (new)") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if out, _ := cli(t, "list"); !strings.Contains(out, "no apps installed") {
		t.Errorf("a dry run installed something:\n%s", out)
	}
}

// The operator's local layer is applied to a bundled app, shows up in the plan and in list, and is removed by
// deleting the file: nothing about it is stored anywhere the binary could overwrite.
func TestLocalChangesToABundledApp(t *testing.T) {
	scratch(t)
	root := t.TempDir()
	t.Setenv("SL_APPS_LOCAL_DIR", root)
	override := filepath.Join(root, "incident-management", "lifecycles", "incident.yaml")
	if err := os.MkdirAll(filepath.Dir(override), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte("states:\n  new:\n    label: Fresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := cli(t, "sync")
	if err != nil || !strings.Contains(out, "lifecycle incident (default + instance)") {
		t.Fatalf("the plan should say the definition includes local changes: %v\n%s", err, out)
	}
	if out, _ := cli(t, "list"); !strings.Contains(out, "1 with local changes") {
		t.Errorf("list should count local changes:\n%s", out)
	}

	if err := os.Remove(override); err != nil {
		t.Fatal(err)
	}
	out, err = cli(t, "sync")
	if err != nil || !regexp.MustCompile(`change\s+lifecycle incident`).MatchString(out) || strings.Contains(out, "instance") || strings.Contains(out, "version bump") {
		t.Errorf("removing the local file should put the shipped definition back:\n%s", out)
	}
	if out, _ := cli(t, "list"); strings.Contains(out, "local changes") {
		t.Errorf("list still reports local changes:\n%s", out)
	}
}

func TestUninstallExplainsThatBundledAppsComeBack(t *testing.T) {
	scratch(t)
	_, _ = cli(t, "sync")
	out, err := cli(t, "uninstall", "incident-management")
	if err != nil || !strings.Contains(out, "installed again at the next start") {
		t.Errorf("want a note about SL_BUNDLED_APPS: %v\n%s", err, out)
	}
	t.Setenv("SL_BUNDLED_APPS", "none")
	out, err = cli(t, "uninstall", "core-itsm")
	if err != nil || strings.Contains(out, "installed again") {
		t.Errorf("no note when the app is not bundled by config: %v\n%s", err, out)
	}
}
