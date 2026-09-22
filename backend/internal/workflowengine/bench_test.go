package workflowengine_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/jobs"
	"github.com/servicelater/servicelater/backend/internal/records"
	"github.com/servicelater/servicelater/backend/internal/workflow"
	"github.com/servicelater/servicelater/backend/internal/workflowengine"
)

// The load harness of docs/apps-platform.md §7.5: how many executions per second a worker pool finishes, how long a
// step takes, and how long an execution waits before its first step. It is what the default pool size is chosen from.
//
//	SL_BENCH=1 go test -run WorkflowThroughput -v ./internal/workflowengine
//	SL_BENCH=1 SL_BENCH_N=2000 SL_BENCH_POOLS=1,2,4,8,16 go test -run WorkflowThroughput -v ./internal/workflowengine
//
// Each execution runs a realistic five-step flow on a ticket of its own: it sets a variable, branches, writes an
// internal comment, edits the ticket through the shared save path (before and after rules, audit, queuing) and ends.
func TestWorkflowThroughput(t *testing.T) {
	if os.Getenv("SL_BENCH") == "" {
		t.Skip("set SL_BENCH=1 to run the load harness")
	}
	n := envInt("SL_BENCH_N", 400)
	pools := []int{1, 2, 4, 8}
	if v := os.Getenv("SL_BENCH_POOLS"); v != "" {
		pools = nil
		for _, f := range splitCSV(v) {
			pools = append(pools, envAtoi(t, f))
		}
	}
	const flow = `  tag:
    type: set
    vars: { level: '=record.priority' }
    next: pick
  pick:
    type: condition
    if: 'vars.level > 2'
    then: note
    else: done
  note:
    type: comment.add
    body: '="seen at " + string(vars.level)'
    internal: true
    next: bump
  bump:
    type: record.update
    set: { priority: 2 }
    next: done
  done:
    type: end
`
	fmt.Printf("\n%d executions of a 5-step workflow per run, one ticket each\n", n)
	fmt.Printf("%6s %10s %12s %12s %12s %12s %12s\n", "pool", "wall", "exec/s", "step p50", "step p99", "lag p50", "lag p99")
	for _, pool := range pools {
		w := setup(t)
		w.install(t, "bench", map[string]string{"flow": wf("flow", "tag", flow)}, nil)
		ids := w.startMany(t, n)

		// A worker like the server's: its own small pool of connections, as many executors as the setting says.
		wp, err := db.ConnectMax(context.Background(), os.Getenv("SL_TEST_DATABASE_URL"), int32(pool)+2) //nolint:gosec // small
		if err != nil {
			t.Fatal(err)
		}
		engine := workflowengine.New(wp, &records.Service{Apps: w.reg, Pool: wp})
		worker := jobs.NewWorker(wp, jobs.Options{Concurrency: pool, PollInterval: 20 * time.Millisecond, Grace: 5 * time.Second})
		worker.Register(workflow.RunKind, engine.Handler())
		ctx, cancel := context.WithCancel(context.Background())
		stopped := make(chan struct{})
		began := time.Now()
		go func() { worker.Run(ctx); close(stopped) }()

		deadline := time.Now().Add(3 * time.Minute)
		for w.count(t, `SELECT count(*) FROM workflow_executions WHERE status NOT IN ('done','failed')`) > 0 {
			if time.Now().After(deadline) {
				t.Fatalf("pool %d: not finished after 3 minutes", pool)
			}
			time.Sleep(20 * time.Millisecond)
		}
		wall := time.Since(began)
		cancel()
		<-stopped
		wp.Close()

		if failed := w.count(t, `SELECT count(*) FROM workflow_executions WHERE status = 'failed'`); failed != 0 {
			t.Fatalf("pool %d: %d executions failed", pool, failed)
		}
		if done := w.count(t, `SELECT count(*) FROM workflow_executions WHERE status = 'done'`); done != len(ids) {
			t.Fatalf("pool %d: %d done, want %d", pool, done, len(ids))
		}
		stepMs := w.floats(t, `SELECT duration_ms::float8 FROM workflow_steps ORDER BY 1`)
		// Queue lag: from an execution being started to its first step being recorded.
		lagMs := w.floats(t, `SELECT extract(epoch FROM (min_step - e.created_at)) * 1000 FROM workflow_executions e
			JOIN (SELECT execution_id, min(created_at) AS min_step FROM workflow_steps GROUP BY 1) s ON s.execution_id = e.id ORDER BY 1`)
		fmt.Printf("%6d %10s %12.0f %10.0fms %10.0fms %10.0fms %10.0fms\n", pool, wall.Round(time.Millisecond), float64(n)/wall.Seconds(),
			pct(stepMs, 50), pct(stepMs, 99), pct(lagMs, 50), pct(lagMs, 99))
	}
	fmt.Println("step latency is the time to perform one step, not counting its commit; lag includes waiting for a free executor, so it grows with the backlog")
}

// startMany starts n executions, each on a ticket of its own, in batches inside transactions, as rules would.
func (w *world) startMany(t *testing.T, n int) []uuid.UUID {
	t.Helper()
	def := w.reg.Workflow("bench", "flow")
	ctx := context.Background()
	var ids []uuid.UUID
	for len(ids) < n {
		batch := min(100, n-len(ids))
		if err := db.WithSystem(ctx, w.pool, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			for i := 0; i < batch; i++ {
				task, err := q.CreateTask(ctx, dbgen.CreateTaskParams{Type: "incident", Title: fmt.Sprintf("bench %d", len(ids)+i), Priority: 3, RequesterID: w.requester, Status: "new", StateCategory: "open"})
				if err != nil {
					return err
				}
				if err := q.CreateIncident(ctx, task.ID); err != nil {
					return err
				}
				id, err := workflow.Start(ctx, q, workflow.StartParams{Def: def, RecordID: &task.ID, UserID: w.agent})
				if err != nil {
					return err
				}
				ids = append(ids, id)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func (w *world) floats(t *testing.T, sql string) []float64 {
	t.Helper()
	rows, err := w.pool.Query(context.Background(), sql)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var f float64
		if err := rows.Scan(&f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	sort.Float64s(out)
	return out
}

func pct(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, len(sorted)*p/100)]
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envAtoi(t *testing.T, s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		t.Fatalf("%q is not a positive number", s)
	}
	return n
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}
