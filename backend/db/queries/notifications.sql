-- name: ListMyNotifications :many
SELECT * FROM notifications WHERE user_id = sqlc.arg('user_id') ORDER BY created_at DESC LIMIT sqlc.arg('limit');

-- name: CountMyUnreadNotifications :one
SELECT count(*) FROM notifications WHERE user_id = sqlc.arg('user_id') AND read_at IS NULL;

-- Only an unread row of this person's own matches, so marking someone else's, or marking twice, finds nothing (RLS
-- on top of this already keeps it to the recipient's own rows).
-- name: MarkNotificationRead :one
UPDATE notifications SET read_at = now() WHERE id = sqlc.arg('id') AND read_at IS NULL RETURNING *;
