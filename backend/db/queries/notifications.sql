-- name: ListMyNotifications :many
SELECT * FROM notifications WHERE user_id = sqlc.arg('user_id') ORDER BY created_at DESC LIMIT sqlc.arg('limit');

-- name: CountMyUnreadNotifications :one
SELECT count(*) FROM notifications WHERE user_id = sqlc.arg('user_id') AND read_at IS NULL;

-- Only an unread row of this person's own matches, so marking someone else's, or marking twice, finds nothing (RLS
-- on top of this already keeps it to the recipient's own rows).
-- name: MarkNotificationRead :one
UPDATE notifications SET read_at = now() WHERE id = sqlc.arg('id') AND read_at IS NULL RETURNING *;

-- Deliver a template notification about a ticket; app_notify (00009_notifications) decides who actually gets one.
-- name: SendNotification :one
SELECT app_notify(sqlc.arg('user_ids')::uuid[], sqlc.arg('task_id')::uuid, sqlc.arg('app')::text, sqlc.arg('template')::text)::int AS sent;

-- name: SendCommentNotification :one
SELECT app_notify_comment(sqlc.arg('user_ids')::uuid[], sqlc.arg('task_id')::uuid, sqlc.arg('comment_id')::uuid,
    sqlc.arg('app')::text, sqlc.arg('template')::text)::int AS sent;

-- Opening a ticket answers what this person was told about it.
-- name: MarkTaskNotificationsRead :execrows
UPDATE notifications SET read_at = now()
WHERE user_id = sqlc.arg('user_id') AND task_id = sqlc.arg('task_id') AND read_at IS NULL;
