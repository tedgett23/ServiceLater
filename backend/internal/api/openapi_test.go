package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/servicelater/servicelater/backend/internal/openapi"
)

// The committed document is what the generator makes from the source now. If this fails, run `make openapi`.
func TestOpenAPISpecIsCurrent(t *testing.T) {
	want, err := openapi.Generate(".")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("internal/api/openapi.json is out of date: run `make openapi` and commit the result")
	}
}

// The generator reads the router's source; chi.Walk sees what the router really serves. They must agree, so a route
// registered in a way the generator does not follow cannot go missing from the document.
func TestOpenAPICoversEveryRoute(t *testing.T) {
	h, _ := setup(t)
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	w := do(h, "GET", "/api/v1/openapi.json", "", nil)
	expect(t, w, 200, "the document, signed out")
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	var documented []string
	for p, ops := range doc.Paths {
		for m := range ops {
			documented = append(documented, strings.ToUpper(m)+" "+p)
		}
	}

	var served []string
	wildcard := regexp.MustCompile(`/\*$`)
	err := chi.Walk(h.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = wildcard.ReplaceAllString(route, "")
		if len(route) > 1 {
			route = strings.TrimSuffix(route, "/")
		}
		if strings.HasPrefix(route, "/api/") {
			served = append(served, method+" "+route)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(documented)
	sort.Strings(served)
	if strings.Join(documented, "\n") != strings.Join(served, "\n") {
		t.Errorf("documented:\n%s\n\nserved:\n%s", strings.Join(documented, "\n"), strings.Join(served, "\n"))
	}
}
