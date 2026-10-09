-- name: CreateUser :one
INSERT INTO users (email, name, role)
VALUES (lower(sqlc.arg(email)::text), $1, $2)
RETURNING id, email, name, role, created_at, deactivated_at, app_name, email_notifications, theme, manager_id;

-- name: GetUser :one
SELECT id, email, name, role, created_at, deactivated_at, app_name, email_notifications, theme, manager_id FROM users WHERE id = $1;

-- People, not app principals: the bootstrap admin is created when there are none.
-- name: CountUsers :one
SELECT count(*) FROM users WHERE app_name IS NULL;

-- Who can be assigned a ticket: everyone active who may work records of its type (records:update:<type>), whatever
-- role gives it; with no type, everyone who may work some type.
-- name: ListStaff :many
SELECT id, email, name, role, created_at, deactivated_at, app_name, email_notifications, theme, manager_id FROM users
WHERE deactivated_at IS NULL AND app_name IS NULL
  AND (CASE WHEN sqlc.narg('record_type')::text IS NULL THEN app_user_can_record_any(id, 'update')
            ELSE app_user_can_record(id, 'update', sqlc.narg('record_type')) END)
ORDER BY name;

-- The directory, for pickers such as group membership. q matches a name or email. Deactivated people are left out
-- unless asked for (the admin's user list).
-- name: ListUsers :many
SELECT id, email, name, role, created_at, deactivated_at, app_name, email_notifications, theme, manager_id FROM users
WHERE app_name IS NULL
  AND (sqlc.narg('q')::text IS NULL OR name ILIKE '%' || sqlc.narg('q') || '%' ESCAPE '\' OR email ILIKE '%' || sqlc.narg('q') || '%' ESCAPE '\')
  AND (sqlc.narg('role')::text IS NULL OR role = sqlc.narg('role'))
  AND (sqlc.arg('include_deactivated')::bool OR deactivated_at IS NULL)
ORDER BY name, id
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: GetUserByEmail :one
SELECT id, email, name, role, created_at, deactivated_at, app_name, email_notifications, theme, manager_id FROM users WHERE email = lower(sqlc.arg('email')::text);

-- An admin's edit. Deactivating keeps the first time it happened; reactivating clears it.
-- name: UpdateUser :one
UPDATE users SET
    email = COALESCE(lower(sqlc.narg('email')::text), email),
    name  = COALESCE(sqlc.narg('name'), name),
    role  = COALESCE(sqlc.narg('role'), role),
    deactivated_at = CASE
        WHEN sqlc.narg('deactivated')::bool IS NULL THEN deactivated_at
        WHEN sqlc.narg('deactivated')::bool THEN COALESCE(deactivated_at, now())
        ELSE NULL END,
    manager_id = CASE WHEN sqlc.arg('set_manager')::bool THEN sqlc.narg('manager_id')::uuid ELSE manager_id END
WHERE id = sqlc.arg('id')
RETURNING id, email, name, role, created_at, deactivated_at, app_name, email_notifications, theme, manager_id;

-- Only someone with no history can be deleted: tickets, comments and approvals keep a reference to them.
-- name: DeleteUser :execrows
DELETE FROM users WHERE id = $1;

-- Through SECURITY DEFINER functions (00017): credentials and sessions are owner-only tables.
-- name: SetUserPassword :exec
SELECT app_set_password(sqlc.arg('user_id')::uuid, sqlc.arg('password_hash')::text);

-- name: RevokeUserSessions :exec
SELECT app_revoke_sessions(sqlc.arg('user_id')::uuid);

-- name: PermissionsOf :one
SELECT app_permissions_of(sqlc.arg('user_id')::uuid)::text[] AS permissions;

-- What the transaction's own person may do, as app_begin loaded it.
-- name: CurrentPermissions :one
SELECT coalesce(current_setting('app.permissions', true), '')::text AS permissions;

-- name: UserCanRecord :one
SELECT app_user_can_record(sqlc.arg('user_id')::uuid, sqlc.arg('verb')::text, sqlc.arg('record_type')::text)::bool AS can;

-- name: UserCan :one
SELECT app_user_can(sqlc.arg('user_id')::uuid, sqlc.arg('permission')::text)::bool AS can;

-- An app's principal, created on first use.
-- name: AppPrincipal :one
SELECT app_principal(sqlc.arg('app')::text)::uuid AS id;

-- Installing an app brings its principal back; uninstalling deactivates it (it stays for the history it wrote).
-- name: SetAppPrincipalActive :exec
UPDATE users SET deactivated_at = CASE WHEN sqlc.arg('active')::bool THEN NULL ELSE coalesce(deactivated_at, now()) END
WHERE app_name = sqlc.arg('app');

-- The person an app principal is acting for in this transaction, if any.
-- name: CurrentOnBehalfOf :one
SELECT app_on_behalf_of()::uuid AS id;

-- Switching who a transaction acts as, for an after rule that runs as its app (run_as: app) and back.
-- name: BeginAs :exec
SELECT app_begin_as(sqlc.arg('principal')::uuid, sqlc.arg('on_behalf_of')::uuid);

-- name: BeginAsUser :exec
SELECT app_begin(sqlc.arg('user_id')::uuid);
