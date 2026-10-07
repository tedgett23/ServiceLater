-- SLA clocks (task_slas, migration 00025). A save reads and writes them as the person saving; the SLA timer as the
-- system.

-- name: ListTaskSLAs :many
-- A record's clocks, for its page: the ones still in force, in the order they started.
SELECT * FROM task_slas WHERE task_id = $1 AND stage <> 'cancelled' ORDER BY started_at, sla;

-- name: ListTaskSLAsForUpdate :many
-- Every clock of a record, locked, for the save that is about to move them.
SELECT * FROM task_slas WHERE task_id = $1 ORDER BY started_at, sla FOR UPDATE;

-- name: CreateTaskSLA :one
INSERT INTO task_slas (task_id, app, sla, definition, checksum, stage, target_ms, elapsed_ms, running_since, due_at,
                       breached_at, warnings_sent, next_alert_at, problem, started_at, stopped_at)
VALUES (sqlc.arg('task_id'), sqlc.arg('app'), sqlc.arg('sla'), sqlc.arg('definition'), sqlc.arg('checksum'),
        sqlc.arg('stage'), sqlc.arg('target_ms'), sqlc.arg('elapsed_ms'), sqlc.narg('running_since'), sqlc.narg('due_at'),
        sqlc.narg('breached_at'), sqlc.arg('warnings_sent'), sqlc.narg('next_alert_at'), sqlc.arg('problem'),
        sqlc.arg('started_at'), sqlc.narg('stopped_at'))
RETURNING id;

-- name: UpdateTaskSLA :exec
UPDATE task_slas SET stage = sqlc.arg('stage'), target_ms = sqlc.arg('target_ms'), elapsed_ms = sqlc.arg('elapsed_ms'),
    running_since = sqlc.narg('running_since'), due_at = sqlc.narg('due_at'), breached_at = sqlc.narg('breached_at'),
    warnings_sent = sqlc.arg('warnings_sent'), next_alert_at = sqlc.narg('next_alert_at'), problem = sqlc.arg('problem'),
    stopped_at = sqlc.narg('stopped_at'), updated_at = now()
WHERE id = sqlc.arg('id');

-- name: ClaimDueSLAs :many
-- Running clocks the timer must look at now: a warning or a breach is due, or their calendar changed. Locked and
-- skipped by other workers, so each is handled once.
SELECT * FROM task_slas
WHERE stage = 'running' AND next_alert_at <= now()
ORDER BY next_alert_at
LIMIT sqlc.arg('max')
FOR UPDATE SKIP LOCKED;

-- name: GetTaskSLA :one
SELECT * FROM task_slas WHERE id = $1;

-- name: ListBusinessCalendars :many
SELECT sqlc.embed(c), u.name AS created_by_name FROM business_calendars c
LEFT JOIN users u ON u.id = c.created_by
ORDER BY c.label, c.name;

-- name: GetBusinessCalendar :one
SELECT sqlc.embed(c), u.name AS created_by_name FROM business_calendars c
LEFT JOIN users u ON u.id = c.created_by
WHERE c.id = $1;

-- name: GetBusinessCalendarByName :one
SELECT * FROM business_calendars WHERE name = $1;

-- name: CreateBusinessCalendar :one
INSERT INTO business_calendars (name, label, time_zone, hours, holidays, created_by)
VALUES ($1, $2, $3, $4, $5, app_current_user_id())
RETURNING id;

-- name: UpdateBusinessCalendar :one
UPDATE business_calendars SET name = $2, label = $3, time_zone = $4, hours = $5, holidays = $6, updated_at = now()
WHERE id = $1
RETURNING id;

-- name: DeleteBusinessCalendar :execrows
DELETE FROM business_calendars WHERE id = $1;

-- name: CountClocksByCalendar :one
-- How many clocks still running or paused count by a calendar, so deleting one in use can be refused.
SELECT count(*)::bigint FROM task_slas WHERE stage IN ('running', 'paused') AND definition ->> 'calendar' = sqlc.arg('name')::text;
