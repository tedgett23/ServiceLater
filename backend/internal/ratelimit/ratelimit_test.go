package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/servicelater/servicelater/backend/internal/dbtest"
	"github.com/servicelater/servicelater/backend/internal/ratelimit"
)

// Two servers share one limit: each counts in memory and they meet in the database once a second. A new minute
// starts the count again, and a refused request is not counted.
func TestServersShareALimit(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 24, 9, 0, 10, 0, time.UTC)
	clock := func() time.Time { return now }
	a, b := ratelimit.New(pool), ratelimit.New(pool)
	a.Now, b.Now = clock, clock

	for i := range 6 {
		if d := a.Allow("api:user:ada", 10); !d.OK || d.Remaining != 10-i-1 {
			t.Fatalf("a's request %d: %+v", i+1, d)
		}
	}
	for range 3 {
		if !b.Allow("api:user:ada", 10).OK {
			t.Fatal("b refused before it knew of a's requests")
		}
	}
	// Before they sync, each only knows its own; after, both know of all nine.
	for _, l := range []*ratelimit.Limiter{a, b, a} {
		if err := l.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if d := b.Allow("api:user:ada", 10); !d.OK || d.Remaining != 0 {
		t.Fatalf("the tenth: %+v", d)
	}
	if d := b.Allow("api:user:ada", 10); d.OK {
		t.Fatalf("the eleventh was allowed: %+v", d)
	}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if d := a.Allow("api:user:ada", 10); d.OK || d.Reset != time.Date(2026, 11, 24, 9, 1, 0, 0, time.UTC) {
		t.Fatalf("a after b's tenth: %+v", d)
	}
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count FROM rate_counts WHERE key = 'api:user:ada'`).Scan(&stored); err != nil || stored != 10 {
		t.Fatalf("stored %d (%v), want 10: refused requests are not counted", stored, err)
	}
	if !a.Allow("api:user:bob", 10).OK {
		t.Fatal("someone else shares ada's limit")
	}

	now = now.Add(time.Minute)
	if !a.Allow("api:user:ada", 10).OK || !b.Allow("api:user:ada", 10).OK {
		t.Fatal("a new minute did not start again")
	}
	if d := a.Allow("api:user:ada", 0); !d.OK || d.Limit != 0 {
		t.Fatalf("no limit: %+v", d)
	}
}

// Sync loads the limits, so a change reaches every server within a second; anything stored out of range is ignored.
func TestSyncLoadsTheLimits(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	l := ratelimit.New(pool)
	if l.Limits().APIPeoplePerMinute != 1200 {
		t.Fatalf("the default: %+v", l.Limits())
	}
	if _, err := pool.Exec(ctx, `INSERT INTO settings (name, value) VALUES ('limits.api_people_per_minute', '50'), ('limits.live_connections', '0')`); err != nil {
		t.Fatal(err)
	}
	if err := l.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := l.Limits(); got.APIPeoplePerMinute != 50 || got.LiveConnections != 10 {
		t.Errorf("after sync: %+v", got)
	}
}
