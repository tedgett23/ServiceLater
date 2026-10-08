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
	"github.com/servicelater/servicelater/backend/internal/notify"
	"github.com/servicelater/servicelater/backend/internal/realtime"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/secrets"
)

type Deps struct {
	Pool   *pgxpool.Pool
	Auth   *auth.Service
	Tokens *auth.Tokens
	Hub    *realtime.Hub  // optional: without it there is no /ws endpoint
	Apps   *apps.Registry // optional: without it every record type uses the built-in lifecycle
	// Keys encrypt and decrypt secrets (optional: without them secrets can be neither set nor used).
	Keys *secrets.Keyring
	// Sender sends test emails; the zero value is the real one.
	Sender notify.Sender
}

type handlers struct {
	pool   *pgxpool.Pool
	perms  *auth.PermissionCache
	auth   *auth.Service
	apps   *apps.Registry
	svc    *records.Service
	keys   *secrets.Keyring
	sender notify.Sender
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
	h := &handlers{pool: d.Pool, perms: auth.NewPermissionCache(d.Pool), auth: d.Auth, apps: d.Apps, svc: &records.Service{Apps: d.Apps, Pool: d.Pool},
		keys: d.Keys, sender: d.Sender}
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
			r.Get("/forms/{type}", h.getForm)
			r.Post("/forms/{type}/states", h.formStates)
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
					r.Get("/history", h.getTicketHistory)
					r.Post("/comments", h.createComment)
					r.With(auth.RequireRecordPermission(authz.Update)).Patch("/", h.updateTicket)
					r.With(auth.RequireRecordPermission(authz.Update)).Post("/transitions", h.transitionTicket)
					r.With(auth.RequirePermission(authz.CMDBRead)).Get("/impact", h.getTicketImpact)
					r.With(auth.RequirePermission(authz.CMDBRead), auth.RequireRecordPermission(authz.Update)).Put("/cis/{ciId}", h.linkTicketCI)
					r.With(auth.RequirePermission(authz.CMDBRead), auth.RequireRecordPermission(authz.Update)).Delete("/cis/{ciId}", h.unlinkTicketCI)
					r.With(auth.RequireRecordPermission(authz.Update)).Put("/articles/{articleId}", h.linkTicketArticle)
					r.With(auth.RequireRecordPermission(authz.Update)).Delete("/articles/{articleId}", h.unlinkTicketArticle)
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
			r.Route("/workflows", func(r chi.Router) {
				r.With(auth.RequirePermission(authz.WorkflowsReadAll, authz.WorkflowsEdit)).Get("/", h.listWorkflows)
				r.With(auth.RequirePermission(authz.WorkflowsReadAll, authz.WorkflowsEdit)).Get("/{app}/{name}", h.getWorkflowDef)
				r.With(auth.RequirePermission(authz.WorkflowsReadAll, authz.WorkflowsEdit)).Get("/{app}/{name}/activity", h.getWorkflowActivity)
				r.With(auth.RequirePermission(authz.WorkflowsEdit)).Put("/{app}/{name}", h.putWorkflowDef)
				r.With(auth.RequirePermission(authz.WorkflowsEdit)).Delete("/{app}/{name}/edits", h.resetWorkflowDef)
			})
			r.Route("/kb", func(r chi.Router) {
				r.Get("/suggest", h.suggestArticles)
				r.Get("/categories", h.listArticleCategories)
				r.Get("/articles", h.listArticles)
				r.With(auth.RequirePermission(authz.KnowledgeWrite, authz.KnowledgePublish)).Post("/articles", h.createArticle)
				r.Route("/articles/{id}", func(r chi.Router) {
					r.Get("/", h.getArticle)
					r.With(auth.RequirePermission(authz.KnowledgeWrite, authz.KnowledgePublish)).Patch("/", h.updateArticle)
					r.With(auth.RequirePermission(authz.KnowledgePublish)).Delete("/", h.deleteArticle)
					r.With(auth.RequirePermission(authz.KnowledgeWrite, authz.KnowledgePublish)).Post("/status", h.setArticleStatus)
					r.Get("/versions", h.listArticleVersions)
					r.Get("/versions/{version}", h.getArticleVersion)
				})
			})
			r.With(auth.RequirePermission(authz.SettingsManage)).Get("/settings/history", h.getHistorySettings)
			r.With(auth.RequirePermission(authz.SettingsManage)).Put("/settings/history", h.putHistorySettings)
			r.Route("/settings/email", func(r chi.Router) {
				r.Use(auth.RequirePermission(authz.SettingsManage))
				r.Get("/", h.getEmailSettings)
				r.Put("/", h.putEmailSettings)
				r.Post("/test", h.testEmail)
				r.Get("/deliveries", h.listEmailDeliveries)
			})
			r.Get("/account", h.getAccount)
			r.Patch("/account", h.patchAccount)
			r.With(auth.RequirePermission(authz.CMDBRead)).Get("/cmdb/model", h.getCMDBModel)
			r.Route("/cis", func(r chi.Router) {
				r.Use(auth.RequirePermission(authz.CMDBRead))
				r.Get("/", h.listCIs)
				r.With(auth.RequirePermission(authz.CMDBManage)).Post("/", h.createCI)
				r.Route("/{id}", func(r chi.Router) {
					r.Get("/", h.getCI)
					r.With(auth.RequirePermission(authz.CMDBManage)).Patch("/", h.updateCI)
					r.With(auth.RequirePermission(authz.CMDBManage)).Delete("/", h.deleteCI)
					r.Get("/impact", h.getCIImpact)
					r.Get("/history", h.getCIHistory)
					r.With(auth.RequirePermission(authz.CMDBManage)).Post("/relationships", h.createCIRelationship)
					r.With(auth.RequirePermission(authz.CMDBManage)).Delete("/relationships/{relId}", h.deleteCIRelationship)
				})
			})
			// TODO: catalog routes (§7.3)
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
