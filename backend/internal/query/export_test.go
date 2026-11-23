package query

import "github.com/jackc/pgx/v5/pgxpool"

// RolePool is the query role's own pool, for tests that send SQL to it without the check.
func RolePool(r *Runner) *pgxpool.Pool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pool
}
