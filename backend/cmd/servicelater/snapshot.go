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

	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/snapshot"
)

const snapshotUsage = `servicelater snapshot: take, check and restore snapshots of this instance (docs/backup-and-clone.md).

  servicelater snapshot create [-o file] [-attachments]
      write a snapshot of the database (and, with -attachments, the files kept in SL_ATTACHMENTS_DIR);
      the file is encrypted under the secrets key (SL_SECRETS_KEY), which restoring it needs
  servicelater snapshot verify <file>
      read a whole snapshot and check it, changing nothing
  servicelater snapshot restore -yes [-change-schema] [-attachments] <file>
      replace everything in the database with the snapshot, in one transaction; everyone signs in again.
      -change-schema allows a snapshot from an older version (the database is migrated to it, loaded and
      migrated up again); -attachments writes the files inside it back to SL_ATTACHMENTS_DIR`

func runSnapshot(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(snapshotUsage)
	}
	keys, err := secrets.FromEnv()
	if err != nil {
		return err
	}
	attachmentsDir := os.Getenv("SL_ATTACHMENTS_DIR")
	if attachmentsDir == "" {
		attachmentsDir = "/data/attachments"
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("snapshot create", flag.ContinueOnError)
		file := fs.String("o", "", "the file to write (default servicelater-<time>.slsnap here)")
		withFiles := fs.Bool("attachments", false, "include the attachment files kept in SL_ATTACHMENTS_DIR")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
			return errors.New(snapshotUsage)
		}
		if *file == "" {
			*file = "servicelater-" + time.Now().UTC().Format("20060102-150405") + ".slsnap"
		}
		pool, err := openDatabase(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		tmp := *file + ".partial"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600) //nolint:gosec // a path the operator chose
		if err != nil {
			return err
		}
		o := snapshot.Options{Keys: keys, Binary: version()}
		if *withFiles {
			o.AttachmentsDir = attachmentsDir
		}
		sum, err := snapshot.Create(ctx, pool, f, o)
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
		return report(out, "wrote "+*file, sum)
	case "verify":
		if len(args) != 2 {
			return errors.New(snapshotUsage)
		}
		f, err := os.Open(filepath.Clean(args[1])) //nolint:gosec // a file the operator named
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		sum, err := snapshot.Verify(f, keys)
		if err != nil {
			return err
		}
		return report(out, args[1]+" is complete and unchanged", sum)
	case "restore":
		fs := flag.NewFlagSet("snapshot restore", flag.ContinueOnError)
		yes := fs.Bool("yes", false, "replace everything in the database (required)")
		change := fs.Bool("change-schema", false, "allow a snapshot from an older version")
		withFiles := fs.Bool("attachments", false, "write the attachment files inside it back to SL_ATTACHMENTS_DIR")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			return errors.New(snapshotUsage)
		}
		if !*yes {
			return errors.New("restoring replaces everything in the database: run it again with -yes")
		}
		f, err := os.Open(filepath.Clean(fs.Arg(0)))
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		pool, err := openDatabase(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		o := snapshot.RestoreOptions{Keys: keys, ChangeSchema: *change}
		if *withFiles {
			o.AttachmentsDir = attachmentsDir
		}
		sum, err := snapshot.Restore(ctx, pool, f, o)
		if err != nil {
			return err
		}
		return report(out, "restored "+fs.Arg(0), sum)
	}
	return fmt.Errorf("unknown command %q\n\n%s", args[0], snapshotUsage)
}

func report(out io.Writer, what string, sum *snapshot.Summary) error {
	var rows int64
	var biggest []string
	for t, n := range sum.Rows {
		rows += n
		if n > 0 {
			biggest = append(biggest, t)
		}
	}
	sort.Slice(biggest, func(i, j int) bool { return sum.Rows[biggest[i]] > sum.Rows[biggest[j]] })
	if len(biggest) > 5 {
		biggest = biggest[:5]
	}
	var most []string
	for _, t := range biggest {
		most = append(most, fmt.Sprintf("%s %d", t, sum.Rows[t]))
	}
	_, err := fmt.Fprintf(out, "%s\n  taken %s, schema %d, key %s\n  %d tables, %d rows (most: %s)\n  %d attachment file(s), %d bytes\n",
		what, sum.TakenAt.Format(time.RFC3339), sum.Schema, sum.KeyID, len(sum.Rows), rows, strings.Join(most, ", "), sum.AttachmentFiles, sum.AttachmentBytes)
	return err
}
