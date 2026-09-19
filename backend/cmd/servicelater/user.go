package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/config"
	"github.com/servicelater/servicelater/backend/internal/db"
)

const userUsage = `usage: servicelater user create -email E -name N [-role admin|agent|end_user] [-password-stdin]

The password is read from the SL_USER_PASSWORD environment variable, or from stdin with -password-stdin.
It is never taken as a flag, because command-line arguments are visible to other users of the machine.
The database is taken from SL_DATABASE_URL`

func runUser(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New(userUsage)
	}
	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	email := fs.String("email", "", "")
	name := fs.String("name", "", "")
	role := fs.String("role", "end_user", "")
	fromStdin := fs.Bool("password-stdin", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("%w\n\n%s", err, userUsage)
	}

	password := os.Getenv("SL_USER_PASSWORD")
	if *fromStdin {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && line == "" {
			return errors.New("no password on stdin")
		}
		password = strings.TrimRight(line, "\r\n")
	}
	if password == "" {
		return errors.New("set SL_USER_PASSWORD or use -password-stdin\n\n" + userUsage)
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, config.DatabaseURL())
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	u, err := auth.NewService(pool, nil, 0).CreateUser(ctx, *email, *name, *role, password)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "created %s (%s)\n", u.Email, u.Role)
	return err
}
