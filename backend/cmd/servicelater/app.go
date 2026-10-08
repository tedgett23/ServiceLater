package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/config"
	"github.com/servicelater/servicelater/backend/internal/db"
)

const appUsage = `usage: servicelater app <command>

  validate <path>...            check apps (folders or .swapp files); needs no database
  pack [-o dir] <path>          write <name>-<version>.swapp (the shipped layer only, never local/)
  install [-dry-run] <path>...  install or update apps, dependencies first
  sync [-dry-run]               install or upgrade the apps bundled in this binary, as at startup
  uninstall <name>...           remove apps and everything they define; records are kept
  list                          show installed apps
  export-local [-app name] <dir>
                                write the changes made in ServiceLater (the builder) as local/ files: one folder
                                per app in <dir>, or with -app that app's files straight into <dir>
  import-local [-app name] [-replace] [-dry-run] <dir>
                                load such files back into this instance and reinstall the apps; -replace removes
                                the changes the files do not have, so the instance matches them exactly

Flags go before paths. The database is taken from SL_DATABASE_URL, bundled apps from SL_BUNDLED_APPS, and this
instance's local overrides from SL_APPS_LOCAL_DIR (one folder per app, laid out like an app's local/ folder)`

func runApp(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(appUsage)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "validate":
		return appValidate(rest, out)
	case "pack":
		return appPack(rest, out)
	case "install":
		return appInstall(ctx, rest, out)
	case "sync":
		return appSync(ctx, rest, out)
	case "uninstall":
		return appUninstall(ctx, rest, out)
	case "list":
		return appList(ctx, out)
	case "export-local":
		return appExportLocal(ctx, rest, out)
	case "import-local":
		return appImportLocal(ctx, rest, out)
	}
	return fmt.Errorf("unknown command %q\n\n%s", cmd, appUsage)
}

func appValidate(paths []string, out io.Writer) error {
	if len(paths) == 0 {
		return errors.New("usage: servicelater app validate <path> [<path> ...]")
	}
	failed := 0
	for _, p := range paths {
		c, err := apps.ValidateIn(p, apps.LocalRoot())
		if err != nil {
			failed++
			_, _ = fmt.Fprintf(out, "%s: invalid\n", p)
			for _, line := range strings.Split(err.Error(), "\n") {
				_, _ = fmt.Fprintf(out, "  %s\n", line)
			}
			continue
		}
		_, _ = fmt.Fprintf(out, "%s: ok (%s %s, %s)\n", p, c.Manifest.Name, c.Manifest.Version, plural(len(c.Definitions), "definition"))
	}
	if failed > 0 {
		return fmt.Errorf("%s failed validation", plural(failed, "app"))
	}
	return nil
}

