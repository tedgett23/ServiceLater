package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

const jobsUsage = `usage: servicelater jobs <command>

  stats                          how many jobs are in each state
  list [-state failed] [-n 20]   recent jobs, newest first; "failed" is the dead letter
  retry <id>...                  put dead-lettered jobs back in the queue
  runs [-outcome failed] [-n 20] what rules did: every async run, and every rule that stopped a save or broke

The database is taken from SL_DATABASE_URL`

func runJobs(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(jobsUsage)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "stats", "list", "retry", "runs":
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, jobsUsage)
	}
	pool, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch cmd {
	case "stats":
		return db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
			rows, err := dbgen.New(tx).CountJobsByState(ctx)
			if err != nil {
				return err
			}
			counts := map[string]int64{}
			for _, r := range rows {
				counts[r.State] = r.N
			}
			for _, s := range []string{"pending", "running", "done", "failed"} {
				_, _ = fmt.Fprintf(out, "%-8s %d\n", s, counts[s])
			}
			return nil
		})
	case "list":
		fs := flag.NewFlagSet("list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		state := fs.String("state", "", "")
		n := fs.Int("n", 20, "")
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || *n < 1 {
			return errors.New("usage: servicelater jobs list [-state pending|running|done|failed] [-n 20]")
		}
		arg := dbgen.ListJobsParams{Limit: int32(min(*n, 1000))} //nolint:gosec // bounded just above
		if *state != "" {
			arg.State = pgtype.Text{String: *state, Valid: true}
		}
		return db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
			rows, err := dbgen.New(tx).ListJobs(ctx, arg)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no jobs")
			}
			for _, j := range rows {
				line := fmt.Sprintf("%-6d %-24s %-8s attempt %d/%d", j.ID, j.Kind, j.State, j.Attempts, j.MaxAttempts)
				if j.LastError.Valid && j.LastError.String != "" {
					line += "  " + oneLine(j.LastError.String, 100)
				}
				_, _ = fmt.Fprintln(out, line)
			}
			return nil
		})
	case "runs":
		fs := flag.NewFlagSet("runs", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		outcome := fs.String("outcome", "", "")
		n := fs.Int("n", 20, "")
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || *n < 1 {
			return errors.New("usage: servicelater jobs runs [-outcome ok|aborted|failed|skipped] [-n 20]")
		}
		arg := dbgen.ListActionRunsParams{Limit: int32(min(*n, 1000))} //nolint:gosec // bounded just above
		if *outcome != "" {
			arg.Outcome = pgtype.Text{String: *outcome, Valid: true}
		}
		return db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
			rows, err := dbgen.New(tx).ListActionRuns(ctx, arg)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no rule runs")
			}
			for _, r := range rows {
				line := fmt.Sprintf("%s  %-8s %-7s %s/%s (%s)", r.CreatedAt.Time.Format("2006-01-02 15:04:05"), r.Outcome, r.Timing, r.App, r.Rule, r.Event)
				if r.RecordID.Valid {
					line += " " + r.RecordID.UUID.String()[:8]
				}
				if r.Detail != "" {
					line += "  " + oneLine(r.Detail, 100)
				}
				_, _ = fmt.Fprintln(out, line)
			}
			return nil
		})
	default: // retry
		if len(rest) == 0 {
			return errors.New("usage: servicelater jobs retry <id> [<id> ...]")
		}
		for _, a := range rest {
			id, err := strconv.ParseInt(a, 10, 64)
			if err != nil {
				return fmt.Errorf("%q is not a job id", a)
			}
			var n int64
			if err := db.WithSystem(ctx, pool, func(tx pgx.Tx) error {
				var err error
				n, err = dbgen.New(tx).RetryJob(ctx, id)
				return err
			}); err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("job %d is not in the dead letter (only failed jobs can be retried)", id)
			}
			_, _ = fmt.Fprintf(out, "job %d will run again\n", id)
		}
		return nil
	}
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
