package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/config"
	"github.com/servicelater/servicelater/backend/internal/server"
)

// commit is the source revision the binary was built from, set at build time:
// go build -ldflags "-X main.commit=$(git rev-parse --short HEAD)". Empty for a plain go build.
var commit string

const usage = `servicelater: one binary for the web UI, the API and the background workers.

Usage:
  servicelater                 run the server (SL_ROLES picks web, api, worker; all by default)
  servicelater version         print the version
  servicelater app ...         install, upgrade and inspect apps (servicelater app lists them)
  servicelater user ...        create users and reset passwords
  servicelater group ...       list and change groups
  servicelater jobs ...        the background job queue
  servicelater secrets ...     make and rotate the key that encrypts stored secrets
  servicelater storage ...     check the attachments folder
  servicelater snapshot ...    take, check and restore snapshots of this instance
  servicelater clone ...       make and check clone packages (copies for UAT or development)

Configuration is environment variables (SL_DATABASE_URL, SL_JWT_SECRET, ...): see .env.example.
`

func version() string {
	v := "servicelater " + apps.PlatformVersion
	if commit != "" {
		v += " (" + commit + ")"
	}
	return v
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-version":
			fmt.Println(version())
			return
		case "help", "--help", "-help", "-h":
			fmt.Print(usage)
			return
		case "app", "clone", "jobs", "group", "secrets", "snapshot", "storage", "user":
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
			os.Exit(2)
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "app" {
		if err := runApp(context.Background(), os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "jobs" {
		if err := runJobs(context.Background(), os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "group" {
		if err := runGroup(context.Background(), os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "secrets" {
		if err := runSecrets(context.Background(), os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "snapshot" {
		if err := runSnapshot(context.Background(), os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "clone" {
		if err := runClone(context.Background(), os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "storage" {
		if err := runStorage(context.Background(), os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "user" {
		if err := runUser(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	slog.Info("starting", "version", version())
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx, cfg)
}
