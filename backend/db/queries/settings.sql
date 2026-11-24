-- name: ListSettings :many
-- The settings whose names start with prefix (email. for the mail server).
SELECT name, value, updated_at FROM settings WHERE name LIKE sqlc.arg('prefix')::text || '%' ORDER BY name;

-- name: UpsertSetting :exec
INSERT INTO settings (name, value, updated_by) VALUES ($1, $2, app_current_user_id())
ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now();

-- name: DeleteSetting :exec
DELETE FROM settings WHERE name = $1;

-- name: SetSecret :exec
SELECT app_set_secret(sqlc.arg('name')::text, sqlc.arg('key_id')::text, sqlc.arg('nonce')::bytea, sqlc.arg('ciphertext')::bytea);

-- name: ClearSecret :exec
SELECT app_clear_secret(sqlc.arg('name')::text);

-- name: SecretStatus :many
-- updated_by_name is empty when that person was deleted.
SELECT name::text AS name, key_id::text AS key_id, updated_at::timestamptz AS updated_at, updated_by_name::text AS updated_by_name
FROM app_secret_status();

-- name: GetSecret :one
-- System only (db.WithSystem): the app role cannot read secrets.
SELECT name, key_id, nonce, ciphertext FROM secrets WHERE name = $1;

-- name: ListSecrets :many
-- System only, for key rotation.
SELECT name, key_id, nonce, ciphertext FROM secrets ORDER BY name FOR UPDATE;

-- name: ResealSecret :exec
-- System only, for key rotation: the same value under another key.
UPDATE secrets SET key_id = $2, nonce = $3, ciphertext = $4 WHERE name = $1;

-- name: GetSystemSettings :many
-- System only: settings for the worker, which acts for nobody.
SELECT name, value FROM settings WHERE name LIKE sqlc.arg('prefix')::text || '%';

-- name: GetEmailDelivery :one
-- System only: a queued email, its notification, and who it is for.
SELECT d.id, d.status, d.attempts, d.to_address, d.kind, d.notification_id, d.user_id, u.name AS user_name,
       n.kind AS notification_kind, n.read_at AS notification_read_at
FROM email_deliveries d
LEFT JOIN users u ON u.id = d.user_id
LEFT JOIN notifications n ON n.id = d.notification_id
WHERE d.id = $1;

-- name: UpdateEmailDelivery :exec
-- System only.
UPDATE email_deliveries SET status = $2, detail = $3, attempts = $4, updated_at = now() WHERE id = $1;

-- name: InsertEmailDelivery :one
-- System only: a test email, recorded with how it went.
INSERT INTO email_deliveries (user_id, to_address, kind, status, detail, attempts)
VALUES ($1, $2, 'test', $3, $4, 1)
RETURNING id;

-- name: ListEmailDeliveries :many
SELECT d.id, d.to_address, d.kind, d.what, d.status, d.detail, d.attempts, d.created_at, d.updated_at, u.name AS user_name
FROM email_deliveries d
LEFT JOIN users u ON u.id = d.user_id
ORDER BY d.created_at DESC, d.id
LIMIT sqlc.arg('limit');

-- name: GetNotificationForEmail :one
-- The notification an email is about, read as its recipient (their RLS).
SELECT * FROM notifications WHERE id = $1;

-- name: SetMyEmailNotifications :one
UPDATE users SET email_notifications = $2 WHERE id = $1 AND id = app_current_user_id()
RETURNING email_notifications;

-- name: SetMyTheme :one
UPDATE users SET theme = $2 WHERE id = $1 AND id = app_current_user_id()
RETURNING theme;

-- name: AddRateCounts :many
-- Adds what one server counted since it last did (one row per caller and minute) and returns the totals every
-- server has counted (internal/ratelimit). A delta of 0 only reads.
INSERT INTO rate_counts (key, window_start, count)
SELECT unnest(sqlc.arg('keys')::text[]), unnest(sqlc.arg('windows')::timestamptz[]), unnest(sqlc.arg('deltas')::int[])
ON CONFLICT (key, window_start) DO UPDATE SET count = rate_counts.count + EXCLUDED.count
RETURNING key, window_start, count;

-- name: DeleteOldRateCounts :exec
-- Windows that ended long before now, by the limiter's clock (its windows are by its clock too).
DELETE FROM rate_counts WHERE window_start < sqlc.arg('now')::timestamptz - interval '5 minutes';
