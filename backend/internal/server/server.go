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
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/servicelater/servicelater/backend/internal/api"
	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/config"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/realtime"
	"github.com/servicelater/servicelater/backend/internal/webui"
)

func Run(ctx context.Context, cfg config.Config) error {
	var deps api.Deps
	if cfg.NeedsDB() {
		pool, err := db.Connect(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := db.Migrate(ctx, pool); err != nil {
			return err
		}
		slog.Info("database ready")
		deps.Pool = pool

		if cfg.Has(config.RoleAPI) {
			tokens, err := auth.NewTokens(cfg.JWTSecret, cfg.AccessTTL)
			if err != nil {
				return err
			}
			deps.Tokens = tokens
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
			deps.Hub = realtime.NewHub()
			go deps.Hub.Run(ctx, cfg.DatabaseURL)
		}
		// TODO: when cfg.Has(config.RoleWorker), start job workers and the workflow engine.
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
