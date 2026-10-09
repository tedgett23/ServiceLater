package apps

import (
	"encoding/json"
	"strings"
	"testing"
)

// The catalog editor saves an item with CatalogItemYAML: every shipped item written back must read as the same one.
func TestCatalogItemsWriteBackAsTheyRead(t *testing.T) {
	b, err := LoadBundled("service-catalog")
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Compile()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range c.Definitions {
		if d.CatalogItem == nil {
			continue
		}
		n++
		data, err := CatalogItemYAML(d.CatalogItem, false)
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		var p Problems
		again := parseCatalogItem(d.Path, data, &p)
		if again == nil {
			t.Fatalf("%s does not read back: %v\n%s", d.Name, p, data)
		}
		want, _ := json.Marshal(d.CatalogItem)
		got, _ := json.Marshal(again)
		if string(want) != string(got) {
			t.Errorf("%s changed on the way back:\nwant %s\n got %s", d.Name, want, got)
		}
	}
	if n < 4 {
		t.Fatalf("only %d shipped items", n)
	}
}

// An item edited in the UI is kept as only what changed, so the app's later changes to the rest arrive; a change the
// smallest override cannot say (reordering questions is a list) is still exact.
func TestAnItemEditKeepsTheAppsLaterChanges(t *testing.T) {
	v1 := "kind: catalog_item\nname: mouse\nlabel: Mouse\ncategory: Hardware\nsummary: A mouse.\nfields:\n" +
		"  colour: {type: choice, label: Colour, options: [{value: black, label: Black}, {value: white, label: White}]}\n" +
		"  wireless: {type: boolean, label: Wireless}\n"
	var p Problems
	it := parseCatalogItem("i", []byte(v1), &p)
	if it == nil {
		t.Fatal(p)
	}
	it.Label = "Computer mouse"
	it.AvailableTo = &Audience{Roles: []string{"agent"}}
	over, err := CatalogItemOverride([]byte(v1), it)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(over); strings.Contains(s, "summary") || strings.Contains(s, "Colour") || !strings.Contains(s, "Computer mouse") ||
		strings.Contains(s, "override") {
		t.Errorf("not the smallest override:\n%s", s)
	}
	// The app's next version rewords the summary; the edit still applies over it.
	v2 := strings.Replace(v1, "A mouse.", "A mouse, wired or not.", 1)
	merged, err := mergeYAML([]byte(v2), over)
	if err != nil {
		t.Fatal(err)
	}
	got := parseCatalogItem("i", merged, &p)
	if got == nil || got.Label != "Computer mouse" || got.Summary != "A mouse, wired or not." || got.AvailableTo == nil {
		t.Fatalf("merged: %+v\n%s", got, merged)
	}

	// Reordering the questions changes the layout list, which an override replaces whole: still exact.
	it.Form.Create = []string{"wireless", "colour"}
	over, err = CatalogItemOverride([]byte(v1), it)
	if err != nil {
		t.Fatal(err)
	}
	merged, _ = mergeYAML([]byte(v1), over)
	if again := parseCatalogItem("i", merged, &p); again == nil || strings.Join(again.Form.Create, ",") != "wireless,colour" {
		t.Errorf("reordered: %s", merged)
	}
	if none, _ := CatalogItemOverride(merged, again(t, merged)); none != nil {
		t.Errorf("an unchanged item still overrides: %s", none)
	}
}

func again(t *testing.T, data []byte) *CatalogItem {
	t.Helper()
	var p Problems
	it := parseCatalogItem("i", data, &p)
	if it == nil {
		t.Fatal(p)
	}
	return it
}
