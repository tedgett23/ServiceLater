-- name: ListFreezeWindows :many
-- The freezes that overlap from..to, by start. Capped like the calendar.
SELECT f.id, f.name, f.reason, f.starts_at, f.ends_at, f.created_by, u.name AS created_by_name, f.created_at, f.updated_at
FROM freeze_windows f
LEFT JOIN users u ON u.id = f.created_by
WHERE f.starts_at < sqlc.arg('to')::timestamptz AND f.ends_at > sqlc.arg('from')::timestamptz
ORDER BY f.starts_at, f.name
LIMIT 201;

-- name: GetFreezeWindow :one
SELECT f.id, f.name, f.reason, f.starts_at, f.ends_at, f.created_by, u.name AS created_by_name, f.created_at, f.updated_at
FROM freeze_windows f
LEFT JOIN users u ON u.id = f.created_by
WHERE f.id = $1;

-- name: CreateFreezeWindow :one
INSERT INTO freeze_windows (name, reason, starts_at, ends_at, created_by)
VALUES ($1, $2, $3, $4, app_current_user_id())
RETURNING id;

-- name: UpdateFreezeWindow :one
UPDATE freeze_windows SET name = $2, reason = $3, starts_at = $4, ends_at = $5, updated_at = now()
WHERE id = $1
RETURNING id;

-- name: DeleteFreezeWindow :execrows
DELETE FROM freeze_windows WHERE id = $1;

-- name: LookupCount :one
-- A rule's or guard's lookup, as the acting person (see app_lookup_count).
SELECT app_lookup_count(sqlc.arg('target')::text, sqlc.arg('filters')::jsonb)::bigint AS n;

-- name: ListFreezeWindowCIs :many
-- The items each of these freezes is limited to. Every reader sees the ids; the names only those who read the CMDB.
SELECT s.freeze_id, s.ci_id, c.name, c.class, c.status
FROM freeze_window_cis s
LEFT JOIN cis c ON c.id = s.ci_id
WHERE s.freeze_id = ANY (sqlc.arg('ids')::uuid[])
ORDER BY s.freeze_id, lower(c.name) NULLS LAST, s.ci_id;

-- name: ClearFreezeWindowCIs :exec
DELETE FROM freeze_window_cis WHERE freeze_id = $1;

-- name: AddFreezeWindowCI :exec
INSERT INTO freeze_window_cis (freeze_id, ci_id) VALUES ($1, $2);

-- name: TouchFreezeWindow :exec
-- Changing only a freeze's items still changes the freeze: open calendars hear of it through the row's trigger.
UPDATE freeze_windows SET updated_at = now() WHERE id = $1;
