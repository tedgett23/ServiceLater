-- Clone profiles and packages (00022_clones, internal/clone, internal/backup). Profiles are read and written by
-- instance:clone holders under RLS; packages are written as the system, and their sealed passphrase is only ever read
-- by the worker (ClaimClonePackage).

-- name: ListCloneProfiles :many
SELECT * FROM clone_profiles ORDER BY name;

-- name: GetCloneProfile :one
SELECT * FROM clone_profiles WHERE name = $1;

-- name: InsertCloneProfile :one
INSERT INTO clone_profiles (name, description, config) VALUES ($1, $2, $3) RETURNING *;

-- name: UpdateCloneProfile :one
UPDATE clone_profiles SET description = $2, config = $3, updated_at = now() WHERE name = $1 RETURNING *;

-- name: DeleteCloneProfile :execrows
DELETE FROM clone_profiles WHERE name = $1;

-- name: InsertClonePackage :one
INSERT INTO clone_packages (profile, config, destination, passphrase_key_id, passphrase_nonce, passphrase, requested_by, requested_by_name)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg('requested_by'), sqlc.arg('requested_by_name'))
RETURNING id, profile, config, status, error, destination, key, size, taken_at, schema_version, rows, report,
    requested_by, requested_by_name, created_at, finished_at, heartbeat_at;

