package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func jobsCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runJobs(context.Background(), args, &out)
	return out.String(), err
}

func TestJobsCommand(t *testing.T) {
	scratch(t)
	// The database is empty, then holds one pending job and one dead-lettered job.
	if out, err := jobsCLI(t, "list"); err != nil || !strings.Contains(out, "no jobs") {
		t.Fatalf("empty list: %v\n%s", err, out)
	}
	pool := dbPool(t)
	if _, err := pool.Exec(context.Background(), `INSERT INTO jobs (kind) VALUES ('waiting.job')`); err != nil {
		t.Fatal(err)
	}
	var dead int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO jobs (kind, state, attempts, last_error, finished_at)
		VALUES ('broken.job', 'failed', 5, E'it went\nwrong   badly', now()) RETURNING id`).Scan(&dead); err != nil {
		t.Fatal(err)
	}

	out, err := jobsCLI(t, "stats")
	if err != nil || !strings.Contains(out, "pending  1") || !strings.Contains(out, "failed   1") {
		t.Errorf("stats: %v\n%s", err, out)
	}
	out, _ = jobsCLI(t, "list", "-state", "failed")
	if !strings.Contains(out, "broken.job") || strings.Contains(out, "waiting.job") || !strings.Contains(out, "it went wrong badly") || !strings.Contains(out, "5/5") {
		t.Errorf("the dead letter, one error per line:\n%s", out)
	}
	out, _ = jobsCLI(t, "list")
	if !strings.Contains(out, "waiting.job") || !strings.Contains(out, "broken.job") {
		t.Errorf("all jobs:\n%s", out)
	}

	out, err = jobsCLI(t, "retry", itoa(dead))
	if err != nil || !strings.Contains(out, "will run again") {
		t.Fatalf("retry: %v\n%s", err, out)
	}
	if _, err := jobsCLI(t, "retry", itoa(dead)); err == nil || !strings.Contains(err.Error(), "not in the dead letter") {
		t.Errorf("retrying a job that is no longer dead-lettered: %v", err)
	}
	out, _ = jobsCLI(t, "stats")
	if !strings.Contains(out, "failed   0") || !strings.Contains(out, "pending  2") {
		t.Errorf("after retry:\n%s", out)
	}
}

func TestJobsUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}, {"retry"}, {"retry", "abc"}, {"list", "-n", "0"}} {
		if _, err := jobsCLI(t, args...); err == nil {
			t.Errorf("%v should be an error", args)
		}
	}
}

func TestJobsRunsListsTheRuleAudit(t *testing.T) {
	scratch(t)
	if out, err := jobsCLI(t, "runs"); err != nil || !strings.Contains(out, "no rule runs") {
		t.Fatalf("empty: %q, %v", out, err)
	}
	pool := dbPool(t)
	if _, err := pool.Exec(context.Background(), `INSERT INTO action_runs (app, rule, timing, event, outcome, detail) VALUES
		('house', 'no-spam', 'after', 'create', 'aborted', 'No   spam.'), ('house', 'note', 'async', 'update', 'ok', '')`); err != nil {
		t.Fatal(err)
	}
	out, err := jobsCLI(t, "runs")
	if err != nil || !strings.Contains(out, "house/no-spam (create)") || !strings.Contains(out, "No spam.") || !strings.Contains(out, "house/note") {
		t.Errorf("all:\n%s (%v)", out, err)
	}
	if out, _ = jobsCLI(t, "runs", "-outcome", "aborted"); strings.Contains(out, "house/note") || !strings.Contains(out, "no-spam") {
		t.Errorf("aborted only:\n%s", out)
	}
	if _, err := jobsCLI(t, "runs", "-n", "0"); err == nil {
		t.Error("-n 0 should be refused")
	}
}
