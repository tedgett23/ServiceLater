// Package api holds the Chi router, middleware and HTTP handlers.
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/realtime"
	"github.com/servicelater/servicelater/backend/internal/records"
)

type Deps struct {
	Pool   *pgxpool.Pool
	Auth   *auth.Service
	Tokens *auth.Tokens
	Hub    *realtime.Hub  // optional: without it there is no /ws endpoint
	Apps   *apps.Registry // optional: without it every record type uses the built-in lifecycle
}

type handlers struct {
	pool  *pgxpool.Pool
	perms *auth.PermissionCache
	auth  *auth.Service
	apps  *apps.Registry
	svc   *records.Service
}

// lifecycleFor is the status flow for a ticket type: the type's own, else the `task` default, else the built-in one.
func (h *handlers) lifecycleFor(recordType string) *apps.Lifecycle {
	return h.apps.Lifecycle(recordType)
}

// knownStatus reports whether any record type's lifecycle has this status, for validating a queue filter that
// spans types.
func (h *handlers) knownStatus(status string) bool {
	for _, t := range apps.KnownRecordTypes {
		if h.apps.Lifecycle(t).HasState(status) {
			return true
		}
	}
	return false
}

func NewRouter(d Deps) http.Handler {
	if d.Apps == nil {
		d.Apps = apps.NewRegistry(nil)
	}
	h := &handlers{pool: d.Pool, perms: auth.NewPermissionCache(d.Pool), auth: d.Auth, apps: d.Apps, svc: &records.Service{Apps: d.Apps, Pool: d.Pool}}
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(sameOrigin)
		r.Get("/openapi.json", h.getOpenAPI)

		r.Route("/auth", func(r chi.Router) {
			r.Post("/login", h.login)
			r.Post("/refresh", h.refresh)
			r.Post("/logout", h.logout)
			r.With(d.Tokens.Authenticate).Get("/me", h.me)
		})

		r.Group(func(r chi.Router) {
			r.Use(d.Tokens.Authenticate, h.perms.Load)

			r.Route("/users", func(r chi.Router) {
				r.With(auth.RequirePermission(authz.UsersRead)).Get("/", h.listUsers)
				r.With(auth.RequirePermission(authz.UsersRead)).Get("/staff", h.listStaff)
				r.With(auth.RequirePermission(authz.UsersManage)).Post("/", h.createUser)
				r.Route("/{id}", func(r chi.Router) {
					r.Use(auth.RequirePermission(authz.UsersManage))
					r.Get("/", h.getUser)
					r.Patch("/", h.updateUser)
					r.Delete("/", h.deleteUser)
					r.Put("/password", h.setUserPassword)
				})
			})
			r.Get("/permissions", h.listPermissions)
			r.Route("/roles", func(r chi.Router) {
				r.With(auth.RequirePermission(authz.RolesManage, authz.UsersManage, authz.GroupsManage)).Get("/", h.listRoles)
				r.With(auth.RequirePermission(authz.RolesManage, authz.UsersManage, authz.GroupsManage)).Get("/{name}", h.getRole)
				r.With(auth.RequirePermission(authz.RolesManage)).Post("/{name}/grants", h.grantRole)
				r.With(auth.RequirePermission(authz.RolesManage)).Delete("/{name}/grants/{id}", h.revokeRole)
			})
			r.Route("/groups", func(r chi.Router) {
				r.Get("/", h.listGroups)
				r.With(auth.RequirePermission(authz.GroupsManage)).Post("/", h.createGroup)
				r.Route("/{id}", func(r chi.Router) {
					r.Get("/", h.getGroup)
					r.With(auth.RequirePermission(authz.GroupsManage)).Patch("/", h.updateGroup)
					r.With(auth.RequirePermission(authz.GroupsManage)).Delete("/", h.deleteGroup)
					r.Get("/members", h.listGroupMembers)
					r.With(auth.RequirePermission(authz.GroupsManage)).Put("/members/{userId}", h.addGroupMember)
					r.With(auth.RequirePermission(authz.GroupsManage)).Delete("/members/{userId}", h.removeGroupMember)
				})
			})
			r.Get("/lifecycles/{recordType}", h.getLifecycle)
			r.Get("/record-types", h.listRecordTypes)
			r.Get("/calendar", h.listCalendar)
			r.Route("/freeze-windows", func(r chi.Router) {
				r.Get("/", h.listFreezeWindows)
				r.With(auth.RequirePermission(authz.FreezesManage)).Post("/", h.createFreezeWindow)
				r.Get("/{id}", h.getFreezeWindow)
				r.With(auth.RequirePermission(authz.FreezesManage)).Patch("/{id}", h.updateFreezeWindow)
				r.With(auth.RequirePermission(authz.FreezesManage)).Delete("/{id}", h.deleteFreezeWindow)
			})
			r.Route("/business-calendars", func(r chi.Router) {
				r.Get("/", h.listBusinessCalendars)
				r.With(auth.RequirePermission(authz.CalendarsManage)).Post("/", h.createBusinessCalendar)
				r.Get("/{id}", h.getBusinessCalendar)
				r.With(auth.RequirePermission(authz.CalendarsManage)).Patch("/{id}", h.updateBusinessCalendar)
				r.With(auth.RequirePermission(authz.CalendarsManage)).Delete("/{id}", h.deleteBusinessCalendar)
			})

			r.Route("/tickets", func(r chi.Router) {
				r.Get("/", h.listTickets)
				r.Post("/", h.createTicket)
				r.Route("/{id}", func(r chi.Router) {
					r.Get("/", h.getTicket)
					r.Get("/comments", h.listComments)
					r.Post("/comments", h.createComment)
					r.With(auth.RequireRecordPermission(authz.Update)).Patch("/", h.updateTicket)
					r.With(auth.RequireRecordPermission(authz.Update)).Post("/transitions", h.transitionTicket)
				})
			})
			r.Route("/approvals", func(r chi.Router) {
				r.Get("/", h.listMyApprovals)
				r.Post("/{id}/decide", h.decideApproval)
			})
			r.Route("/notifications", func(r chi.Router) {
				r.Get("/", h.listMyNotifications)
				r.Post("/read", h.markTaskNotificationsRead)
				r.Post("/{id}/read", h.markNotificationRead)
			})
			r.Route("/workflow-runs", func(r chi.Router) {
				r.Use(auth.RequirePermission(authz.WorkflowsReadAll))
				r.Get("/", h.listRuns)
				r.With(auth.RequirePermission(authz.WorkflowsRun)).Post("/", h.startRun)
				r.Get("/{id}", h.getRun)
				r.With(auth.RequirePermission(authz.WorkflowsRun)).Post("/{id}/cancel", h.cancelRun)
			})
			r.With(auth.RequirePermission(authz.WorkflowsReadAll)).Get("/workflows", h.listWorkflows)
			// TODO: cmdb, catalog routes (§7.3)
		})
	})

	if d.Hub != nil {
		r.With(d.Tokens.Authenticate, h.perms.Load).Get("/ws/v1/subscribe", func(w http.ResponseWriter, req *http.Request) {
			p, _ := auth.PrincipalFrom(req.Context())
			d.Hub.Serve(w, req, p)
		})
	}
	return r
}
