-- SMS and Teams deliveries (00051).

-- name: GetChannelDelivery :one
-- System only: a queued SMS or Teams post and what it is about.
SELECT d.id, d.channel, d.status, d.attempts, d.to_address, d.kind, d.notification_id, d.user_id, d.group_id,
       d.task_id, d.app, d.template, d.actor_id, n.read_at AS notification_read_at
FROM channel_deliveries d
LEFT JOIN notifications n ON n.id = d.notification_id
WHERE d.id = $1;

-- name: UpdateChannelDelivery :exec
-- System only.
UPDATE channel_deliveries SET status = $2, detail = $3, attempts = $4, updated_at = now() WHERE id = $1;

-- name: InsertChannelDelivery :one
-- System only: a test message or a verification code, recorded with how it went.
INSERT INTO channel_deliveries (channel, user_id, group_id, to_address, kind, status, detail, attempts)
VALUES ($1, $2, $3, $4, $5, $6, $7, 1)
RETURNING id;

-- name: ListChannelDeliveries :many
-- The latest SMS and Teams messages (RLS: settings:manage).
SELECT d.id, d.channel, d.to_address, d.kind, d.what, d.status, d.detail, d.attempts, d.created_at, d.updated_at,
       u.name AS user_name
FROM channel_deliveries d
LEFT JOIN users u ON u.id = d.user_id
WHERE sqlc.narg('channel')::text IS NULL OR d.channel = sqlc.narg('channel')
ORDER BY d.created_at DESC, d.id
LIMIT sqlc.arg('limit');

-- name: GetMyPhone :one
SELECT number, verified_at, sms_notifications FROM user_phones WHERE user_id = app_current_user_id();

-- name: SetMyPhone :exec
SELECT app_set_phone(sqlc.arg('number')::text, sqlc.arg('code_hash')::bytea, sqlc.arg('expires_at')::timestamptz);

-- name: VerifyMyPhone :one
SELECT app_verify_phone(sqlc.arg('code_hash')::bytea)::bool AS verified;

-- name: SetMySMSNotifications :one
SELECT coalesce(app_set_sms_notifications(sqlc.arg('on')::bool), false)::bool AS changed;

-- name: ClearMyPhone :exec
SELECT app_clear_phone();

-- name: NotifyGroupChannel :one
SELECT app_notify_group_channel(sqlc.arg('group_id')::uuid, sqlc.arg('task_id')::uuid, sqlc.arg('app')::text, sqlc.arg('template')::text)::bool AS posted;

-- name: ApprovalRecordSummary :one
-- What an approver may know of the record they are asked about: its reference and title (app_approval_task_summary).
SELECT s.reference::text AS reference, s.title::text AS title
FROM workflow_approvals wa
JOIN workflow_executions e ON e.id = wa.execution_id
CROSS JOIN LATERAL app_approval_task_summary(e.record_id) s
WHERE wa.id = $1;
