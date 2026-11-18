package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
)

// Admin → Connections lists the apps that call out (the hosts they may call, the secrets they declare) and sets a
// secret, which is kept encrypted and never shown again.
func TestConnectionsSetAnAppsSecrets(t *testing.T) {
	h, pool, reg, keys := setupEmail(t, testKey(9))
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	c, err := (&apps.Bundle{Files: map[string][]byte{"app.yaml": []byte("apiVersion: 1\nname: hooks\nversion: 1.0.0\nauthor: t\ndescription: \"Hooks.\"\n" +
		"permissions: [\"http:api.example.com\"]\nconfig:\n  - { key: token, label: Example API token, type: secret }\n")}}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Install(context.Background(), pool, c, apps.InstallOptions{ApprovePermissions: true}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	expect(t, agent.do(h, "GET", "/api/v1/connections", ""), 403, "an agent")
	type conn struct {
		App     string
		Hosts   []string
		Secrets []struct {
			Key, Label string
			Set        bool
		}
	}
	list := decodeBody[struct{ Items []conn }](t, admin.do(h, "GET", "/api/v1/connections", ""))
	if len(list.Items) != 1 || list.Items[0].App != "hooks" || strings.Join(list.Items[0].Hosts, ",") != "api.example.com" ||
		len(list.Items[0].Secrets) != 1 || list.Items[0].Secrets[0].Set {
		t.Fatalf("connections: %+v", list.Items)
	}
	expect(t, admin.do(h, "PUT", "/api/v1/connections/hooks/secrets/token", `{"value":"abc123"}`), 204, "setting it")
	expect(t, admin.do(h, "PUT", "/api/v1/connections/hooks/secrets/other", `{"value":"x"}`), 404, "an undeclared secret")
	body := admin.do(h, "GET", "/api/v1/connections", "").Body.String()
	if !strings.Contains(body, `"set":true`) || strings.Contains(body, "abc123") {
		t.Errorf("after setting: %s", body)
	}
	if err := db.WithSystem(context.Background(), pool, func(tx pgx.Tx) error {
		v, err := secrets.Get(context.Background(), dbgen.New(tx), keys, apps.AppSecretName("hooks", "token"))
		if v != "abc123" {
			t.Errorf("stored %q", v)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	expect(t, admin.do(h, "PUT", "/api/v1/connections/hooks/secrets/token", `{"value":null}`), 204, "clearing it")
	if !strings.Contains(admin.do(h, "GET", "/api/v1/connections", "").Body.String(), `"set":false`) {
		t.Error("not cleared")
	}
}
