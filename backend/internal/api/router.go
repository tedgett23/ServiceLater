// Package api holds the Chi router, middleware and HTTP handlers.
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/realtime"
)

type Deps struct {
	Pool   *pgxpool.Pool
	Auth   *auth.Service
	Tokens *auth.Tokens
	Hub    *realtime.Hub // optional: without it there is no /ws endpoint
}

type handlers struct {
	pool *pgxpool.Pool
	auth *auth.Service
}

func NewRouter(d Deps) http.Handler {
	h := &handlers{pool: d.Pool, auth: d.Auth}
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(sameOrigin)

		r.Route("/auth", func(r chi.Router) {
			r.Post("/login", h.login)
			r.Post("/refresh", h.refresh)
			r.Post("/logout", h.logout)
			r.With(d.Tokens.Authenticate).Get("/me", h.me)
		})

		r.Group(func(r chi.Router) {
			r.Use(d.Tokens.Authenticate)

			r.With(auth.RequireRole("admin", "agent")).Get("/users/staff", h.listStaff)

			r.Route("/tickets", func(r chi.Router) {
				r.Get("/", h.listTickets)
				r.Post("/", h.createTicket)
				r.Route("/{id}", func(r chi.Router) {
					r.Get("/", h.getTicket)
					r.Get("/comments", h.listComments)
					r.Post("/comments", h.createComment)
					r.With(auth.RequireRole("admin", "agent")).Patch("/", h.updateTicket)
					r.With(auth.RequireRole("admin", "agent")).Post("/transitions", h.transitionTicket)
				})
			})
			// TODO: cmdb, workflows, catalog routes (§7.3)
		})
	})

	if d.Hub != nil {
		r.With(d.Tokens.Authenticate).Get("/ws/v1/subscribe", func(w http.ResponseWriter, req *http.Request) {
			p, _ := auth.PrincipalFrom(req.Context())
			d.Hub.Serve(w, req, p)
		})
	}
	return r
}
