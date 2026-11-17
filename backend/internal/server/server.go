// Package server assembles the HTTP handler and listener for the roles this process runs.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/servicelater/servicelater/backend/internal/api"
	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/config"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/notify"
	"github.com/servicelater/servicelater/backend/internal/realtime"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/ruleruns"
	"github.com/servicelater/servicelater/backend/internal/ruleworker"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/slaclock"
	"github.com/servicelater/servicelater/backend/internal/slatimer"
	"github.com/servicelater/servicelater/backend/internal/storage"
	"github.com/servicelater/servicelater/backend/internal/webui"
	"github.com/servicelater/servicelater/backend/internal/workflow"
	"github.com/servicelater/servicelater/backend/internal/workflowengine"
)

func Run(ctx context.Context, cfg config.Config) error {
	var deps api.Deps
	// Background goroutines that must finish before the database pool closes (the job worker draining).
	var background sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cfg.NeedsDB() {
		pool, err := db.Connect(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer pool.Close()
		// Runs before pool.Close (defers unwind last-in first): stop the workers, then wait for them to drain.
		defer func() { cancel(); background.Wait() }()
		if err := db.Migrate(ctx, pool); err != nil {
			return err
		}
		slog.Info("database ready")
		deps.Pool = pool
		// The secrets key: without one the instance runs, and anything that needs a secret says so.
		keys, err := secrets.FromEnv()
		if err != nil {
			return err
		}
		if !keys.Configured() {
			slog.Warn("no secrets key: secrets (such as the mail server password) can be neither set nor used; set SL_SECRETS_KEY or SL_SECRETS_KEY_FILE")
		}
		deps.Keys = keys
		// Attachments: each role reads the storage settings from the database when it needs them.
		deps.Files = &storage.Manager{Pool: pool, Keys: keys, LocalDir: cfg.AttachmentsDir}

		// Install or upgrade the first-party apps that ship in this binary, applying this instance's local layer.
		// A problem with an app is logged and leaves it as it was: the instance keeps running.
		bundled, err := apps.BundledSelection(cfg.BundledApps)
		if err != nil {
			return err
		}
		apps.LogSync(apps.SyncBundled(ctx, pool, bundled, cfg.AppsLocalDir))

		if cfg.Has(config.RoleAPI) {
			tokens, err := auth.NewTokens(cfg.JWTSecret, cfg.AccessTTL)
			if err != nil {
				return err
			}
			deps.Tokens = tokens
			deps.Signer = storage.NewSigner(cfg.JWTSecret)
			deps.Auth = auth.NewService(pool, tokens, cfg.RefreshTTL)
			if cfg.BootstrapEmail != "" {
				created, err := deps.Auth.Bootstrap(ctx, cfg.BootstrapEmail, cfg.BootstrapName, cfg.BootstrapPassword)
				if err != nil {
					return err
				}
				if created {
					slog.Info("created bootstrap admin", "email", cfg.BootstrapEmail)
				}
			}
			go cleanupLoop(ctx, deps.Auth)
			deps.Apps = apps.NewRegistry(pool)
			if err := deps.Apps.Load(ctx); err != nil {
				return err
			}
			go deps.Apps.Run(ctx, cfg.DatabaseURL)
			deps.Hub = realtime.NewHub()
			deps.Hub.MaxPerUser = cfg.MaxLiveConns
			deps.Hub.Audience = realtime.AudienceFrom(pool)
			deps.Apps.OnChange(deps.Hub.DefinitionsChanged)
			go deps.Hub.Run(ctx, cfg.DatabaseURL)
		}
		if cfg.Has(config.RoleWorker) {
			// The worker has a small pool of its own, so a burst of background jobs can never take the connections
			// that web requests need (docs/apps-platform.md §7.5). Each executor holds one only while claiming or
			// recording a job, plus whatever its handler uses.
			workerPool, err := db.ConnectMax(ctx, cfg.DatabaseURL, int32(cfg.WorkerConcurrency)+2) //nolint:gosec // config.Load limits it to 1..64
			if err != nil {
				return err
			}
			worker := jobs.NewWorker(workerPool, jobs.Options{Concurrency: cfg.WorkerConcurrency, DSN: cfg.DatabaseURL})
			// The worker keeps its own copy of the installed definitions, refreshed by NOTIFY like the api's, so the
			// roles never depend on sharing a process (a worker started alone still knows every rule).
			workerApps := apps.NewRegistry(workerPool)
			if err := workerApps.Load(ctx); err != nil {
				return err
			}
			go workerApps.Run(ctx, cfg.DatabaseURL)
			worker.Register(ruleruns.Kind, ruleworker.Handler(workerPool, workerApps))
			// Workflows: the handler moves an execution one step; the timer wakes sleeping ones when they are due.
			engine := workflowengine.New(workerPool, &records.Service{Apps: workerApps, Pool: workerPool})
			worker.Register(workflow.RunKind, engine.Handler())
			worker.Register(slaclock.AlertKind, slatimer.Handler(workerPool, workerApps))
			worker.Register(notify.EmailKind, (&notify.Mailer{Pool: workerPool, Apps: workerApps, Keys: keys}).Handler())
			workerFiles := &storage.Manager{Pool: workerPool, Keys: keys, LocalDir: cfg.AttachmentsDir}
			worker.Register(storage.PurgeKind, workerFiles.PurgeHandler())
			worker.Register(storage.ExpireKind, workerFiles.ExpireHandler())
			background.Add(1)
			go func() {
				defer background.Done()
				var timer sync.WaitGroup
				timer.Add(2)
				go func() { defer timer.Done(); engine.RunTimer(ctx, time.Second) }()
				go func() { defer timer.Done(); slatimer.Run(ctx, workerPool, time.Second) }()
				worker.Run(ctx)
				timer.Wait() // both use the pool, so it closes only when both have stopped
				workerPool.Close()
			}()
			slog.Info("job worker started", "concurrency", cfg.WorkerConcurrency)
		}
	}

	h, err := handler(cfg, deps)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.Addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}

	var serve func() error
	switch cfg.TLS {
	case "auto":
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.Domain),
			Cache:      autocert.DirCache(cfg.CertCache),
		}
		srv.TLSConfig = &tls.Config{GetCertificate: m.GetCertificate, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1", "acme-tls/1"}}
		serve = func() error { return srv.ListenAndServeTLS("", "") }
	case "files":
		serve = func() error { return srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey) }
	default:
		serve = srv.ListenAndServe
	}

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr, "roles", roleList(cfg), "tls", cfg.TLS)
		errc <- serve()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	return nil
}

