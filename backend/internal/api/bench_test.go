package api_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// The save-path benchmark from docs/apps-platform.md: what a lifecycle guard adds to a real transition request,
// through the HTTP handlers and the database. It is a test so it can reuse the same setup, and is skipped unless
// asked for:
//
//	SL_BENCH=1 SL_TEST_DATABASE_URL=... go test -run SavePath -v ./internal/api
func TestSavePathBenchmark(t *testing.T) {
	if os.Getenv("SL_BENCH") == "" {
		t.Skip("set SL_BENCH=1 to run the benchmark")
	}
	h, pool, reg := setupApps(t)
	agent := newActor(t, h, pool, "agent@x.test", "agent")
	tk := decodeBody[ticket](t, agent.do(h, "POST", "/api/v1/tickets", `{"title":"bench"}`))
	path := "/api/v1/tickets/" + tk.ID

	// A ticket that can be toggled between two states indefinitely.
	toggle := func(b *testing.B) {
		for b.Loop() {
			if w := agent.do(h, "POST", path+"/transitions", `{"to":"on_hold"}`); w.Code != 200 {
				b.Fatalf("to on_hold: %d %s", w.Code, w.Body)
			}
			if w := agent.do(h, "POST", path+"/transitions", `{"to":"in_progress"}`); w.Code != 200 {
				b.Fatalf("to in_progress: %d %s", w.Code, w.Body)
			}
		}
	}
	read := func(b *testing.B) {
		for b.Loop() {
			if w := agent.do(h, "GET", path, ""); w.Code != 200 {
				b.Fatalf("get: %d", w.Code)
			}
		}
	}
	if w := agent.do(h, "POST", path+"/transitions", `{"to":"in_progress"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}

	guard := `has(record.assignee_id) || (user.role in ["admin", "agent"] && record.priority <= 4 && now - record.created_at > duration("0s"))`
	guarded := strings.NewReplacer("__G__", guard).Replace(`kind: lifecycle
record_type: incident
initial: new
states:
  new:         { label: New,         category: open }
  in_progress: { label: In progress, category: open }
  on_hold:     { label: On hold,     category: waiting }
transitions:
  - { from: new,         to: in_progress, verb: Start }
  - { from: in_progress, to: on_hold,     verb: Hold, when: '__G__', error: no }
  - { from: on_hold,     to: in_progress, verb: Resume, when: '__G__', error: no }
`)

	per := func(r testing.BenchmarkResult, requests int) string {
		return fmt.Sprintf("%8.0f µs/request  (%d requests)", float64(r.NsPerOp())/float64(requests)/1000, r.N*requests)
	}
	noGuardWrite := testing.Benchmark(toggle)
	noGuardRead := testing.Benchmark(read)

	installApp(t, pool, reg, guarded)
	// Installing replaced the ticket's lifecycle; put the ticket where the loop expects it.
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET status = 'in_progress', state_category = 'open'`); err != nil {
		t.Fatal(err)
	}
	guardWrite := testing.Benchmark(toggle)
	guardRead := testing.Benchmark(read)

	t.Logf("transition, no guards:        %s", per(noGuardWrite, 2))
	t.Logf("transition, one guard each:   %s", per(guardWrite, 2))
	t.Logf("read ticket + moves, no guard: %s", per(noGuardRead, 1))
	t.Logf("read ticket + moves, guarded:  %s", per(guardRead, 1))
	overhead := (float64(guardWrite.NsPerOp()) - float64(noGuardWrite.NsPerOp())) / 2 / 1000
	t.Logf("guard overhead on a transition: about %.0f µs per request", overhead)
	if overhead > 1000 {
		t.Errorf("a guard adds %.0f µs to a transition; the budget is 1000 µs", overhead)
	}
}
