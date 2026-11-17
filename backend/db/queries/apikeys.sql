-- name: CreateServiceAccount :exec
INSERT INTO service_accounts (user_id, description, created_by) VALUES ($1, $2, app_current_user_id());

-- name: GetServiceAccount :one
SELECT user_id, description, created_by, created_at FROM service_accounts WHERE user_id = $1;

-- name: ListServiceAccountIDs :many
SELECT user_id FROM service_accounts;

-- name: CreateAPIKey :one
SELECT app_create_api_key(sqlc.arg('user_id')::uuid, sqlc.arg('name')::text, sqlc.arg('prefix')::text,
    sqlc.arg('secret_hash')::bytea, sqlc.narg('scopes')::text[], sqlc.narg('expires_at')::timestamptz)::uuid AS id;

-- An account's keys, live ones first (RLS: users:manage).
-- name: ListAPIKeys :many
SELECT k.id, k.name, k.prefix, k.scopes, k.created_at, c.name AS created_by_name, k.expires_at, k.last_used_at, k.revoked_at
FROM api_keys k LEFT JOIN users c ON c.id = k.created_by
WHERE k.user_id = $1
ORDER BY k.revoked_at IS NOT NULL, k.created_at DESC;

-- name: RevokeAPIKey :one
SELECT app_revoke_api_key(sqlc.arg('user_id')::uuid, sqlc.arg('id')::uuid)::bool AS revoked;

-- What a request's key is good for, read as the system (owner role) before anyone is known: nothing when it is
-- revoked, expired, or its account is deactivated or no longer a service account.
-- name: GetAPIKeyForAuth :one
SELECT k.id, k.user_id, k.scopes, k.expires_at, u.role
FROM api_keys k
JOIN users u ON u.id = k.user_id
JOIN service_accounts s ON s.user_id = u.id
WHERE k.secret_hash = $1 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > now())
  AND u.deactivated_at IS NULL;

-- At most one write a minute per key, so a busy integration does not turn every read into a write.
-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now() WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: NarrowPermissions :one
SELECT app_narrow_permissions(sqlc.arg('perms')::text[], sqlc.arg('scopes')::text[])::text[] AS perms;
