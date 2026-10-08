-- name: CreateAttachment :one
INSERT INTO attachments (id, task_id, filename, content_type, size, backend, key, upload_id, part_size, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, app_current_user_id())
RETURNING *;

-- name: GetAttachment :one
-- An attachment the reader may see (an upload in flight only by its uploader).
SELECT a.*, u.name AS created_by_name FROM attachments a JOIN users u ON u.id = a.created_by WHERE a.id = $1;

-- name: ListTaskAttachments :many
-- The finished attachments of a record, in the order they arrived.
SELECT a.*, u.name AS created_by_name FROM attachments a JOIN users u ON u.id = a.created_by
WHERE a.task_id = $1 AND a.status = 'ready' ORDER BY a.ready_at, a.id;

-- name: CountMyPendingAttachments :one
SELECT count(*) FROM attachments WHERE created_by = app_current_user_id() AND status = 'pending';

-- name: FinishAttachment :one
UPDATE attachments SET status = 'ready', ready_at = now(), upload_id = ''
WHERE id = $1 AND status = 'pending' AND created_by = app_current_user_id()
RETURNING *;

-- name: DeleteAttachment :execrows
DELETE FROM attachments WHERE id = $1;

-- name: GetAttachmentSystem :one
-- System only: an attachment by id, for a signed upload or download link, which carries no session.
SELECT * FROM attachments WHERE id = $1;

-- name: ExpireAttachment :execrows
-- System only: an upload not finished in time goes (its trigger queues the removal of what was stored).
DELETE FROM attachments WHERE id = $1 AND status = 'pending';

-- name: AttachmentUsage :many
-- System only: how many finished files each backend holds, and their size.
SELECT backend, count(*) AS files, coalesce(sum(size), 0)::bigint AS bytes
FROM attachments WHERE status = 'ready' GROUP BY backend ORDER BY backend;

-- name: ClaimStorageSetting :exec
-- System only: set a setting nobody has set yet (the local folder's id, claimed by the first server to use it).
INSERT INTO settings (name, value) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING;

-- name: GetSystemSetting :one
-- System only.
SELECT value FROM settings WHERE name = $1;

-- name: DeleteSystemSetting :exec
-- System only.
DELETE FROM settings WHERE name = $1;
