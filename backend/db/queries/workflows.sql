-- name: InsertExecution :one
INSERT INTO workflow_executions (app, workflow, app_version, definition, record_id, started_by, cause, current_step, run_as, vars)
VALUES (sqlc.arg('app'), sqlc.arg('workflow'), sqlc.arg('app_version'), sqlc.arg('definition'), sqlc.narg('record_id'),
        sqlc.arg('started_by'), sqlc.arg('cause'), sqlc.arg('current_step'), app_principal(sqlc.arg('app')),
        coalesce(sqlc.narg('vars')::jsonb, '{}'))
RETURNING id;

-- name: GetExecution :one
SELECT * FROM workflow_executions WHERE id = $1;

-- name: GetExecutionForUpdate :one
SELECT * FROM workflow_executions WHERE id = $1 FOR UPDATE;

-- What the engine needs before it can open a transaction as the right person.
-- name: GetExecutionOwner :one
SELECT started_by, run_as, status FROM workflow_executions WHERE id = $1;

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

-- name: WorkflowInFlight :many
-- The runs of a workflow still going, with the record each is on (when the reader may see it), oldest first.
SELECT e.id, e.status, e.current_step, e.created_at, e.updated_at, e.wake_at, COALESCE(tt.prefix || t.number::text, '')::text AS record_reference,
       t.title AS record_title
FROM workflow_executions e
LEFT JOIN tasks t ON t.id = e.record_id
LEFT JOIN task_types tt ON tt.type = t.type
WHERE e.app = sqlc.arg('app') AND e.workflow = sqlc.arg('workflow') AND e.status IN ('queued', 'running', 'waiting')
ORDER BY e.created_at
LIMIT 101;

-- name: WorkflowStepOutcomes :many
-- How each step of a workflow went since a time: visits by outcome (ok, waiting, timeout, failed).
SELECT s.step_id, s.outcome, count(*)::bigint AS n
FROM workflow_steps s
JOIN workflow_executions e ON e.id = s.execution_id
WHERE e.app = sqlc.arg('app') AND e.workflow = sqlc.arg('workflow') AND e.created_at >= sqlc.arg('since')
  AND s.created_at >= sqlc.arg('since')
GROUP BY s.step_id, s.outcome;

-- name: WorkflowRunCounts :many
-- How a workflow's runs since a time ended up, by status.
SELECT status, count(*)::bigint AS n FROM workflow_executions
WHERE app = sqlc.arg('app') AND workflow = sqlc.arg('workflow') AND created_at >= sqlc.arg('since')
GROUP BY status;

-- name: InsertChildExecution :one
-- A subflow's or foreach's child run, started by a step of its parent, as the parent runs.
INSERT INTO workflow_executions (app, workflow, app_version, definition, record_id, started_by, cause, current_step, run_as,
                                 vars, parent_id, parent_step, parent_index, depth)
VALUES (sqlc.arg('app'), sqlc.arg('workflow'), sqlc.arg('app_version'), sqlc.arg('definition'), sqlc.narg('record_id'),
        sqlc.arg('started_by'), sqlc.arg('cause'), sqlc.arg('current_step'), app_principal(sqlc.arg('app')),
        sqlc.arg('vars'), sqlc.arg('parent_id'), sqlc.arg('parent_step'), sqlc.arg('parent_index'), sqlc.arg('depth'))
RETURNING id;

-- name: ListChildExecutions :many
-- The children a step of a run started, in order.
SELECT id, status, vars, parent_index, error, record_id
FROM workflow_executions
WHERE parent_id = $1 AND parent_step = $2
ORDER BY parent_index;

-- name: ClaimSchedule :one
-- Takes a scheduled workflow's due time if no one has: true for the one worker that should start it.
INSERT INTO workflow_schedules (app, workflow, last_due) VALUES ($1, $2, $3)
ON CONFLICT (app, workflow) DO UPDATE SET last_due = EXCLUDED.last_due
WHERE workflow_schedules.last_due < EXCLUDED.last_due
RETURNING true;

-- name: GetScheduleLastDue :one
SELECT last_due FROM workflow_schedules WHERE app = $1 AND workflow = $2;

-- name: FindRecords :many
-- A query step's or schedule's records: ids of one type matching every filter, as the caller sees them.
SELECT id::uuid AS id FROM app_find_records(sqlc.arg('type')::text, sqlc.arg('filters')::jsonb, sqlc.arg('max')::integer) AS f(id);

-- name: CancelChildExecutions :exec
-- Stops a step's children still going (the step gave up waiting for them).
UPDATE workflow_executions SET status = 'cancelled', error = 'the step that started it stopped waiting', finished_at = now(), updated_at = now()
WHERE parent_id = $1 AND parent_step = $2 AND status IN ('queued', 'running', 'waiting');

-- name: ListRunChildren :many
-- Every child run a run's steps started, in order, as the reader may see them.
SELECT id, workflow, status, parent_step, parent_index FROM workflow_executions
WHERE parent_id = $1 ORDER BY parent_step, parent_index;

-- name: InsertWebhookDelivery :one
-- Records a call to a webhook. A started one whose key an earlier started call already used returns nothing: the
-- caller then looks up that call's run (FindWebhookDelivery) and answers with it.
INSERT INTO workflow_webhook_deliveries (app, workflow, delivery_key, outcome, reason, execution_id, size, remote)
VALUES (sqlc.arg('app'), sqlc.arg('workflow'), sqlc.narg('delivery_key'), sqlc.arg('outcome'), sqlc.arg('reason'),
        sqlc.narg('execution_id'), sqlc.arg('size'), sqlc.arg('remote'))
ON CONFLICT (app, workflow, delivery_key) WHERE outcome = 'started' AND delivery_key IS NOT NULL DO NOTHING
RETURNING id;

-- name: SetWebhookDeliveryRun :exec
UPDATE workflow_webhook_deliveries SET execution_id = $2 WHERE id = $1;

-- name: FindWebhookDelivery :one
-- The run an earlier call with this key started.
SELECT execution_id FROM workflow_webhook_deliveries
WHERE app = $1 AND workflow = $2 AND delivery_key = $3 AND outcome = 'started';

-- name: ListWebhookDeliveries :many
-- A webhook's latest calls, newest first, for the builder (as the reader: workflows:read_all).
SELECT id, delivery_key, outcome, reason, execution_id, size, remote, received_at
FROM workflow_webhook_deliveries
WHERE app = $1 AND workflow = $2
ORDER BY received_at DESC
LIMIT 50;

-- name: DeleteOldWebhookDeliveries :execrows
DELETE FROM workflow_webhook_deliveries WHERE received_at < now() - interval '30 days';
