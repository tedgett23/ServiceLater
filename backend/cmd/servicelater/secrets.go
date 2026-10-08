package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/secrets"
)

const secretsUsage = `usage: servicelater secrets <command>

  gen-key   print a new random key for SL_SECRETS_KEY (or a file for SL_SECRETS_KEY_FILE)
  status    which secrets are stored, and whether this key can read each (never their values)
  rotate    re-encrypt every secret with the current key; the old one must be in SL_SECRETS_OLD_KEYS

Secrets are set in the admin section, never here, so they stay out of shell history.
The key comes from SL_SECRETS_KEY or SL_SECRETS_KEY_FILE, the database from SL_DATABASE_URL.

To rotate: generate a key, set it as SL_SECRETS_KEY and the old one in SL_SECRETS_OLD_KEYS, run rotate, restart every
server with the new key, then drop SL_SECRETS_OLD_KEYS`

func runSecrets(ctx context.Context, args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New(secretsUsage)
	}
	switch args[0] {
	case "gen-key":
		k, err := secrets.GenerateKey()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, k)
		return err
	case "status", "rotate":
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], secretsUsage)
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
	return db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if args[0] == "rotate" {
			n, err := secrets.Rotate(ctx, q, keys)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(out, "re-encrypted %d secret(s) with key %s\n", n, keys.Current())
			return err
		}
		rows, err := q.ListSecrets(ctx)
		if err != nil {
			return err
		}
		if !keys.Configured() {
			_, _ = fmt.Fprintln(out, "no key is configured (SL_SECRETS_KEY or SL_SECRETS_KEY_FILE)")
		} else {
			_, _ = fmt.Fprintf(out, "current key %s\n", keys.Current())
		}
		for _, r := range rows {
			state := "readable"
			switch {
			case !keys.Has(r.KeyID):
				state = "NOT readable: its key is not configured"
			case r.KeyID != keys.Current():
				state = "readable with an old key: run rotate"
			}
			_, _ = fmt.Fprintf(out, "%-24s key %s  %s\n", r.Name, r.KeyID, state)
		}
		if len(rows) == 0 {
			_, _ = fmt.Fprintln(out, "no secrets are stored")
		}
		return nil
	})
}
