package apps

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// The studio layer is an app's local layer edited in ServiceLater itself (docs/apps-platform.md §4.5): override files
// kept in the database (app_overrides), applied over the shipped files and any on disk at every install and sync, so
// an upgrade never undoes them. The workflow builder writes it; anything that installs an app attaches it.

// LoadStudio reads an app's studio layer, keyed like its local/ folder.
func LoadStudio(ctx context.Context, q *dbgen.Queries, app string) (map[string][]byte, error) {
	rows, err := q.ListAppOverrides(ctx, app)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(rows))
	for _, r := range rows {
		out[r.Path] = []byte(r.Content)
	}
	return out, nil
}

// AttachStudio reads the app's studio layer from the database, as the system.
func (b *Bundle) AttachStudio(ctx context.Context, pool *pgxpool.Pool) error {
	name := b.appName()
	if name == "" || pool == nil {
		return nil
	}
	return db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		var err error
		b.Studio, err = LoadStudio(ctx, dbgen.New(tx), name)
		return err
	})
}

// StoredBundle is an installed app's own files as its last install kept them (app_files), or, for a bundled app
// installed before files were kept, the files in this binary. q must be the system's.
func StoredBundle(ctx context.Context, q *dbgen.Queries, app string) (*Bundle, error) {
	rows, err := q.ListAppFiles(ctx, app)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		if b, err := LoadBundled(app); err == nil {
			return b, nil
		}
		return nil, fmt.Errorf("the files of %s are not kept: install it again once, then it can be edited here", app)
	}
	b := &Bundle{Files: make(map[string][]byte, len(rows))}
	for _, r := range rows {
		b.Files[r.Path] = r.Content
	}
	return b, nil
}

// Validate is what `app validate` checks: the shipped layer alone, then with every local layer applied.
func (b *Bundle) Validate() (*Compiled, error) { return b.validate() }

// storeFiles keeps an app's own files, replacing what an earlier install kept.
func storeFiles(ctx context.Context, q *dbgen.Queries, c *Compiled) error {
	if c.Files == nil {
		return nil
	}
	if err := q.DeleteAppFiles(ctx, c.Manifest.Name); err != nil {
		return err
	}
	for path, content := range c.Files {
		if err := q.InsertAppFile(ctx, dbgen.InsertAppFileParams{AppName: c.Manifest.Name, Path: path, Content: content}); err != nil {
			return err
		}
	}
	return nil
}

// Below is a definition file as every layer under the studio layer makes it (default, local, instance, merged): what
// an edit made here is a change to. ok is false when no such layer has the file.
func (b *Bundle) Below(rel string) (content []byte, ok bool, err error) {
	var p Problems
	for _, l := range b.layers(&p) {
		if l.name == "studio" {
			continue
		}
		over, has := l.files[rel]
		if !has {
			continue
		}
		if content == nil {
			content = over
			continue
		}
		if content, err = mergeYAML(content, over); err != nil {
			return nil, false, err
		}
	}
	return content, content != nil, nil
}

// StudioUpdate changes an installed app's studio layer.
type StudioUpdate struct {
	App string
	// Files are written into the layer, keyed like local/ (workflows/cab.yaml); a nil one is removed.
	Files map[string][]byte
	// ReplaceAll makes the layer exactly Files: anything else in it is removed (an import that mirrors a folder).
	ReplaceAll  bool
	UpdatedBy   uuid.NullUUID
	InstalledBy string
	DryRun      bool
}

// UpdateStudio compiles an installed app with its studio layer changed, then installs it and records the change in
// one transaction: an edit that does not compile (Problems) or that the install refuses changes nothing.
func UpdateStudio(ctx context.Context, pool *pgxpool.Pool, u StudioUpdate) (*Compiled, *Plan, error) {
	var b *Bundle
	if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var err error
		if b, err = StoredBundle(ctx, q, u.App); err != nil {
			return err
		}
		b.Studio, err = LoadStudio(ctx, q, u.App)
		return err
	}); err != nil {
		return nil, nil, err
	}
	if err := b.AttachInstanceLocal(LocalRoot()); err != nil {
		return nil, nil, err
	}
	before := b.Studio
	next := map[string][]byte{}
	if !u.ReplaceAll {
		for k, v := range before {
			next[k] = v
		}
	}
	for k, v := range u.Files {
		if v == nil {
			delete(next, k)
		} else {
			next[k] = v
		}
	}
	b.Studio = next
	c, err := b.Validate()
	if err != nil {
		return nil, nil, err
	}
	plan, err := Install(ctx, pool, c, InstallOptions{DryRun: u.DryRun, InstalledBy: u.InstalledBy, Also: func(ctx context.Context, q *dbgen.Queries) error {
		for path := range before {
			if _, keep := next[path]; !keep {
				if _, err := q.DeleteAppOverride(ctx, dbgen.DeleteAppOverrideParams{AppName: u.App, Path: path}); err != nil {
					return err
				}
			}
		}
		for path, content := range next {
			if old, had := before[path]; had && string(old) == string(content) {
				continue
			}
			if err := q.UpsertAppOverride(ctx, dbgen.UpsertAppOverrideParams{AppName: u.App, Path: path, Content: string(content),
				UpdatedBy: u.UpdatedBy}); err != nil {
				return err
			}
		}
		return nil
	}})
	if err != nil {
		return nil, nil, err
	}
	return c, plan, nil
}

// storeHistoryExclusions keeps the fields the app's forms and classes leave out of history (history: false), where
// the history triggers look for them, replacing what an earlier install kept.
func storeHistoryExclusions(ctx context.Context, q *dbgen.Queries, c *Compiled) error {
	if err := q.DeleteHistoryExclusions(ctx, c.Manifest.Name); err != nil {
		return err
	}
	add := func(subject, kind, field string) error {
		return q.InsertHistoryExclusion(ctx, dbgen.InsertHistoryExclusionParams{AppName: c.Manifest.Name, Subject: subject, Kind: kind, Field: field})
	}
	classes := map[string]*Class{}
	for _, d := range c.Definitions {
		if f := d.Form; f != nil {
			for _, fd := range f.Fields {
				if fd.Recorded() {
					continue
				}
				field := "fields." + fd.Name
				if _, core := coreField(f.RecordType, fd.Name); core {
					field = fd.Name
				}
				if err := add("task", f.RecordType, field); err != nil {
					return err
				}
			}
		}
		if d.Class != nil {
			classes[d.Class.Name] = d.Class
		}
	}
	// A class has the fields of the classes it extends, so what one leaves out is left out of those too.
	resolveClasses(classes)
	for _, cl := range classes {
		for _, fd := range cl.All {
			if !fd.Recorded() {
				if err := add("ci", cl.Name, "fields."+fd.Name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
