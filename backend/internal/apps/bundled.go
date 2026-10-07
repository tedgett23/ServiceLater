package apps

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	firstparty "github.com/servicelater/servicelater/apps"
)

// DefaultBundled are the first-party apps installed at startup unless SL_BUNDLED_APPS says otherwise. The others
// ship in the binary too, but stay off until they have something to offer.
var DefaultBundled = []string{"core-itsm", "incident-management", "change-management"}

// BundledNames lists every app embedded in the binary.
func BundledNames() ([]string, error) {
	entries, err := fs.ReadDir(firstparty.FS, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || hidden(e.Name()) {
			continue
		}
		if _, err := fs.Stat(firstparty.FS, e.Name()+"/app.yaml"); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// LoadBundled reads an app embedded in the binary. It has only the shipped layer; the operator's local layer is
// attached from the instance's local directory (AttachInstanceLocal).
func LoadBundled(name string) (*Bundle, error) {
	sub, err := fs.Sub(firstparty.FS, name)
	if err != nil {
		return nil, err
	}
	files, err := readFiles(sub)
	if err != nil {
		return nil, err
	}
	if _, ok := files["app.yaml"]; !ok {
		return nil, fmt.Errorf("%q is not an app bundled with this build", name)
	}
	return &Bundle{Files: files}, nil
}

// BundledSelection interprets SL_BUNDLED_APPS: empty means the default set, "none" means none, "all" means every
// bundled app, and otherwise it is a comma-separated list of names.
func BundledSelection(setting string) ([]string, error) {
	setting = strings.TrimSpace(setting)
	switch setting {
	case "":
		return DefaultBundled, nil
	case "none":
		return nil, nil
	}
	all, err := BundledNames()
	if err != nil {
		return nil, err
	}
	if setting == "all" {
		return all, nil
	}
	var out []string
	for _, n := range strings.Split(setting, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !contains(all, n) {
			return nil, fmt.Errorf("SL_BUNDLED_APPS names %q, which is not bundled with this build (available: %s)", n, strings.Join(all, ", "))
		}
		out = append(out, n)
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// SyncResult is the outcome for one bundled app.
type SyncResult struct {
	App  string
	Plan *Plan
	Err  error
}

// SyncBundled installs or upgrades the named bundled apps, applying the instance's local layer. It is what runs at
// startup, and what `servicelater app sync` runs on demand, for example after editing a local file.
//
// A problem with one app is reported and leaves that app exactly as it was, so a bad local change or a failed
// upgrade never takes the instance down: it keeps running on what is already installed.
func SyncBundled(ctx context.Context, pool *pgxpool.Pool, names []string, localRoot string) []SyncResult {
	var results []SyncResult
	var ready []*Compiled
	for _, name := range names {
		b, err := LoadBundled(name)
		if err == nil {
			err = b.AttachInstanceLocal(localRoot)
		}
		var c *Compiled
		if err == nil {
			c, err = b.Compile()
		}
		if err != nil {
			results = append(results, SyncResult{App: name, Err: fmt.Errorf("not installed or upgraded, so it is unchanged: %w", err)})
			continue
		}
		ready = append(ready, c)
	}
	ordered, err := OrderByDependencies(ready)
	if err != nil {
		return append(results, SyncResult{Err: err})
	}
	for _, c := range ordered {
		// First-party apps ship with the binary, so the operator approved their permissions by deploying it.
		plan, err := Install(ctx, pool, c, InstallOptions{InstalledBy: "bundled", ApprovePermissions: true})
		results = append(results, SyncResult{App: c.Manifest.Name, Plan: plan, Err: err})
	}
	return results
}

// LogSync reports sync results the way the server does at startup.
func LogSync(results []SyncResult) {
	for _, r := range results {
		switch {
		case r.Err != nil:
			slog.Error("bundled app not synced", "app", r.App, "err", r.Err)
		case r.Plan.NoChange():
			slog.Info("bundled app up to date", "app", r.App, "version", r.Plan.ToVersion)
		default:
			slog.Info("bundled app synced", "app", r.App, "from", r.Plan.FromVersion, "to", r.Plan.ToVersion,
				"added", len(r.Plan.Added), "changed", len(r.Plan.Changed), "removed", len(r.Plan.Removed))
		}
	}
}

// OrderByDependencies puts each app after the apps in the same batch that it depends on.
func OrderByDependencies(all []*Compiled) ([]*Compiled, error) {
	byName := map[string]*Compiled{}
	for _, c := range all {
		if byName[c.Manifest.Name] != nil {
			return nil, fmt.Errorf("%s is given twice", c.Manifest.Name)
		}
		byName[c.Manifest.Name] = c
	}
	var out []*Compiled
	state := map[string]int{} // 0 new, 1 visiting, 2 done
	var visit func(c *Compiled) error
	visit = func(c *Compiled) error {
		switch state[c.Manifest.Name] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("apps depend on each other in a loop, through %s", c.Manifest.Name)
		}
		state[c.Manifest.Name] = 1
		deps := append([]Dependency{}, c.Manifest.Deps...)
		sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
		for _, d := range deps {
			if dep := byName[d.Name]; dep != nil {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		state[c.Manifest.Name] = 2
		out = append(out, c)
		return nil
	}
	for _, c := range all {
		if err := visit(c); err != nil {
			return nil, err
		}
	}
	return out, nil
}
