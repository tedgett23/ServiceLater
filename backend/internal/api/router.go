// Package api holds the Chi router, middleware and HTTP handlers.
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func NewRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	r.Route("/api/v1", func(r chi.Router) {
		// TODO: JWT -> RBAC -> rate limit middleware, then tickets, cmdb, workflows, catalog routes (§7.3)
	})
	return r
}
