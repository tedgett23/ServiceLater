-- name: ListMyQueryRuns :many
-- The caller's most recent queries, newest first (the query page's history).
SELECT id, sql, params, outcome, error, row_count, truncated, duration_ms, started_at
FROM query_runs
WHERE user_id = app_current_user_id()
ORDER BY started_at DESC
LIMIT 50;

-- name: ListSavedViews :many
-- The saved views the caller may read (their own and the shared ones), by name.
SELECT v.name, v.description, v.sql, v.shared, v.columns, v.depends_on, v.owner_id, u.name AS owner_name,
       v.created_at, v.updated_at
FROM saved_views v
JOIN users u ON u.id = v.owner_id
ORDER BY v.name;

-- name: GetSavedView :one
SELECT v.name, v.description, v.sql, v.shared, v.columns, v.depends_on, v.owner_id, u.name AS owner_name,
       v.created_at, v.updated_at
FROM saved_views v
JOIN users u ON u.id = v.owner_id
WHERE v.name = $1;

-- name: LockSavedViews :exec
-- Saves and removals take turns, so two of them cannot together make a cycle or break a view between check and write.
SELECT pg_advisory_xact_lock(hashtext('saved_views'));

-- name: InsertSavedView :exec
INSERT INTO saved_views (name, description, sql, shared, columns, depends_on, owner_id)
VALUES ($1, $2, $3, $4, $5, $6, app_current_user_id());

-- name: UpdateSavedView :execrows
UPDATE saved_views SET description = $2, sql = $3, shared = $4, columns = $5, depends_on = $6, updated_at = now()
WHERE name = $1;

-- name: DeleteSavedView :execrows
DELETE FROM saved_views WHERE name = $1;

-- name: SavedViewDependents :many
-- The views that read this one, named only when the caller may see them.
SELECT name::text, shared::boolean, visible::boolean FROM app_saved_view_dependents(@name::text);
