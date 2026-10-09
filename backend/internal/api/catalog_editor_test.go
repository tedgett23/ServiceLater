package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type itemDef struct {
	Item      map[string]any `json:"item"`
	App       string         `json:"app"`
	Edited    bool           `json:"edited"`
	Shipped   bool           `json:"shipped"`
	Override  string         `json:"override"`
	YAML      string         `json:"yaml"`
	Workflows []string       `json:"workflows"`
}

// The catalog editor changes items the way the workflow builder changes workflows: an edit is kept as only what it
// changes, in the app's studio layer, and is live at once; a new item joins the catalog; resetting undoes either.
func TestTheCatalogEditor(t *testing.T) {
	h, pool, reg := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	cara := newActor(t, h, pool, "cara@x.test", "end_user")
	reload := func() {
		t.Helper()
		if err := reg.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	expect(t, agent.do(h, "GET", "/api/v1/catalog-items", ""), 403, "an agent opening the editor")
	customRole(t, h, pool, admin, "catalog-keeper", cara, "catalog:manage")

	list := decodeBody[struct {
		Items []struct {
			Name    string `json:"name"`
			App     string `json:"app"`
			Shipped bool   `json:"shipped"`
		} `json:"items"`
		Apps []struct {
			Name      string   `json:"name"`
			Workflows []string `json:"workflows"`
		} `json:"apps"`
	}](t, cara.do(h, "GET", "/api/v1/catalog-items", ""))
	if len(list.Items) != 4 || !list.Items[0].Shipped {
		t.Errorf("items: %+v", list.Items)
	}
	found := false
	for _, a := range list.Apps {
		if a.Name == "service-catalog" && strings.Join(a.Workflows, ",") == "laptop,new-starter,software-access" {
			found = true
		}
	}
	if !found {
		t.Errorf("apps: %+v", list.Apps)
	}

	// Rename the laptop and add a question: only that is kept, and it is what people see at once.
	def := decodeBody[itemDef](t, cara.do(h, "GET", "/api/v1/catalog-items/new-laptop", ""))
	if def.App != "service-catalog" || def.Edited || !strings.Contains(def.YAML, "kind: catalog_item") {
		t.Fatalf("definition: %+v", def)
	}
	item := def.Item
	item["label"] = "Laptop"
	form := item["form"].(map[string]any)
	form["fields"] = append(form["fields"].([]any), map[string]any{"name": "dock", "type": "boolean", "label": "I need a dock too"})
	form["create"] = append(form["create"].([]any), "dock")
	body, _ := json.Marshal(map[string]any{"item": item})
	w := cara.do(h, "PUT", "/api/v1/catalog-items/new-laptop", string(body))
	expect(t, w, 200, "saving an edit")
	saved := decodeBody[itemDef](t, w)
	if !saved.Edited || !saved.Shipped || !strings.Contains(saved.Override, "label: Laptop") || !strings.Contains(saved.Override, "dock") ||
		strings.Contains(saved.Override, "summary") {
		t.Errorf("the override is not just the edit:\n%s", saved.Override)
	}
	reload()
	if got := cara.do(h, "GET", "/api/v1/catalog/new-laptop", "").Body.String(); !strings.Contains(got, `"label":"Laptop"`) || !strings.Contains(got, "I need a dock too") {
		t.Errorf("not live: %s", got)
	}

	// An edit that does not compile changes nothing and says why.
	form["fields"] = append(form["fields"].([]any), map[string]any{"name": "colour", "type": "text", "label": "Colour", "visible_when": "record.fields.nope"})
	form["create"] = append(form["create"].([]any), "colour")
	body, _ = json.Marshal(map[string]any{"item": item})
	w = cara.do(h, "PUT", "/api/v1/catalog-items/new-laptop", string(body))
	if w.Code != 422 || !strings.Contains(w.Body.String(), "problems") || !strings.Contains(w.Body.String(), "nope") {
		t.Errorf("a broken edit: %d %s", w.Code, w.Body)
	}

	// A new item, added to an app.
	lamp := `{"app":"service-catalog","item":{"label":"Desk lamp","category":"Office","icon":"box","summary":"A lamp for your desk.",
		"form":{"fields":[{"name":"where","type":"text","label":"Which desk?","required":true}],"create":["where"],"detail":[{"fields":["where"]}]}}}`
	expect(t, cara.do(h, "PUT", "/api/v1/catalog-items/desk-lamp", lamp), 200, "adding an item")
	reload()
	expect(t, cara.do(h, "POST", "/api/v1/catalog/desk-lamp/request", `{"fields":{"where":"3.14"}}`), 201, "ordering the new item")
	refusal(t, cara.do(h, "PUT", "/api/v1/catalog-items/desk-light", strings.Replace(lamp, `"app":"service-catalog",`, "", 1)), 422,
		"app is required", "a new item with no app")

	// Resetting: the laptop goes back to the app's version; the lamp leaves the catalog.
	back := decodeBody[itemDef](t, cara.do(h, "DELETE", "/api/v1/catalog-items/new-laptop/edits", ""))
	if back.Edited || back.Item["label"] != "New laptop" {
		t.Errorf("reset: %+v", back)
	}
	expect(t, cara.do(h, "DELETE", "/api/v1/catalog-items/desk-lamp/edits", ""), 204, "removing the new item")
	reload()
	expect(t, cara.do(h, "GET", "/api/v1/catalog/desk-lamp", ""), 404, "the removed item")
	expect(t, cara.do(h, "DELETE", "/api/v1/catalog-items/new-laptop/edits", ""), 404, "resetting what has no edits")
}
