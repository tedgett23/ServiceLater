package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// The changes made in ServiceLater itself (the studio layer, kept in the database) as files, and back. Exported, they
// are an app's local/ files, so they can be kept in version control and carried from one environment to the next
// (dev, then test, then production) without moving any records.

var (
	appNameRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
	localFileRE = regexp.MustCompile(`^[a-z]+/[a-z0-9][a-z0-9_.-]*\.yaml$`)
)

func appExportLocal(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("export-local", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	only := fs.String("app", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return errors.New("usage: servicelater app export-local [-app name] <dir>")
	}
	dir := fs.Arg(0)
	pool, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	var rows []dbgen.ListAllAppOverridesRow
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		rows, err = dbgen.New(tx).ListAllAppOverrides(ctx)
		return err
	}); err != nil {
		return err
	}
	n := 0
	for _, r := range rows {
		if *only != "" && r.AppName != *only {
			continue
		}
		target := filepath.Join(dir, r.AppName, filepath.FromSlash(r.Path))
		if *only != "" {
			target = filepath.Join(dir, filepath.FromSlash(r.Path))
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(r.Content), 0o600); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, target)
		n++
	}
	_, err = fmt.Fprintf(out, "exported %d file(s)\n", n)
	return err
}

func appImportLocal(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("import-local", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	only := fs.String("app", "", "")
	replace := fs.Bool("replace", false, "")
	dry := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return errors.New("usage: servicelater app import-local [-app name] [-replace] [-dry-run] <dir>")
	}
	byApp, err := readLocalDir(fs.Arg(0), *only)
	if err != nil {
		return err
	}
	if len(byApp) == 0 && !*replace {
		return fmt.Errorf("%s has no local files to import", fs.Arg(0))
	}
	pool, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	if *only != "" && byApp[*only] == nil {
		byApp[*only] = map[string][]byte{} // -replace with an empty folder clears the app's changes
	}
	if *replace && *only == "" {
		// Matching the folder exactly also clears the apps it has no folder for.
		if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
			rows, err := dbgen.New(tx).ListAllAppOverrides(ctx)
			for _, r := range rows {
				if byApp[r.AppName] == nil {
					byApp[r.AppName] = map[string][]byte{}
				}
			}
			return err
		}); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(byApp))
	for name := range byApp {
		names = append(names, name)
	}
	sort.Strings(names)
	failed := 0
	for _, name := range names {
		_, plan, err := apps.UpdateStudio(ctx, pool, apps.StudioUpdate{App: name, Files: byApp[name], ReplaceAll: *replace,
			InstalledBy: "import-local", DryRun: *dry})
		if err != nil {
			failed++
			_, _ = fmt.Fprintf(out, "%s: not imported, so it is unchanged:\n%v\n", name, err)
			continue
		}
		_, _ = fmt.Fprint(out, plan)
	}
	if *dry {
		_, _ = fmt.Fprintln(out, "dry run: nothing was changed")
	}
	if failed > 0 {
		return fmt.Errorf("%d app(s) could not be imported", failed)
	}
	return nil
}

// readLocalDir reads local/ files: one folder per app under dir, or, with only set, that app's files straight in dir.
// Anything that is not a definition file (lifecycles/x.yaml, workflows/y.yaml...) is refused, not skipped.
func readLocalDir(dir, only string) (map[string]map[string][]byte, error) {
	out := map[string]map[string][]byte{}
	var bad []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != dir {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		app, file := only, rel
		if only == "" {
			var ok bool
			if app, file, ok = strings.Cut(rel, "/"); !ok || !appNameRE.MatchString(app) {
				bad = append(bad, rel+": expected <app>/<folder>/<file>.yaml")
				return nil
			}
		}
		if !localFileRE.MatchString(file) {
			bad = append(bad, rel+": not a definition file such as workflows/name.yaml")
			return nil
		}
		data, err := os.ReadFile(p) //nolint:gosec // the operator names the folder
		if err != nil {
			return err
		}
		if out[app] == nil {
			out[app] = map[string][]byte{}
		}
		out[app][file] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("nothing was imported:\n  %s", strings.Join(bad, "\n  "))
	}
	return out, nil
}
