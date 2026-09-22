-- name: InsertExecution :one
INSERT INTO workflow_executions (app, workflow, app_version, definition, record_id, started_by, cause, current_step)
VALUES (sqlc.arg('app'), sqlc.arg('workflow'), sqlc.arg('app_version'), sqlc.arg('definition'), sqlc.narg('record_id'),
        sqlc.arg('started_by'), sqlc.arg('cause'), sqlc.arg('current_step'))
RETURNING id;

-- name: GetExecution :one
SELECT * FROM workflow_executions WHERE id = $1;

-- name: GetExecutionForUpdate :one
SELECT * FROM workflow_executions WHERE id = $1 FOR UPDATE;

-- What the engine needs before it can open a transaction as the right person.
-- name: GetExecutionOwner :one
SELECT started_by, status FROM workflow_executions WHERE id = $1;

-- name: SaveExecution :exec
UPDATE workflow_executions SET
    status = sqlc.arg('status'), current_step = sqlc.arg('current_step'), vars = sqlc.arg('vars'),
    steps_run = sqlc.arg('steps_run'), wait_started_at = sqlc.narg('wait_started_at'), wait_deadline = sqlc.narg('wait_deadline'),
    wait_until = sqlc.arg('wait_until'), wake_at = sqlc.narg('wake_at'), error = sqlc.arg('error'),
    finished_at = CASE WHEN sqlc.arg('status')::text IN ('done', 'failed', 'cancelled') THEN now() ELSE NULL END,
    updated_at = now()
WHERE id = sqlc.arg('id');

-- name: InsertStep :exec
INSERT INTO workflow_steps (execution_id, seq, step_id, type, outcome, detail, error, duration_ms)
VALUES (sqlc.arg('execution_id'), sqlc.arg('seq'), sqlc.arg('step_id'), sqlc.arg('type'), sqlc.arg('outcome'),
        sqlc.arg('detail'), sqlc.arg('error'), sqlc.arg('duration_ms'));

-- name: ListExecutions :many
SELECT * FROM workflow_executions
WHERE (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (sqlc.narg('record_id')::uuid IS NULL OR record_id = sqlc.narg('record_id'))
  AND (sqlc.narg('workflow')::text IS NULL OR workflow = sqlc.narg('workflow'))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: ListSteps :many
SELECT * FROM workflow_steps WHERE execution_id = $1 ORDER BY seq;

-- name: CancelExecution :execrows
UPDATE workflow_executions SET status = 'cancelled', finished_at = now(), updated_at = now(), wake_at = NULL, error = 'cancelled'
WHERE id = $1 AND status IN ('queued', 'running', 'waiting');

-- The timer (system): sleeping executions that are due become queued, and the caller queues a run job for each.
-- name: WakeDueExecutions :many
UPDATE workflow_executions SET status = 'queued', wake_at = NULL, updated_at = now()
WHERE id IN (
    SELECT id FROM workflow_executions WHERE status = 'waiting' AND wake_at <= now()
    ORDER BY wake_at LIMIT 200 FOR UPDATE SKIP LOCKED)
RETURNING id;

-- name: CountExecutionsByStatus :many
SELECT status, count(*)::bigint AS n FROM workflow_executions GROUP BY status;

-- name: PurgeExecutions :execrows
DELETE FROM workflow_executions WHERE finished_at < now() - make_interval(secs => sqlc.arg('older_than_secs')::float8);

-- The run viewer: an execution with the reference of the ticket it is about (empty when there is none, or it is hidden).
-- name: ListExecutionViews :many
SELECT sqlc.embed(e), COALESCE(tt.prefix || t.number::text, '')::text AS reference
FROM workflow_executions e
LEFT JOIN tasks t ON t.id = e.record_id
LEFT JOIN task_types tt ON tt.type = t.type
WHERE (sqlc.narg('status')::text IS NULL OR e.status = sqlc.narg('status'))
  AND (sqlc.narg('record_id')::uuid IS NULL OR e.record_id = sqlc.narg('record_id'))
  AND (sqlc.narg('workflow')::text IS NULL OR e.workflow = sqlc.narg('workflow'))
ORDER BY e.created_at DESC, e.id DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: GetExecutionView :one
SELECT sqlc.embed(e), COALESCE(tt.prefix || t.number::text, '')::text AS reference
FROM workflow_executions e
LEFT JOIN tasks t ON t.id = e.record_id
LEFT JOIN task_types tt ON tt.type = t.type
WHERE e.id = $1;
