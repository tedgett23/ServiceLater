package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

const groupUsage = `usage: servicelater group <command>

  list                                  every group and how many people are in it
  create -name N [-description D]       a new group
  members -group N                      who is in a group
  add -group N -email E                 put someone in a group
  remove -group N -email E              take someone out of a group

Groups are named, case-insensitively, by -group. The database is taken from SL_DATABASE_URL`

func runGroup(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(groupUsage)
	}
	cmd := args[0]
	fs := flag.NewFlagSet("group "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "")
	description := fs.String("description", "", "")
	group := fs.String("group", "", "")
	email := fs.String("email", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("%w\n\n%s", err, groupUsage)
	}
	need := func(flags ...string) error {
		for _, f := range flags {
			if strings.TrimSpace(fs.Lookup(f).Value.String()) == "" {
				return fmt.Errorf("%s needs -%s\n\n%s", cmd, f, groupUsage)
			}
		}
		return nil
	}
	var err error
	switch cmd {
	case "list":
	case "create":
		err = need("name")
	case "members":
		err = need("group")
	case "add", "remove":
		err = need("group", "email")
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, groupUsage)
	}
	if err != nil {
		return err
	}

	pool, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	// The operator's own tool, like `user create`: it acts as the database owner, not as a signed-in person.
	return db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		findGroup := func() (dbgen.Group, error) {
			g, err := q.GetGroupByName(ctx, *group)
			if errors.Is(err, pgx.ErrNoRows) {
				return g, fmt.Errorf("there is no group named %q", *group)
			}
			return g, err
		}
		findUser := func() (dbgen.User, error) {
			u, err := q.GetUserByEmail(ctx, *email)
			if errors.Is(err, pgx.ErrNoRows) {
				return u, fmt.Errorf("there is no user with the email %q", *email)
			}
			return u, err
		}
		switch cmd {
		case "list":
			rows, err := q.ListGroups(ctx)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				_, err = fmt.Fprintln(out, "no groups")
				return err
			}
			for _, g := range rows {
				_, _ = fmt.Fprintf(out, "%-30s %s\n", g.Name, plural(int(g.MemberCount), "member"))
			}
			return nil
		case "create":
			g, err := q.CreateGroup(ctx, dbgen.CreateGroupParams{Name: strings.TrimSpace(*name), Description: *description})
			var pe *pgconn.PgError
			switch {
			case errors.As(err, &pe) && pe.Code == "23505":
				return fmt.Errorf("a group named %q already exists", *name)
			case errors.As(err, &pe) && pe.Code == "23514":
				return errors.New("the name must be 1 to 100 characters and the description at most 1000")
			case err != nil:
				return err
			}
			_, err = fmt.Fprintf(out, "created %s\n", g.Name)
			return err
		case "members":
			g, err := findGroup()
			if err != nil {
				return err
			}
			rows, err := q.ListGroupMembers(ctx, g.ID)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				_, err = fmt.Fprintf(out, "%s has no members\n", g.Name)
				return err
			}
			for _, m := range rows {
				_, _ = fmt.Fprintf(out, "%-30s %-40s %s\n", m.Name, m.Email, m.Role)
			}
			return nil
		}
		g, err := findGroup()
		if err != nil {
			return err
		}
		u, err := findUser()
		if err != nil {
			return err
		}
		if cmd == "add" {
			if err := q.AddGroupMember(ctx, dbgen.AddGroupMemberParams{GroupID: g.ID, UserID: u.ID}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(out, "%s is in %s\n", u.Email, g.Name)
			return err
		}
		n, err := q.RemoveGroupMember(ctx, dbgen.RemoveGroupMemberParams{GroupID: g.ID, UserID: u.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%s is not in %s", u.Email, g.Name)
		}
		_, err = fmt.Fprintf(out, "%s is no longer in %s\n", u.Email, g.Name)
		return err
	})
}
