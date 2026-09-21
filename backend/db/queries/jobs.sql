-- The job queue. Everything except EnqueueJob runs through db.WithSystem: users cannot read the table.

-- Callable by users (it is SECURITY DEFINER). Returns 0 when the dedupe key was already used.
-- name: EnqueueJob :one
SELECT COALESCE(enqueue_job(sqlc.arg('kind')::text, sqlc.arg('payload')::jsonb, sqlc.arg('run_at')::timestamptz, sqlc.narg('dedupe_key')::text), 0)::bigint AS id;

-- Claims the oldest due job. SKIP LOCKED lets any number of workers do this at once without waiting on each other.
-- name: ClaimJob :one
UPDATE jobs SET state = 'running', locked_by = $1, locked_at = now(), attempts = attempts + 1
WHERE id = (
    SELECT id FROM jobs WHERE state = 'pending' AND run_at <= now()
    ORDER BY run_at, id LIMIT 1 FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- Both finishing calls check the lease, so a worker that was presumed dead and replaced cannot overwrite the outcome.
-- name: CompleteJob :execrows
UPDATE jobs SET state = 'done', finished_at = now(), locked_by = NULL, locked_at = NULL
WHERE id = $1 AND state = 'running' AND locked_by = $2;

-- Retries with exponential backoff (base * 2^(attempt-1), capped), or dead-letters the job when attempts run out.
-- name: FailJob :execrows
UPDATE jobs SET
    state = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
    run_at = CASE WHEN attempts >= max_attempts THEN run_at
                  ELSE now() + make_interval(secs => LEAST(sqlc.arg('max_backoff_secs')::float8, sqlc.arg('base_backoff_secs')::float8 * power(2, attempts - 1))) END,
    finished_at = CASE WHEN attempts >= max_attempts THEN now() END,
    last_error = sqlc.arg('error')::text,
    locked_by = NULL, locked_at = NULL
WHERE id = sqlc.arg('id') AND state = 'running' AND locked_by = sqlc.arg('locked_by');

-- For an error that retrying cannot fix: straight to the dead letter.
-- name: DeadLetterJob :execrows
UPDATE jobs SET state = 'failed', finished_at = now(), last_error = $3, locked_by = NULL, locked_at = NULL
WHERE id = $1 AND state = 'running' AND locked_by = $2;

-- A worker that died mid-job leaves it running forever. Give such jobs back so another worker can take them.
-- name: ReapStuckJobs :execrows
UPDATE jobs SET state = 'pending', locked_by = NULL, locked_at = NULL,
    last_error = COALESCE(last_error, '') || 'worker lost; '
WHERE state = 'running' AND locked_at < now() - make_interval(secs => sqlc.arg('stuck_after_secs')::float8);

-- Retention: finished work does not need to live forever.
-- name: PurgeFinishedJobs :execrows
DELETE FROM jobs WHERE (state = 'done' AND finished_at < now() - make_interval(secs => sqlc.arg('done_after_secs')::float8))
                    OR (state = 'failed' AND finished_at < now() - make_interval(secs => sqlc.arg('failed_after_secs')::float8));

-- name: ListJobs :many
SELECT * FROM jobs WHERE (sqlc.narg('state')::text IS NULL OR state = sqlc.narg('state')) ORDER BY id DESC LIMIT sqlc.arg('limit');

-- name: RetryJob :execrows
UPDATE jobs SET state = 'pending', attempts = 0, run_at = now(), finished_at = NULL, locked_by = NULL, locked_at = NULL
WHERE id = $1 AND state = 'failed';

-- name: CountJobsByState :many
SELECT state, count(*)::bigint AS n FROM jobs GROUP BY state;
