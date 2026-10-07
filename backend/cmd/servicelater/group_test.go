package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func groupCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runGroup(context.Background(), args, &out)
	return out.String(), err
}

func TestGroupCommand(t *testing.T) {
	scratch(t)
	if _, err := dbPool(t).Exec(context.Background(), `INSERT INTO users (email, name, role) VALUES ('ann@x.test', 'Ann', 'agent')`); err != nil {
		t.Fatal(err)
	}
	if out, err := groupCLI(t, "list"); err != nil || !strings.Contains(out, "no groups") {
		t.Fatalf("empty list: %v\n%s", err, out)
	}
	if _, err := groupCLI(t, "create"); err == nil || !strings.Contains(err.Error(), "needs -name") {
		t.Errorf("create without a name: %v", err)
	}
	if out, err := groupCLI(t, "create", "-name", "Network", "-description", "Links"); err != nil || !strings.Contains(out, "created Network") {
		t.Fatalf("create: %v\n%s", err, out)
	}
	if _, err := groupCLI(t, "create", "-name", "NETWORK"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate: %v", err)
	}
	if out, err := groupCLI(t, "add", "-group", "network", "-email", "ANN@x.test"); err != nil || !strings.Contains(out, "ann@x.test is in Network") {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, _ := groupCLI(t, "members", "-group", "Network"); !strings.Contains(out, "Ann") {
		t.Errorf("members:\n%s", out)
	}
	if out, _ := groupCLI(t, "list"); !strings.Contains(out, "1 member") {
		t.Errorf("list:\n%s", out)
	}
	if _, err := groupCLI(t, "add", "-group", "Nope", "-email", "ann@x.test"); err == nil || !strings.Contains(err.Error(), `no group named "Nope"`) {
		t.Errorf("unknown group: %v", err)
	}
	if _, err := groupCLI(t, "add", "-group", "Network", "-email", "who@x.test"); err == nil || !strings.Contains(err.Error(), "no user") {
		t.Errorf("unknown user: %v", err)
	}
	if out, err := groupCLI(t, "remove", "-group", "Network", "-email", "ann@x.test"); err != nil || !strings.Contains(out, "no longer in") {
		t.Errorf("remove: %v\n%s", err, out)
	}
	if _, err := groupCLI(t, "remove", "-group", "Network", "-email", "ann@x.test"); err == nil {
		t.Error("removed twice")
	}
	if _, err := groupCLI(t, "frobnicate"); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("unknown command: %v", err)
	}
}
