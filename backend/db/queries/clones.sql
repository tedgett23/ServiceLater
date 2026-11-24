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