func appPack(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("pack", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("o", ".", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return errors.New("usage: servicelater app pack [-o dir] <path>")
	}
	name, data, err := apps.Pack(fs.Arg(0))
	if err != nil {
		return err
	}
	dest := filepath.Join(*dir, name)
	if err := os.WriteFile(dest, data, 0o644); err != nil { //nolint:gosec // an app archive is meant to be shared, so it is world-readable
		return err
	}
	_, err = fmt.Fprintf(out, "wrote %s (%d bytes)\n", dest, len(data))
	return err
}

func appInstall(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dry := fs.Bool("dry-run", false, "")
	approve := fs.Bool("approve-permissions", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		return errors.New("usage: servicelater app install [-dry-run] [-approve-permissions] <path> [<path> ...]")
	}

	pool, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Validate everything before writing anything, so a typo in the last app does not leave the first half installed.
	// Each app is checked with its local layers, edits made in ServiceLater included.
	var bundles []*apps.Compiled
	for _, p := range fs.Args() {
		b, err := apps.Load(p)
		if err == nil {
			err = b.AttachInstanceLocal(apps.LocalRoot())
		}
		if err == nil {
			err = b.AttachStudio(ctx, pool)
		}
		var c *apps.Compiled
		if err == nil {
			c, err = b.Validate()
		}
		if err != nil {
			return fmt.Errorf("%s:\n%w", p, err)
		}
		bundles = append(bundles, c)
	}
	ordered, err := apps.OrderByDependencies(bundles)
	if err != nil {
		return err
	}
	plans, err := apps.InstallAll(ctx, pool, ordered, apps.InstallOptions{DryRun: *dry, InstalledBy: "cli", ApprovePermissions: *approve})
	if err != nil {
		return err
	}
	for _, plan := range plans {
		_, _ = fmt.Fprint(out, plan)
	}
	if *dry {
		_, _ = fmt.Fprintln(out, "dry run: nothing was changed")
	}
	return nil
}

func appSync(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dry := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: servicelater app sync [-dry-run]")
	}
	names, err := apps.BundledSelection(os.Getenv("SL_BUNDLED_APPS"))
	if err != nil {
		return err
	}
	pool, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	if *dry {
		// Plan the whole batch in one rolled-back transaction, so each app sees the ones before it.
		var ready []*apps.Compiled
		failed := 0
		for _, name := range names {
			b, err := apps.LoadBundled(name)
			if err == nil {
				err = b.AttachInstanceLocal(apps.LocalRoot())
			}
			if err == nil {
				err = b.AttachStudio(ctx, pool)
			}
			var c *apps.Compiled
			if err == nil {
				c, err = b.Compile()
			}
			if err != nil {
				failed++
				_, _ = fmt.Fprintf(out, "%s: %v\n", name, err)
				continue
			}
			ready = append(ready, c)
		}
		ordered, err := apps.OrderByDependencies(ready)
		if err != nil {
			return err
		}
		plans, err := apps.InstallAll(ctx, pool, ordered, apps.InstallOptions{DryRun: true, ApprovePermissions: true})
		if err != nil {
			return err
		}
		for _, plan := range plans {
			_, _ = fmt.Fprint(out, plan)
		}
		_, _ = fmt.Fprintln(out, "dry run: nothing was changed")
		if failed > 0 {
			return fmt.Errorf("%s could not be planned", plural(failed, "app"))
		}
		return nil
	}

	failed := 0
	for _, r := range apps.SyncBundled(ctx, pool, names, apps.LocalRoot()) {
		if r.Err != nil {
			failed++
			_, _ = fmt.Fprintf(out, "%s: %v\n", r.App, r.Err)
			continue
		}
		_, _ = fmt.Fprint(out, r.Plan)
	}
	if failed > 0 {
		return fmt.Errorf("%s could not be synced", plural(failed, "app"))
	}
	return nil
}

func appUninstall(ctx context.Context, names []string, out io.Writer) error {
	if len(names) == 0 {
		return errors.New("usage: servicelater app uninstall <name> [<name> ...]")
	}
	pool, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	for _, name := range names {
		removed, err := apps.Uninstall(ctx, pool, name)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "uninstalled %s (%s removed; records kept)\n", name, plural(len(removed), "definition"))
		if bundled, _ := apps.BundledSelection(os.Getenv("SL_BUNDLED_APPS")); slices.Contains(bundled, name) {
			_, _ = fmt.Fprintf(out, "  note: %s is bundled with this build and is installed again at the next start; leave it out of SL_BUNDLED_APPS to keep it off\n", name)
		}
	}
	return nil
}

func appList(ctx context.Context, out io.Writer) error {
	pool, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	list, err := apps.List(ctx, pool)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		_, err = fmt.Fprintln(out, "no apps installed")
		return err
	}
	for _, a := range list {
		extra := ""
		if a.Local > 0 {
			extra = fmt.Sprintf(" (%d with local changes)", a.Local)
		}
		_, _ = fmt.Fprintf(out, "%-24s %-8s %s%s\n", a.Name, a.Version, plural(a.Definitions, "definition"), extra)
	}
	return nil
}

func openDatabase(ctx context.Context) (*pgxpool.Pool, error) {
	pool, err := db.Connect(ctx, config.DatabaseURL())
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
