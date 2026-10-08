package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/servicelater/servicelater/backend/internal/secrets"
	"github.com/servicelater/servicelater/backend/internal/settings"
	"github.com/servicelater/servicelater/backend/internal/storage"
)

const storageUsage = `usage: servicelater storage <command>

  check          write, read back and remove a small file with the instance's storage settings, as this server
  claim-folder   make this server's attachments folder the instance's, when the old one is gone for good: files kept
                 in the old folder can no longer be read. Every other server must then mount this same folder.

The folder is SL_ATTACHMENTS_DIR (default /data/attachments), the database SL_DATABASE_URL`

func runStorage(ctx context.Context, args []string, out io.Writer) error {
	if len(args) != 1 || (args[0] != "check" && args[0] != "claim-folder") {
		return errors.New(storageUsage)
	}
	keys, err := secrets.FromEnv()
	if err != nil {
		return err
	}
	pool, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	dir := os.Getenv("SL_ATTACHMENTS_DIR")
	if dir == "" {
		dir = "/data/attachments"
	}
	m := &storage.Manager{Pool: pool, Keys: keys, LocalDir: dir}
	if args[0] == "claim-folder" {
		id, err := m.ClaimFolder(ctx)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s is now the instance's attachments folder (%s)\n", dir, id)
		return err
	}
	s, cfg, err := m.Current(ctx)
	if err == nil {
		err = s.Probe(ctx)
	}
	if err != nil {
		return fmt.Errorf("storage (%s) does not work from this server: %w", cfg.Backend, err)
	}
	where := dir
	if cfg.Backend == settings.BackendS3 {
		where = cfg.S3Endpoint + "/" + cfg.S3Bucket
	}
	_, err = fmt.Fprintf(out, "storage works: %s (%s)\n", cfg.Backend, where)
	return err
}
