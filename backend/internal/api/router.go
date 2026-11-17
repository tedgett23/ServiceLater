// Package api holds the Chi router, middleware and HTTP handlers.
package api

import (
	"crypto/rand"
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
	"github.com/servicelater/servicelater/backend/internal/storage"
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
	// Files are where attachments are kept (optional: without them files cannot be attached).
	Files *storage.Manager
	// Signer signs the links parts are uploaded to and files downloaded from when they go through the server. Every
	// api server must have the same key (storage.NewSigner with the token secret); left out, links work only on this
	// server.
	Signer *storage.Signer
}

type handlers struct {
	pool   *pgxpool.Pool
	perms  *auth.PermissionCache
	auth   *auth.Service
	apps   *apps.Registry
	svc    *records.Service
	keys   *secrets.Keyring
	sender notify.Sender
	files  *storage.Manager
	signer *storage.Signer
}

// lifecycleFor is the status flow for a ticket type: the type's own, else the `task` default, else the built-in one.
func (h *handlers) lifecycleFor(recordType string) *apps.Lifecycle {
	return h.apps.Lifecycle(recordType)
}

// knownStatus reports whether any record type's lifecycle has this status, for validating a queue filter that
// spans types.
func (h *handlers) knownStatus(status string) bool {
	for _, t := range append(h.apps.RecordTypes(), "task") {
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
	if d.Files == nil {
		d.Files = &storage.Manager{Pool: d.Pool, Keys: d.Keys}
	}
	if d.Signer == nil {
		d.Signer = storage.NewSigner(rand.Text())
	}
	h := &handlers{pool: d.Pool, perms: auth.NewPermissionCache(d.Pool), auth: d.Auth, apps: d.Apps, svc: &records.Service{Apps: d.Apps, Pool: d.Pool},
		keys: d.Keys, sender: d.Sender, files: d.Files, signer: d.Signer}
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(sameOrigin)
		r.Get("/openapi.json", h.getOpenAPI)
		// Links handed out by /attachments/{id}/parts and /download: they carry a signed token, not a session.
		r.Put("/attachments/{id}/parts/{n}", h.putAttachmentPart)
		r.Get("/attachments/{id}/file", h.serveAttachment)

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
					r.Post("/attachments", h.createAttachment)
				})
			})
			r.Route("/attachments/{id}", func(r chi.Router) {
				r.Get("/download", h.downloadAttachment)
				r.Post("/parts", h.attachmentPartURLs)
				r.Post("/complete", h.completeAttachment)
				r.Delete("/", h.deleteAttachment)
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
			r.Route("/settings/storage", func(r chi.Router) {
				r.Use(auth.RequirePermission(authz.SettingsManage))
				r.Get("/", h.getStorageSettings)
				r.Put("/", h.putStorageSettings)
				r.Post("/test", h.testStorage)
			})
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
			r.Route("/cmdb/csv", func(r chi.Router) {
				r.Use(auth.RequirePermission(authz.CMDBRead))
				r.Get("/items/{class}", h.exportCIs)
				r.With(auth.RequirePermission(authz.CMDBManage)).Post("/items/{class}", h.importCIs)
				r.Get("/relationships", h.exportCIRelationships)
				r.With(auth.RequirePermission(authz.CMDBManage)).Post("/relationships", h.importCIRelationships)
			})
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
			r.Route("/catalog-items", func(r chi.Router) {
				r.Use(auth.RequirePermission(authz.CatalogManage))
				r.Get("/", h.listCatalogItems)
				r.Get("/{name}", h.getCatalogItemDef)
				r.Put("/{name}", h.putCatalogItemDef)
				r.Delete("/{name}/edits", h.resetCatalogItemDef)
			})
			r.Route("/catalog", func(r chi.Router) {
				r.Get("/", h.listCatalog)
				r.Get("/{name}", h.getCatalogItem)
				r.Post("/{name}/states", h.catalogStates)
				r.Post("/{name}/request", h.orderCatalogItem)
			})
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
