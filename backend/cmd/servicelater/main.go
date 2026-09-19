package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/servicelater/servicelater/backend/internal/config"
	"github.com/servicelater/servicelater/backend/internal/server"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "app" {
		// TODO: app install|update|uninstall|pack|dev (§11.3)
		fmt.Fprintln(os.Stderr, "app subcommands not implemented yet")
		os.Exit(1)
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
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx, cfg)
}
