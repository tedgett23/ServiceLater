package api

import (
	_ "embed"
	"net/http"
)

// openAPISpec is generated from this package's source by `make openapi` (internal/openapi); never edit it by hand.
// TestOpenAPISpecIsCurrent fails when it is out of date.
//
//go:embed openapi.json
var openAPISpec []byte

// GET /openapi.json: this document. It describes the API, not anyone's data, so it needs no sign-in.
func (h *handlers) getOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPISpec)
}
