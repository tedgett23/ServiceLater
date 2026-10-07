-- name: CreateUser :one
INSERT INTO users (email, name, role)
VALUES (lower(sqlc.arg(email)::text), $1, $2)
RETURNING id, email, name, role, created_at, deactivated_at;

-- name: GetUser :one
SELECT id, email, name, role, created_at, deactivated_at FROM users WHERE id = $1;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- Who can be assigned a ticket: active agents and admins.
-- name: ListStaff :many
SELECT id, email, name, role, created_at, deactivated_at FROM users
WHERE role IN ('admin', 'agent') AND deactivated_at IS NULL
ORDER BY name;

-- The directory, for pickers such as group membership. q matches a name or email. Deactivated people are left out
-- unless asked for (the admin's user list).
-- name: ListUsers :many
SELECT id, email, name, role, created_at, deactivated_at FROM users
WHERE (sqlc.narg('q')::text IS NULL OR name ILIKE '%' || sqlc.narg('q') || '%' ESCAPE '\' OR email ILIKE '%' || sqlc.narg('q') || '%' ESCAPE '\')
  AND (sqlc.narg('role')::text IS NULL OR role = sqlc.narg('role'))
  AND (sqlc.arg('include_deactivated')::bool OR deactivated_at IS NULL)
ORDER BY name, id
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: GetUserByEmail :one
SELECT id, email, name, role, created_at, deactivated_at FROM users WHERE email = lower(sqlc.arg('email')::text);

-- An admin's edit. Deactivating keeps the first time it happened; reactivating clears it.
-- name: UpdateUser :one
UPDATE users SET
    email = COALESCE(lower(sqlc.narg('email')::text), email),
    name  = COALESCE(sqlc.narg('name'), name),
    role  = COALESCE(sqlc.narg('role'), role),
    deactivated_at = CASE
        WHEN sqlc.narg('deactivated')::bool IS NULL THEN deactivated_at
        WHEN sqlc.narg('deactivated')::bool THEN COALESCE(deactivated_at, now())
        ELSE NULL END
WHERE id = sqlc.arg('id')
RETURNING id, email, name, role, created_at, deactivated_at;

-- Only someone with no history can be deleted: tickets, comments and approvals keep a reference to them.
-- name: DeleteUser :execrows
DELETE FROM users WHERE id = $1;

-- Through SECURITY DEFINER functions (00017): credentials and sessions are owner-only tables.
-- name: SetUserPassword :exec
SELECT app_set_password(sqlc.arg('user_id')::uuid, sqlc.arg('password_hash')::text);

-- name: RevokeUserSessions :exec
SELECT app_revoke_sessions(sqlc.arg('user_id')::uuid);
