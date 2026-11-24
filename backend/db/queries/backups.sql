-- Snapshots and restores (00021_backups, internal/backup). Written as the system; read by instance:backup holders.

-- name: InsertSnapshot :one
INSERT INTO snapshots (kind, destination, include_attachments, scheduled_for, requested_by, requested_by_name)
VALUES ($1, $2, $3, sqlc.narg('scheduled_for'), sqlc.narg('requested_by'), sqlc.arg('requested_by_name'))
RETURNING *;

-- name: InsertUploadedSnapshot :one
-- A snapshot someone uploaded (from another instance, to restore here): ready as soon as it is stored and read.
INSERT INTO snapshots (kind, status, destination, key, size, include_attachments, taken_at, schema_version, key_id, instance,
                       rows, attachment_files, requested_by, requested_by_name, finished_at)
VALUES ('uploaded', 'ready', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, sqlc.narg('requested_by'), sqlc.arg('requested_by_name'), now())
RETURNING *;

-- name: ClaimSnapshot :one
-- The oldest snapshot waiting to be taken, for one worker.
UPDATE snapshots SET status = 'running', heartbeat_at = now()
WHERE id = (SELECT id FROM snapshots WHERE status = 'pending' ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING *;

-- name: HeartbeatSnapshot :exec
UPDATE snapshots SET heartbeat_at = now() WHERE id = $1 AND status = 'running';

-- name: FinishSnapshot :exec
UPDATE snapshots SET status = 'ready', key = $2, size = $3, taken_at = $4, schema_version = $5, key_id = $6, instance = $7,
    rows = $8, attachment_files = $9, finished_at = now(), heartbeat_at = NULL
WHERE id = $1;

-- name: FailSnapshot :exec
UPDATE snapshots SET status = 'failed', error = $2, finished_at = now(), heartbeat_at = NULL WHERE id = $1;

-- name: FailStaleSnapshots :exec
-- A snapshot whose worker stopped saying it is alive (it was stopped or died) failed.
UPDATE snapshots SET status = 'failed', error = 'the worker taking it stopped', finished_at = now(), heartbeat_at = NULL
WHERE status = 'running' AND heartbeat_at < now() - interval '1 minute';

-- name: ListSnapshots :many
SELECT * FROM snapshots ORDER BY created_at DESC LIMIT 200;

-- name: GetSnapshot :one
SELECT * FROM snapshots WHERE id = $1;

-- name: DeleteSnapshot :exec
DELETE FROM snapshots WHERE id = $1;

-- name: LastScheduledSnapshot :one
-- The latest due time the schedule queued a snapshot for (by the worker's clock, as the due time is worked out).
SELECT coalesce(max(scheduled_for), '-infinity'::timestamptz)::timestamptz AS at FROM snapshots WHERE kind = 'scheduled';

-- name: ScheduledSnapshots :many
-- The scheduled snapshots that are ready, newest first: what retention chooses from.
SELECT id, destination, key, taken_at FROM snapshots WHERE kind = 'scheduled' AND status = 'ready' ORDER BY taken_at DESC;

-- name: InsertRestore :one
INSERT INTO snapshot_restores (snapshot_id, change_schema, requested_by, requested_by_name)
VALUES ($1, $2, sqlc.narg('requested_by'), sqlc.arg('requested_by_name'))
RETURNING *;

-- name: ClaimRestore :one
UPDATE snapshot_restores SET status = 'running', started_at = now(), heartbeat_at = now()
WHERE id = (SELECT id FROM snapshot_restores WHERE status = 'pending' ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING *;

-- name: HeartbeatRestore :exec
UPDATE snapshot_restores SET heartbeat_at = now() WHERE id = $1 AND status = 'running';

-- name: FinishRestore :exec
UPDATE snapshot_restores SET status = $2, error = $3, finished_at = now(), heartbeat_at = NULL WHERE id = $1;

-- name: FailStaleRestores :exec
UPDATE snapshot_restores SET status = 'failed', error = 'the worker restoring it stopped; nothing was changed', finished_at = now(), heartbeat_at = NULL
WHERE status = 'running' AND heartbeat_at < now() - interval '1 minute';

-- name: FailUnclaimedRestores :exec
-- A restore no worker took within two minutes (none is running, or none can reach the snapshot) is given up, so the
-- instance does not stay in maintenance.
UPDATE snapshot_restores SET status = 'failed', error = 'no worker took it within two minutes: is a worker running?', finished_at = now()
WHERE status = 'pending' AND created_at < now() - interval '2 minutes';

-- name: ListRestores :many
SELECT r.*, s.taken_at AS snapshot_taken_at FROM snapshot_restores r JOIN snapshots s ON s.id = r.snapshot_id
ORDER BY r.created_at DESC LIMIT 20;

-- name: ActiveRestore :one
-- The restore waiting or running, if any: while there is one the instance is in maintenance.
SELECT r.id, r.status, r.created_at, r.started_at, r.requested_by_name
FROM snapshot_restores r
WHERE (r.status = 'pending' AND r.created_at > now() - interval '2 minutes')
   OR (r.status = 'running' AND r.heartbeat_at > now() - interval '1 minute')
ORDER BY r.created_at LIMIT 1;
