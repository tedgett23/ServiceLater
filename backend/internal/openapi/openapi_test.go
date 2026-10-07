package openapi

import "testing"

func TestHumanize(t *testing.T) {
	for in, want := range map[string]string{
		"listMyNotifications": "List my notifications",
		"getOpenAPI":          "Get open API",
		"me":                  "Me",
		"decideApproval":      "Decide approval",
		"getHTTPStatus":       "Get HTTP status",
	} {
		if got := humanize(in); got != want {
			t.Errorf("humanize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDescribeDropsTheRouteLine(t *testing.T) {
	for in, want := range map[string]string{
		"GET /tickets/{id}/comments. Internal notes are hidden.":   "Internal notes are hidden.",
		"GET /workflow-runs/{id} (staff)":                          "",
		"POST /groups (admin) with {\"name\": \"...\"}.":           "With {\"name\": \"...\"}.",
		"Something else entirely.":                                 "Something else entirely.",
		"POST /x: does a thing,\nover two lines.\n\nAnd a second.": "Does a thing, over two lines.\n\nAnd a second.",
	} {
		method := "GET"
		if len(in) > 4 && in[:4] == "POST" {
			method = "POST"
		}
		if got := describe(method, in); got != want {
			t.Errorf("describe(%q) = %q, want %q", in, got, want)
		}
	}
}
