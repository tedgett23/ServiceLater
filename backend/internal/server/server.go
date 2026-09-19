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
	"github.com/servicelater/servicelater/backend/internal/config"
	"github.com/servicelater/servicelater/backend/internal/webui"
)

func Run(ctx context.Context, cfg config.Config) error {
	if cfg.NeedsDB() {
		// TODO: connect pgxpool and run embedded goose migrations (under an advisory lock).
	}
	if cfg.Has(config.RoleAPI) {
		// TODO: start the LISTEN/NOTIFY goroutine and WebSocket hub.
	}
	if cfg.Has(config.RoleWorker) {
		// TODO: start job workers and the workflow engine.
	}

	h, err := handler(cfg)
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
func handler(cfg config.Config) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	var apiH http.Handler
	switch {
	case cfg.Has(config.RoleAPI):
		apiH = api.NewRouter()
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
