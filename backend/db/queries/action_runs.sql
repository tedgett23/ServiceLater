-- name: InsertActionRun :exec
INSERT INTO action_runs (app, rule, timing, event, record_id, outcome, detail, duration_ms, job_id, attempt)
VALUES (sqlc.arg('app'), sqlc.arg('rule'), sqlc.arg('timing'), sqlc.arg('event'), sqlc.narg('record_id'),
        sqlc.arg('outcome'), sqlc.arg('detail'), sqlc.arg('duration_ms'), sqlc.narg('job_id'), sqlc.arg('attempt'));

-- name: ListActionRuns :many
SELECT * FROM action_runs
WHERE (sqlc.narg('outcome')::text IS NULL OR outcome = sqlc.narg('outcome'))
ORDER BY id DESC
LIMIT sqlc.arg('limit');

-- name: PurgeActionRuns :execrows
DELETE FROM action_runs WHERE created_at < now() - make_interval(secs => sqlc.arg('older_than_secs')::float8);