-- name: ClaimClonePackage :one
-- The oldest package waiting to be made, for one worker, with its sealed passphrase.
UPDATE clone_packages SET status = 'running', heartbeat_at = now()
WHERE id = (SELECT id FROM clone_packages WHERE status = 'pending' ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING *;

-- name: HeartbeatClonePackage :exec
UPDATE clone_packages SET heartbeat_at = now() WHERE id = $1 AND status = 'running';

-- name: FinishClonePackage :exec
-- Ready: the passphrase is no longer kept.
UPDATE clone_packages SET status = 'ready', key = $2, size = $3, taken_at = $4, schema_version = $5, rows = $6, report = $7,
    passphrase_key_id = '', passphrase_nonce = NULL, passphrase = NULL, finished_at = now(), heartbeat_at = NULL
WHERE id = $1;

-- name: FailClonePackage :exec
UPDATE clone_packages SET status = 'failed', error = $2, passphrase_key_id = '', passphrase_nonce = NULL, passphrase = NULL,
    finished_at = now(), heartbeat_at = NULL
WHERE id = $1;

-- name: FailStaleClonePackages :exec
UPDATE clone_packages SET status = 'failed', error = 'the worker making it stopped', passphrase_key_id = '',
    passphrase_nonce = NULL, passphrase = NULL, finished_at = now(), heartbeat_at = NULL
WHERE status = 'running' AND heartbeat_at < now() - interval '1 minute';

-- name: ListClonePackages :many
SELECT id, profile, config, status, error, destination, key, size, taken_at, schema_version, rows, report,
    requested_by, requested_by_name, created_at, finished_at, heartbeat_at
FROM clone_packages ORDER BY created_at DESC LIMIT 200;

-- name: GetClonePackage :one
SELECT id, profile, config, status, error, destination, key, size, taken_at, schema_version, rows, report,
    requested_by, requested_by_name, created_at, finished_at, heartbeat_at
FROM clone_packages WHERE id = $1;

-- name: DeleteClonePackage :exec
DELETE FROM clone_packages WHERE id = $1;

-- name: InsertCloneLoad :one
-- A package uploaded to load here: kept until someone applies or deletes it.
INSERT INTO clone_loads (destination, key, size, source, requested_by, requested_by_name)
VALUES ($1, $2, $3, $4, sqlc.narg('requested_by'), sqlc.arg('requested_by_name'))
RETURNING id, status, error, destination, key, size, source, keep_studio, keep_people, report, requested_by,
    requested_by_name, created_at, started_at, finished_at, heartbeat_at, origin, pull_key_id, scheduled_for, apply_when_pulled;

-- name: GetCloneLoad :one
SELECT id, status, error, destination, key, size, source, keep_studio, keep_people, report, requested_by,
    requested_by_name, created_at, started_at, finished_at, heartbeat_at, origin, pull_key_id, scheduled_for, apply_when_pulled
FROM clone_loads WHERE id = $1;

-- name: ListCloneLoads :many
SELECT id, status, error, destination, key, size, source, keep_studio, keep_people, report, requested_by,
    requested_by_name, created_at, started_at, finished_at, heartbeat_at, origin, pull_key_id, scheduled_for, apply_when_pulled
FROM clone_loads ORDER BY created_at DESC LIMIT 100;

-- name: RequestCloneLoad :one
-- Apply an uploaded package (again, after one that failed or was done): the passphrase waits sealed for the worker.
UPDATE clone_loads SET status = 'pending', error = '', keep_studio = $2, keep_people = $3, passphrase_key_id = $4,
    passphrase_nonce = $5, passphrase = $6, requested_by = sqlc.narg('requested_by'), requested_by_name = sqlc.arg('requested_by_name'),
    created_at = now(), started_at = NULL, finished_at = NULL, report = NULL
WHERE id = $1 AND status IN ('uploaded', 'done', 'failed')
RETURNING id;

-- name: ClaimCloneLoad :one
UPDATE clone_loads SET status = 'running', started_at = now(), heartbeat_at = now()
WHERE id = (SELECT id FROM clone_loads WHERE status = 'pending' ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING *;

-- name: HeartbeatCloneLoad :exec
UPDATE clone_loads SET heartbeat_at = now() WHERE id = $1 AND status = 'running';

-- name: FinishCloneLoad :exec
UPDATE clone_loads SET status = $2, error = $3, report = $4, passphrase_key_id = '', passphrase_nonce = NULL, passphrase = NULL,
    finished_at = now(), heartbeat_at = NULL
WHERE id = $1;

-- name: FailStaleCloneLoads :exec
-- A load or pull whose worker stopped, or one no worker took within two minutes (a load would keep the instance in
-- maintenance).
UPDATE clone_loads SET status = 'failed', passphrase_key_id = '', passphrase_nonce = NULL, passphrase = NULL,
    error = CASE WHEN status = 'running' THEN 'the worker loading it stopped'
                 WHEN status = 'pulling' AND started_at IS NOT NULL THEN 'the worker fetching it stopped'
                 ELSE 'no worker took it within two minutes: is the worker running?' END,
    finished_at = now(), heartbeat_at = NULL
WHERE (status IN ('running', 'pulling') AND started_at IS NOT NULL AND heartbeat_at < now() - interval '1 minute')
   OR (status IN ('pending', 'pulling') AND started_at IS NULL AND created_at < now() - interval '2 minutes');

-- name: DeleteCloneLoad :exec
DELETE FROM clone_loads WHERE id = $1;

-- name: ListCloneKeepPeople :many
SELECT k.user_id, u.name, u.email, k.added_at FROM clone_keep_people k JOIN users u ON u.id = k.user_id ORDER BY u.name;

-- name: AddCloneKeepPerson :exec
INSERT INTO clone_keep_people (user_id) VALUES ($1) ON CONFLICT DO NOTHING;

-- name: RemoveCloneKeepPerson :execrows
DELETE FROM clone_keep_people WHERE user_id = $1;

-- ---- the scheduled pull (00024_clone_pull) ----

-- name: InsertClonePull :one
-- A pull from the source, waiting for the worker to fetch it (one at a time: a second is not added).
INSERT INTO clone_loads (status, origin, destination, source, keep_studio, keep_people, scheduled_for, apply_when_pulled,
    requested_by, requested_by_name)
VALUES ('pulling', 'pull', $1, '{}'::jsonb, $2, $3, sqlc.narg('scheduled_for'), $4, sqlc.narg('requested_by'), sqlc.arg('requested_by_name'))
ON CONFLICT DO NOTHING
RETURNING id;

-- name: ClaimClonePull :one
UPDATE clone_loads SET started_at = now(), heartbeat_at = now()
WHERE id = (SELECT id FROM clone_loads WHERE status = 'pulling' AND started_at IS NULL ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING *;

-- name: HeartbeatClonePull :exec
UPDATE clone_loads SET heartbeat_at = now() WHERE id = $1 AND status = 'pulling';

-- name: FinishClonePull :exec
-- Fetched: waiting to be previewed and applied, or (apply_when_pulled) applied now. started_at is cleared so a load
-- that follows has its own.
UPDATE clone_loads SET status = sqlc.arg('status'), error = sqlc.arg('error'), key = $2, size = $3, source = $4, pull_key_id = $5,
    created_at = CASE WHEN sqlc.arg('status') = 'pending' THEN now() ELSE created_at END, started_at = NULL, heartbeat_at = NULL
WHERE id = $1 AND status = 'pulling';

-- name: FailClonePull :exec
UPDATE clone_loads SET status = 'failed', error = $2, finished_at = now(), heartbeat_at = NULL WHERE id = $1;

-- name: LastScheduledPull :one
-- The latest due time the schedule asked for a pull for (by the worker's clock, as the due time is worked out).
SELECT coalesce(max(scheduled_for), '-infinity'::timestamptz)::timestamptz AS at FROM clone_loads WHERE origin = 'pull';

-- name: InsertClonePullKey :one
INSERT INTO clone_pull_keys (profile, name, prefix, secret_hash, created_by_name) VALUES ($1, $2, $3, $4, $5) RETURNING id;

-- name: ListClonePullKeys :many
SELECT id, profile, name, prefix, created_by_name, created_at, last_used_at, revoked_at FROM clone_pull_keys
ORDER BY profile, created_at;

-- name: RevokeClonePullKey :execrows
UPDATE clone_pull_keys SET revoked_at = now() WHERE id = $1 AND profile = $2 AND revoked_at IS NULL;

-- name: FindClonePullKey :one
-- As the system: the live key with this hash.
UPDATE clone_pull_keys SET last_used_at = now() WHERE secret_hash = $1 AND revoked_at IS NULL
RETURNING id, profile, name;

-- name: StartServedPull :one
INSERT INTO clone_pulls (profile, key_id, key_name, remote_addr) VALUES ($1, $2, $3, $4) RETURNING id;

-- name: HeartbeatServedPull :exec
UPDATE clone_pulls SET heartbeat_at = now() WHERE id = $1 AND status = 'running';

-- name: FinishServedPull :exec
UPDATE clone_pulls SET status = $2, error = $3, size = $4, rows = $5, finished_at = now() WHERE id = $1;

-- name: FailStaleServedPulls :exec
UPDATE clone_pulls SET status = 'failed', error = 'the server sending it stopped', finished_at = now()
WHERE status = 'running' AND heartbeat_at < now() - interval '1 minute';

-- name: ListServedPulls :many
SELECT * FROM clone_pulls ORDER BY started_at DESC LIMIT 50;
