// Package api holds the Chi router, middleware and HTTP handlers.
package api

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/authz"
	"github.com/servicelater/servicelater/backend/internal/backup"
	"github.com/servicelater/servicelater/backend/internal/inbound"
	"github.com/servicelater/servicelater/backend/internal/instance"
	"github.com/servicelater/servicelater/backend/internal/notify"
	"github.com/servicelater/servicelater/backend/internal/query"
	"github.com/servicelater/servicelater/backend/internal/ratelimit"
	"github.com/servicelater/servicelater/backend/internal/realtime"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
	"github.com/servicelater/servicelater/backend/internal/sso"
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
	// SSO runs single sign-on. Every api server must share its StateKey (derive it from the token secret); left out,
	// a sign-in must come back to the server it started on.
	SSO *sso.Manager
	// SMS texts messages (tests and verification codes; notifications are sent by the worker). notify.Twilio when left
	// out.
	SMS SMSSender
	// Teams posts to Teams channels (connecting a group posts a test). notify.TeamsPoster when left out.
	Teams TeamsPoster
	// OpenMailbox connects to the incoming mailbox, for the settings' test; inbound.Open when left out.
	OpenMailbox func(ctx context.Context, cfg settings.Inbound, secret string, tlsConfig *tls.Config) (inbound.Mailbox, error)
	// Query runs the SQL people write (optional: without it there are no queries).
	Query *query.Runner
	// Limits counts API requests per caller (optional: without it each router counts on its own, with the default
	// limits, which is enough for tests).
	Limits *ratelimit.Limiter
	// Instance is what this instance is and whether it is in maintenance (optional: tests get one that syncs on
	// GET /instance).
	Instance *instance.Watcher
	// Backups takes, keeps and restores snapshots (optional: tests get one keeping them in a temporary folder).
	Backups *backup.Service
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
	sso    *sso.Manager
	// smsSender and teamsPoster send straight away, for tests and verification codes.
	smsSender   SMSSender
	teamsPoster TeamsPoster
	openMailbox func(ctx context.Context, cfg settings.Inbound, secret string, tlsConfig *tls.Config) (inbound.Mailbox, error)
	query       *query.Runner
	limits      *ratelimit.Limiter
	instance    *instance.Watcher
	tokens      *auth.Tokens
	backups     *backup.Service
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
	if d.SMS == nil {
		d.SMS = notify.Twilio{}
	}
	if d.Teams == nil {
		d.Teams = notify.TeamsPoster{}
	}
	if d.OpenMailbox == nil {
		d.OpenMailbox = inbound.Open
	}
	if d.Limits == nil {
		d.Limits = ratelimit.New(d.Pool)
	}
	d.Instance = instanceWatcher(d)
	if d.Backups == nil {
		d.Backups = &backup.Service{Pool: d.Pool, Keys: d.Keys, Files: d.Files, Dir: filepath.Join(os.TempDir(), "servicelater-snapshots"), Registry: d.Apps}
	}
	if d.SSO == nil {
		d.SSO = &sso.Manager{Pool: d.Pool, Keys: d.Keys, StateKey: []byte(rand.Text())}
	}
	h := &handlers{pool: d.Pool, perms: auth.NewPermissionCache(d.Pool), auth: d.Auth, apps: d.Apps, svc: &records.Service{Apps: d.Apps, Pool: d.Pool},
		keys: d.Keys, sender: d.Sender, files: d.Files, signer: d.Signer, sso: d.SSO,
		smsSender: d.SMS, teamsPoster: d.Teams, openMailbox: d.OpenMailbox, query: d.Query, limits: d.Limits, instance: d.Instance, backups: d.Backups, tokens: d.Tokens}
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, finishWrites)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(sameOrigin, h.maintenance)
		r.Get("/instance", h.getInstance)
		r.Get("/openapi.json", h.getOpenAPI)
		// Links handed out by /attachments/{id}/parts and /download: they carry a signed token, not a session.
		r.Put("/attachments/{id}/parts/{n}", h.putAttachmentPart)
		r.Get("/attachments/{id}/file", h.serveAttachment)
		// Webhooks that start workflows: the caller proves itself with the webhook's secret, not a session.
		r.Post("/hooks/{app}/{workflow}", h.receiveWebhook)

		r.Route("/auth", func(r chi.Router) {
			r.Post("/login", h.login)
			r.Get("/sso", h.signInOptions)
			r.Get("/sso/{id}/start", h.ssoStart)
			r.Get("/sso/{id}/callback", h.ssoCallback)
			r.Post("/refresh", h.refresh)
			r.Post("/logout", h.logout)
			r.With(d.Tokens.Authenticate).Get("/me", h.me)
		})

		r.Group(func(r chi.Router) {
			r.Use(d.Tokens.Authenticate, h.perms.Load, h.rateLimit)

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
					r.Get("/api-keys", h.listAPIKeys)
					r.Post("/api-keys", h.createAPIKey)
					r.Delete("/api-keys/{keyId}", h.revokeAPIKey)
					r.Patch("/api-keys/{keyId}", h.updateAPIKey)
				})
			})
			r.With(auth.RequirePermission(authz.UsersManage)).Post("/service-accounts", h.createServiceAccount)
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
			r.Get("/search", h.search)
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
				r.Get("/counts", h.countTickets)
				r.Get("/suggest", h.suggestTickets)
				r.Route("/{id}", func(r chi.Router) {
					r.Get("/", h.getTicket)
					r.Get("/comments", h.listComments)
					r.Get("/history", h.getTicketHistory)
					r.Get("/suggestions", h.ticketSuggestions)
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
				r.With(auth.RequirePermission(authz.WorkflowsReadAll)).Get("/{app}/{name}/deliveries", h.listWebhookDeliveries)
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
			r.Route("/settings/sms", func(r chi.Router) {
				r.Use(auth.RequirePermission(authz.SettingsManage))
				r.Get("/", h.getSMSSettings)
				r.Put("/", h.putSMSSettings)
				r.Post("/test", h.testSMS)
			})
			r.Route("/settings/inbound", func(r chi.Router) {
				r.Use(auth.RequirePermission(authz.SettingsManage))
				r.Get("/", h.getInboundSettings)
				r.Put("/", h.putInboundSettings)
				r.Post("/test", h.testInbound)
				r.Get("/log", h.listInboundEmails)
			})
			r.With(auth.RequirePermission(authz.SettingsManage)).Get("/settings/teams", h.getTeamsSettings)
			r.With(auth.RequirePermission(authz.SettingsManage)).Put("/settings/teams", h.putTeamsSettings)
			r.With(auth.RequirePermission(authz.SettingsManage)).Get("/settings/messaging/deliveries", h.listChannelDeliveries)
			r.With(auth.RequirePermission(authz.SettingsManage)).Put("/groups/{id}/teams", h.putGroupTeams)
			r.With(auth.RequirePermission(authz.SettingsManage)).Delete("/groups/{id}/teams", h.deleteGroupTeams)
			r.With(auth.RequirePermission(authz.SettingsManage)).Get("/settings/sign-in", h.getSignInSettings)
			r.With(auth.RequirePermission(authz.SettingsManage)).Put("/settings/sign-in", h.putSignInSettings)
			r.Route("/sso-providers", func(r chi.Router) {
				r.Use(auth.RequirePermission(authz.SettingsManage))
				r.Get("/", h.listSSOProviders)
				r.Put("/{id}", h.putSSOProvider)
				r.Delete("/{id}", h.deleteSSOProvider)
			})
			r.With(auth.RequirePermission(authz.SettingsManage)).Get("/settings/history", h.getHistorySettings)
			r.With(auth.RequirePermission(authz.SettingsManage)).Put("/settings/history", h.putHistorySettings)
			r.With(auth.RequirePermission(authz.SettingsManage)).Get("/settings/instance", h.getInstanceSettings)
			r.With(auth.RequirePermission(authz.SettingsManage)).Put("/settings/instance", h.putInstanceSettings)
			r.Route("/backups", func(r chi.Router) {
				r.Use(auth.RequirePermission(authz.InstanceBackup))
				r.Get("/", h.getBackups)
				r.Post("/", h.takeSnapshot)
				r.Put("/settings", h.putBackupSettings)
				r.Post("/upload", h.uploadSnapshot)
				r.Get("/{id}/file", h.downloadSnapshot)
				r.Delete("/{id}", h.deleteSnapshot)
				r.Post("/{id}/restore", h.restoreSnapshot)
			})
			r.Route("/clones", func(r chi.Router) {
				r.Use(auth.RequirePermission(authz.InstanceClone))
				r.Get("/", h.getClones)
				r.Post("/profiles", h.createCloneProfile)
				r.Put("/profiles/{name}", h.updateCloneProfile)
				r.Delete("/profiles/{name}", h.deleteCloneProfile)
				r.Post("/preview", h.previewClone)
				r.Post("/packages", h.makeClonePackage)
				r.Get("/packages/{id}/file", h.downloadClonePackage)
				r.Delete("/packages/{id}", h.deleteClonePackage)
			})
			r.Route("/clone-in", func(r chi.Router) {
				r.Use(auth.RequirePermission(authz.InstanceClone))
				r.Get("/", h.getCloneIn)
				r.Put("/settings", h.putCloneInSettings)
				r.Put("/keep/{userId}", h.keepOnClone)
				r.Delete("/keep/{userId}", h.unkeepOnClone)
				r.Post("/upload", h.uploadCloneLoad)
				r.Post("/{id}/preview", h.previewCloneLoad)
				r.Post("/{id}/apply", h.applyCloneLoad)
				r.Delete("/{id}", h.deleteCloneLoad)
			})
			r.With(auth.RequirePermission(authz.SettingsManage)).Get("/settings/limits", h.getLimits)
			r.With(auth.RequirePermission(authz.SettingsManage)).Put("/settings/limits", h.putLimits)
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
			r.Put("/account/phone", h.putMyPhone)
			r.Post("/account/phone/verify", h.verifyMyPhone)
			r.Delete("/account/phone", h.deleteMyPhone)
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
			// Apps' connections: the hosts they may call and their secrets.
			r.With(auth.RequirePermission(authz.SettingsManage)).Get("/connections", h.listConnections)
			r.With(auth.RequirePermission(authz.SettingsManage)).Put("/connections/{app}/secrets/{key}", h.putConnectionSecret)
			// Scripts: checked as they are typed and tried against a record by those who may change them; the apps'
			// script files read by whoever may read workflows.
			r.Route("/scripts", func(r chi.Router) {
				r.With(auth.RequirePermission(authz.ScriptsEdit)).Post("/check", h.checkScript)
				r.With(auth.RequirePermission(authz.ScriptsEdit)).Post("/test", h.testScript)
				r.With(auth.RequirePermission(authz.WorkflowsReadAll, authz.WorkflowsEdit)).Get("/", h.listScriptFiles)
				r.With(auth.RequirePermission(authz.WorkflowsReadAll, authz.WorkflowsEdit)).Get("/{app}/{file}", h.getScriptFile)
				r.With(auth.RequirePermission(authz.ScriptsEdit)).Put("/{app}/{file}", h.putScriptFile)
				r.With(auth.RequirePermission(authz.ScriptsEdit)).Delete("/{app}/{file}/edits", h.resetScriptFile)
			})
			// The form editor: forms:manage.
			r.With(auth.RequirePermission(authz.FormsManage)).Get("/forms", h.listFormDefs)
			r.With(auth.RequirePermission(authz.FormsManage)).Get("/forms/{type}/definition", h.getFormDef)
			r.With(auth.RequirePermission(authz.FormsManage)).Put("/forms/{type}/definition", h.putFormDef)
			r.With(auth.RequirePermission(authz.FormsManage)).Post("/forms/{type}/definition/check", h.checkFormDef)
			r.With(auth.RequirePermission(authz.FormsManage)).Delete("/forms/{type}/edits", h.resetFormDef)
			// Reports: reports:read.
			r.With(auth.RequirePermission(authz.ReportsRead)).Get("/reports/slas", h.slaReport)
			r.With(auth.RequirePermission(authz.ReportsRead)).Get("/reports/slas/export", h.slaReportExport)
			// QUERY (RFC 10008) is the way to run one; POST is the same, for networks that do not pass QUERY.
			r.With(auth.RequirePermission(authz.QueryRun)).Query("/query", h.runQuery)
			r.With(auth.RequirePermission(authz.QueryRun), stopWithClient).Post("/query", h.runQuery)
			r.With(auth.RequirePermission(authz.QueryRun)).Get("/query/schema", h.querySchema)
			r.With(auth.RequirePermission(authz.QueryRun)).Get("/query/runs", h.listQueryRuns)
			// Saved views: queries saved under a name, read like a table.
			r.Route("/views", func(r chi.Router) {
				r.Use(auth.RequirePermission(authz.QueryRun))
				r.Get("/", h.listSavedViews)
				r.Get("/{name}", h.getSavedView)
				r.Put("/{name}", h.putSavedView)
				r.Delete("/{name}", h.deleteSavedView)
				r.Get("/{name}/rows", h.readSavedView)
				r.Query("/{name}/rows", h.queryViewRows)
			})
			// Which hours each SLA counts: slas:manage.
			r.With(auth.RequirePermission(authz.SLAsManage)).Get("/slas", h.listSLADefs)
			r.With(auth.RequirePermission(authz.SLAsManage)).Put("/slas/{app}/{name}/calendar", h.putSLACalendar)
			r.With(auth.RequirePermission(authz.SLAsManage)).Delete("/slas/{app}/{name}/edits", h.resetSLADef)
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

// writeTimeout bounds a change the client is no longer waiting for. It is long because file parts are uploads too:
// a slow link still sending one is not a client that has gone.
const writeTimeout = 5 * time.Minute

// methodQuery is RFC 10008's QUERY: a read whose input is in the body. net/http has no constant for it yet.
const methodQuery = "QUERY"

// clientContextKey holds the request's own context (done when the client goes away) under one finishWrites detached.
type clientContextKey struct{}

// finishWrites lets a change finish when the client goes away. A read (GET, HEAD, OPTIONS, QUERY) stops with the
// client. A change does not: once someone has sent it (a field saved as they leave it, then the tab reloaded or
// closed), it is carried through or fails on its own within writeTimeout, instead of being cut off half done and
// silently lost.
func finishWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions, methodQuery:
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), writeTimeout)
		defer cancel()
		ctx = context.WithValue(ctx, clientContextKey{}, r.Context())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// stopWithClient makes a route that is a read in all but its method (POST /query, for clients that cannot send
// QUERY) stop when the client goes away, as a read does, keeping everything the middleware before it added.
func stopWithClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, ok := r.Context().Value(clientContextKey{}).(context.Context)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(client, cancel)
		defer stop()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
