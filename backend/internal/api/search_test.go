package api_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/servicelater/servicelater/backend/internal/apps"
)

type searchItem struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Title     string `json:"title"`
	Detail    string `json:"detail"`
	Link      string `json:"link"`
}

type searchResult struct {
	Mode   string       `json:"mode"`
	Prefix string       `json:"prefix"`
	Items  []searchItem `json:"items"`
}

func searchFor(t *testing.T, h http.Handler, a actor, q string) searchResult {
	t.Helper()
	w := a.do(h, "GET", "/api/v1/search?q="+url.QueryEscape(q), "")
	expect(t, w, 200, "searching for "+q)
	return decodeBody[searchResult](t, w)
}

func refs(items []searchItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Reference
	}
	return out
}

func sameRefs(got []searchItem, want ...string) bool { return slices.Equal(refs(got), want) }

// A query that starts with a record type's prefix is read as a reference first: the prefix alone lists the newest of
// the type, digits narrow it to numbers that start with them (exact first), in any case and with leading zeros.
func TestSearchReadsAReferenceFirst(t *testing.T) {
	h, pool, _ := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	for i := 0; i < 12; i++ {
		expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Incident number `+string(rune('a'+i))+`"}`), 201, "filing")
	}
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"change","title":"Patch the incubator"}`), 201, "filing a change")

	got := searchFor(t, h, agent, "inc")
	if got.Mode != "reference" || got.Prefix != "INC" || len(got.Items) != 8 || got.Items[0].Reference != "INC12" {
		t.Fatalf("a prefix alone lists the newest of the type: %+v", got)
	}
	if it := got.Items[0]; it.Kind != "record" || it.Link != "/tickets/INC12" || it.Detail != "Incident · New" {
		t.Errorf("a result is ready to show and open: %+v", it)
	}
	if got := searchFor(t, h, agent, "INC1"); !sameRefs(got.Items, "INC1", "INC10", "INC11", "INC12") {
		t.Errorf("INC1: the exact record, then the numbers that start with 1: %v", refs(got.Items))
	}
	for _, q := range []string{"inc12", "INC 12", "inc-12", "INC0012"} {
		if got := searchFor(t, h, agent, q); !sameRefs(got.Items, "INC12") {
			t.Errorf("%q finds INC12, got %v", q, refs(got.Items))
		}
	}
	if got := searchFor(t, h, agent, "CHG1"); !sameRefs(got.Items, "CHG1") || got.Items[0].Detail != "Change · Draft" {
		t.Errorf("each type has its own numbers: %+v", got.Items)
	}
	// A reference that finds nothing is searched as text instead: here it matches a title.
	if got := searchFor(t, h, agent, "INC99"); got.Mode != "text" || len(got.Items) != 0 {
		t.Errorf("no such number and no such text: %+v", got)
	}
	if got := searchFor(t, h, agent, "incub"); got.Mode != "text" || !sameRefs(got.Items, "CHG1") {
		t.Errorf("letters that are not a prefix are text: %+v", got)
	}

	expect(t, agent.do(h, "GET", "/api/v1/search?q=in", ""), 400, "two characters")
	expect(t, agent.do(h, "GET", "/api/v1/search", ""), 400, "nothing typed")
}

// Text is matched against titles that contain it, then against titles that look like it, so a typo or part of a
// word still finds the record. Articles and items are found too, and people by those who manage them.
func TestSearchFallsBackToFuzzyText(t *testing.T) {
	h, pool, _ := setupApps(t)
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	newActor(t, h, pool, "printz@x.test", "end_user")
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Shared printer on floor 3 jams on duplex"}`), 201, "filing")
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"Sprint board will not load"}`), 201, "filing")
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"100% of the disk is used"}`), 201, "filing")
	expect(t, agent.do(h, "POST", "/api/v1/kb/articles", `{"title":"Clearing a printer jam","body":"Open the rear tray and pull the sheet out."}`), 201, "writing an article")
	expect(t, admin.do(h, "POST", "/api/v1/cis", `{"class":"server","name":"print-server-01"}`), 201, "adding an item")

	// The record whose title has the word comes first, then the article; the sprint is not a printer. (The item
	// print-server-01 looks like it too, and comes after.)
	got := searchFor(t, h, agent, "printer")
	if got.Mode != "text" || len(got.Items) != 3 || got.Items[0].Reference != "INC1" || got.Items[1].Kind != "article" ||
		got.Items[1].Detail != "Article · draft" || got.Items[1].Link != "/kb/"+got.Items[1].Reference || got.Items[2].Kind != "item" {
		t.Fatalf("a word in a title: %+v", got.Items)
	}
	if got := searchFor(t, h, agent, "printr jam"); len(got.Items) == 0 || got.Items[0].Reference != "INC1" && got.Items[0].Kind != "article" {
		t.Errorf("a typo still finds it: %+v", got.Items)
	}
	if got := searchFor(t, h, agent, "print-server"); len(got.Items) != 1 || got.Items[0].Kind != "item" || got.Items[0].Detail != "Server" {
		t.Errorf("an item by its name, with its class: %+v", got.Items)
	}
	if got := searchFor(t, h, agent, "rear tray"); len(got.Items) != 1 || got.Items[0].Kind != "article" {
		t.Errorf("an article by its text: %+v", got.Items)
	}
	// % and _ are what was typed, not wildcards.
	if got := searchFor(t, h, agent, "100%"); !sameRefs(got.Items, "INC3") {
		t.Errorf("a percent sign is searched for as itself: %v", refs(got.Items))
	}
	if got := searchFor(t, h, agent, "___"); len(got.Items) != 0 {
		t.Errorf("underscores match nothing here: %v", refs(got.Items))
	}

	// People are found only by those who manage them.
	people := func(a actor) int {
		n := 0
		for _, it := range searchFor(t, h, a, "printz").Items {
			if it.Kind == "person" {
				n++
			}
		}
		return n
	}
	if people(admin) != 1 || people(agent) != 0 {
		t.Errorf("people: admin %d, agent %d", people(admin), people(agent))
	}
}

