package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/apps"
	"github.com/servicelater/servicelater/backend/internal/clone"
	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
	"github.com/servicelater/servicelater/backend/internal/storage"
)

const cloneUsage = `servicelater clone: make and check clone packages of this instance (docs/backup-and-clone.md §2).

  servicelater clone package -profile name -passphrase-file f [-o file]
      write a clone package made from a profile of Admin → Clones, encrypted with the passphrase in f
      (or SL_CLONE_PASSPHRASE)
  servicelater clone verify -passphrase-file f <file>
      read a whole clone package and check it, changing nothing`

func clonePassphrase(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	if p := os.Getenv("SL_CLONE_PASSPHRASE"); p != "" {
		return p, nil
	}
	return "", errors.New("give the passphrase in a file (-passphrase-file) or SL_CLONE_PASSPHRASE")
}

func runClone(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(cloneUsage)
	}
	switch args[0] {
	case "package":
		fs := flag.NewFlagSet("clone package", flag.ContinueOnError)
		profile := fs.String("profile", "", "the profile to make it from")
		passFile := fs.String("passphrase-file", "", "a file holding the passphrase")
		file := fs.String("o", "", "the file to write (default servicelater-<profile>-<time>.slclone here)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 || *profile == "" {
			return errors.New(cloneUsage)
		}
		passphrase, err := clonePassphrase(*passFile)
		if err != nil {
			return err
		}
		if *file == "" {
			*file = "servicelater-" + *profile + "-" + time.Now().UTC().Format("20060102-150405") + ".slclone"
		}
		pool, err := openDatabase(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		reg := apps.NewRegistry(pool)
		if err := reg.Load(ctx); err != nil {
			return err
		}
		var config clone.Profile
		var in settings.Instance
		if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			p, err := q.GetCloneProfile(ctx, *profile)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("there is no clone profile %q", *profile)
			}
			if err != nil {
				return err
			}
			if config, err = clone.ProfileFrom(p.Config); err != nil {
				return err
			}
			rows, err := q.GetSystemSettings(ctx, settings.InstancePrefix)
			stored := map[string][]byte{}
			for _, r := range rows {
				stored[r.Name] = r.Value
			}
			in = settings.InstanceFrom(stored)
			return err
		}); err != nil {
			return err
		}
		o := clone.Options{Profile: config, ProfileName: *profile, Passphrase: passphrase, Exclusion: clone.Compiler(reg),
			MaskedFields: clone.MaskedFields(reg), Instance: in.Name, Environment: in.Environment, Binary: version()}
		if keys, err := secrets.FromEnv(); err == nil {
			dir := os.Getenv("SL_ATTACHMENTS_DIR")
			if dir == "" {
				dir = "/data/attachments"
			}
			files := &storage.Manager{Pool: pool, Keys: keys, LocalDir: dir}
			o.Files = func(ctx context.Context, backend, key string) (io.ReadCloser, int64, error) {
				store, err := files.For(ctx, backend)
				if err != nil {
					return nil, 0, err
				}
				f, err := store.Open(ctx, key)
				if err != nil {
					return nil, 0, err
				}
				return f, f.Size(), nil
			}
		}
		tmp := *file + ".partial"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600) //nolint:gosec // a path the operator chose
		if err != nil {
			return err
		}
		sum, err := clone.Export(ctx, pool, f, o)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, *file); err != nil {
			return err
		}
		return cloneReport(out, "wrote "+*file, sum)
	case "verify":
		fs := flag.NewFlagSet("clone verify", flag.ContinueOnError)
		passFile := fs.String("passphrase-file", "", "a file holding the passphrase")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			return errors.New(cloneUsage)
		}
		passphrase, err := clonePassphrase(*passFile)
		if err != nil {
			return err
		}
		f, err := os.Open(filepath.Clean(fs.Arg(0)))
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		sum, err := clone.Verify(f, passphrase)
		if err != nil {
			return err
		}
		return cloneReport(out, fs.Arg(0)+" is complete and unchanged", sum)
	}
	return fmt.Errorf("unknown command %q\n\n%s", args[0], cloneUsage)
}

func cloneReport(out io.Writer, what string, sum *clone.Summary) error {
	var rows int64
	for _, n := range sum.Rows {
		rows += n
	}
	var kinds []string
	for _, c := range sum.Report.Records {
		kinds = append(kinds, fmt.Sprintf("%s %d of %d", c.Type, c.Kept, c.Total))
	}
	sort.Strings(kinds)
	from := sum.Instance
	if from == "" {
		from = "an unnamed instance"
	}
	_, err := fmt.Fprintf(out, "%s\n  profile %s, from %s, taken %s, schema %d\n  %d rows; records: %s\n  %d of %d people, %d attachment file(s), %d link(s) cleared\n",
		what, sum.Profile, from, sum.TakenAt.Format(time.RFC3339), sum.Schema, rows, strings.Join(kinds, ", "),
		sum.Report.People, sum.Report.PeopleTotal, sum.AttachmentFiles, sum.Report.ClearedTotal)
	return err
}
