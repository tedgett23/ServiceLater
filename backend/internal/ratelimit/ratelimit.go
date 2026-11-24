// Package ratelimit is the API's rate limit (settings limits.*, Admin → Rate limits). Each api server counts
// requests per caller per minute in memory, so a check costs no database work, and once a second (Sync) adds what it
// counted to rate_counts and reads back what every server has counted: the limit is shared by all of them, give or
// take a second of traffic. Sync also reloads the limits, so a change reaches every server within a second.
package ratelimit

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
	"github.com/servicelater/servicelater/backend/internal/settings"
)

// Window is how long a count lasts.
const Window = time.Minute

// Limiter counts requests per caller. The zero value is not usable; use New.
type Limiter struct {
	pool *pgxpool.Pool
	// Now is the clock (tests move it).
	Now func() time.Time

	mu     sync.Mutex
	counts map[string]*count
	limits settings.Limits
	swept  time.Time
}

// count is one caller's minute: what every server had counted at the last sync, and what this one has counted since.
type count struct {
	window  time.Time
	shared  int
	pending int
}

// New makes a Limiter with the default limits; Sync (or Run) loads the configured ones.
func New(pool *pgxpool.Pool) *Limiter {
	return &Limiter{pool: pool, Now: time.Now, counts: map[string]*count{}, limits: settings.DefaultLimits}
}

// Limits are the limits as last loaded.
func (l *Limiter) Limits() settings.Limits {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limits
}

// Decision is what Allow decided, with what the RateLimit headers say.
type Decision struct {
	OK        bool
	Limit     int
	Remaining int
	// Reset is when the current window ends and the count starts again.
	Reset time.Time
}

// Allow counts a request by key against limit a minute, unless it is over: then it is not counted. A limit of 0 or
// less allows everything and counts nothing.
func (l *Limiter) Allow(key string, limit int) Decision {
	if limit <= 0 {
		return Decision{OK: true}
	}
	now := l.Now()
	w := now.Truncate(Window)
	d := Decision{Limit: limit, Reset: w.Add(Window)}
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.counts[key]
	if c == nil || !c.window.Equal(w) {
		c = &count{window: w}
		l.counts[key] = c
	}
	used := c.shared + c.pending
	if used >= limit {
		return d
	}
	c.pending++
	d.OK, d.Remaining = true, limit-used-1
	return d
}

// Sync adds what this server counted since the last sync to the shared counts, takes back the totals, forgets
// finished windows and reloads the limits. Run calls it every second.
func (l *Limiter) Sync(ctx context.Context) error {
	now := l.Now()
	current := now.Truncate(Window)
	l.mu.Lock()
	var keys []string
	var windows []pgtype.Timestamptz
	var deltas []int32
	for k, c := range l.counts {
		if c.window.Before(current) {
			delete(l.counts, k) // a finished minute: what it still had pending no longer matters
			continue
		}
		keys = append(keys, k)
		windows = append(windows, pgtype.Timestamptz{Time: c.window, Valid: true})
		deltas = append(deltas, int32(c.pending)) //nolint:gosec // at most a minute's requests
	}
	sweep := now.Sub(l.swept) >= Window
	l.mu.Unlock()

	var rows []dbgen.RateCount
	var stored []dbgen.GetSystemSettingsRow
	err := db.WithSystem(ctx, l.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var err error
		if len(keys) > 0 {
			if rows, err = q.AddRateCounts(ctx, dbgen.AddRateCountsParams{Keys: keys, Windows: windows, Deltas: deltas}); err != nil {
				return err
			}
		}
		if sweep {
			if err := q.DeleteOldRateCounts(ctx, pgtype.Timestamptz{Time: now, Valid: true}); err != nil {
				return err
			}
		}
		stored, err = q.GetSystemSettings(ctx, settings.LimitsPrefix)
		return err
	})
	if err != nil {
		return err
	}

	sent := make(map[string]int32, len(keys))
	for i, k := range keys {
		sent[k] = deltas[i]
	}
	values := map[string][]byte{}
	for _, s := range stored {
		values[s.Name] = s.Value
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range rows {
		c := l.counts[r.Key]
		if c == nil || !c.window.Equal(r.WindowStart.Time) {
			continue
		}
		c.shared = int(r.Count)
		c.pending -= int(sent[r.Key]) // what was counted while the sync ran stays pending
	}
	if sweep {
		l.swept = now
	}
	l.limits = settings.LimitsFrom(values)
	return nil
}

// Run syncs every interval until ctx ends.
func (l *Limiter) Run(ctx context.Context, every time.Duration) {
	if err := l.Sync(ctx); err != nil && ctx.Err() == nil {
		slog.Error("rate limits", "err", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.Sync(ctx); err != nil && ctx.Err() == nil {
				slog.Error("rate limits", "err", err)
			}
		}
	}
}

// Load reads the limits from the database, for checks made rarely enough to read them each time (signing in,
// webhooks, queries, SMS codes), so a change applies at once. q must be the system's (db.WithSystem).
func Load(ctx context.Context, q *dbgen.Queries) (settings.Limits, error) {
	rows, err := q.GetSystemSettings(ctx, settings.LimitsPrefix)
	if err != nil {
		return settings.DefaultLimits, err
	}
	values := map[string][]byte{}
	for _, s := range rows {
		values[s.Name] = s.Value
	}
	return settings.LimitsFrom(values), nil
}