// Search runs as the person searching: an end user finds their own records and published articles, never anyone
// else's ticket, an item, or a draft. A retired type's records are not found at all.
func TestSearchFindsOnlyWhatTheReaderMaySee(t *testing.T) {
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	admin := newActor(t, h, pool, "boss@x.test", "admin")
	lena := newActor(t, h, pool, "lena@x.test", "end_user")
	chris := newActor(t, h, pool, "chris@x.test", "end_user")
	expect(t, lena.do(h, "POST", "/api/v1/tickets", `{"title":"Projector in room 4 flickers"}`), 201, "lena filing")
	expect(t, chris.do(h, "POST", "/api/v1/tickets", `{"title":"Projector remote is missing"}`), 201, "chris filing")
	expect(t, agent.do(h, "POST", "/api/v1/kb/articles", `{"title":"Projector troubleshooting draft","body":"Not ready."}`), 201, "a draft article")
	expect(t, admin.do(h, "POST", "/api/v1/cis", `{"class":"server","name":"projector-controller"}`), 201, "adding an item")

	if got := searchFor(t, h, agent, "projector"); len(got.Items) != 4 {
		t.Fatalf("an agent finds both tickets, the draft and the item: %+v", got.Items)
	}
	if got := searchFor(t, h, lena, "projector"); !sameRefs(got.Items, "INC1") {
		t.Errorf("an end user finds only their own ticket: %+v", got.Items)
	}
	if got := searchFor(t, h, lena, "INC"); !sameRefs(got.Items, "INC1") {
		t.Errorf("by reference too: %+v", got.Items)
	}
	if got := searchFor(t, h, lena, "INC2"); len(got.Items) != 0 {
		t.Errorf("someone else's reference finds nothing: %+v", got.Items)
	}

	installTyler(t, pool, reg)
	expect(t, agent.do(h, "POST", "/api/v1/tickets", `{"type":"tyler_thing","title":"Projector stand"}`), 201, "filing a Thing")
	if got := searchFor(t, h, agent, "tyl"); got.Mode != "reference" || !sameRefs(got.Items, "TYL1") {
		t.Errorf("an app's prefix is a prefix: %+v", got)
	}
	if _, err := apps.Uninstall(context.Background(), pool, "tyler"); err != nil {
		t.Fatal(err)
	}
	if got := searchFor(t, h, agent, "TYL1"); len(got.Items) != 0 {
		t.Errorf("a retired type's prefix finds nothing: %+v", got.Items)
	}
	for _, it := range searchFor(t, h, agent, "projector").Items {
		if it.Reference == "TYL1" {
			t.Errorf("a retired type's record is not found by text either: %+v", it)
		}
	}
}