// handler routes /api, /ws and /healthz to the API (local or upstream) and everything else to the SPA.
func handler(cfg config.Config, deps api.Deps) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	var apiH http.Handler
	switch {
	case cfg.Has(config.RoleAPI):
		apiH = api.NewRouter(deps)
	case cfg.Has(config.RoleWeb) && cfg.APIURL != "":
		u, err := url.Parse(cfg.APIURL)
		if err != nil {
			return nil, err
		}
		apiH = httputil.NewSingleHostReverseProxy(u) // also proxies WebSocket upgrades
	}
	if apiH != nil {
		mux.Handle("/api/", apiH)
		mux.Handle("/ws/", apiH)
	}
	if cfg.Has(config.RoleWeb) {
		mux.Handle("/", webui.Handler())
	}
	return mux, nil
}

func roleList(cfg config.Config) string {
	var r []string
	for _, n := range []string{config.RoleWeb, config.RoleAPI, config.RoleWorker} {
		if cfg.Has(n) {
			r = append(r, n)
		}
	}
	return strings.Join(r, ",")
}

// cleanupLoop expires sessions and rate-limit rows (§5.2).
func cleanupLoop(ctx context.Context, a *auth.Service) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.Cleanup(ctx); err != nil {
				slog.Error("session cleanup", "err", err)
			}
		}
	}
}
