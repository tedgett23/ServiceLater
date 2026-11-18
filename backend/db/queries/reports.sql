-- The SLA report (internal/api/reports.go). Every query runs as the reader, so a record they cannot see does not
-- count. The filters are the same in each: record type, SLA (app and name), assignment group (or none) and priority.

-- name: SLAReportEnded :many
-- Clocks that ended in [from, to), counted by period bucket, SLA, record type, priority and assignment group: how
-- many, and how many met their target. The caller adds them up for each view.
SELECT date_trunc(sqlc.arg('bucket')::text, s.stopped_at, 'UTC')::timestamptz AS bucket,
       s.app, s.sla, coalesce(s.definition ->> 'label', s.sla)::text AS label,
       t.type, t.priority, t.assignment_group_id, coalesce(g.name, '')::text AS group_name,
       count(*)::bigint AS ended, (count(*) FILTER (WHERE s.breached_at IS NULL))::bigint AS met
FROM task_slas s
JOIN tasks t ON t.id = s.task_id
LEFT JOIN groups g ON g.id = t.assignment_group_id
WHERE s.stage = 'stopped' AND s.stopped_at >= sqlc.arg('from') AND s.stopped_at < sqlc.arg('to')
  AND (sqlc.narg('type')::text IS NULL OR t.type = sqlc.narg('type')::text)
  AND (sqlc.narg('app')::text IS NULL OR (s.app = sqlc.narg('app')::text AND s.sla = sqlc.narg('sla')::text))
  AND (sqlc.narg('group_id')::uuid IS NULL OR t.assignment_group_id = sqlc.narg('group_id')::uuid)
  AND (NOT sqlc.arg('no_group')::bool OR t.assignment_group_id IS NULL)
  AND (sqlc.narg('priority')::int IS NULL OR t.priority = sqlc.narg('priority')::int)
GROUP BY 1, s.app, s.sla, 4, t.type, t.priority, t.assignment_group_id, g.name;

-- name: SLAReportMedians :many
-- For each SLA, the median working time its clocks that ended in [from, to) used, and their median target.
SELECT s.app, s.sla, coalesce(s.definition ->> 'label', s.sla)::text AS label, count(*)::bigint AS ended,
       (percentile_cont(0.5) WITHIN GROUP (ORDER BY s.elapsed_ms))::float8 AS median_ms,
       (percentile_cont(0.5) WITHIN GROUP (ORDER BY s.target_ms))::float8 AS median_target_ms
FROM task_slas s
JOIN tasks t ON t.id = s.task_id
WHERE s.stage = 'stopped' AND s.stopped_at >= sqlc.arg('from') AND s.stopped_at < sqlc.arg('to')
  AND (sqlc.narg('type')::text IS NULL OR t.type = sqlc.narg('type')::text)
  AND (sqlc.narg('app')::text IS NULL OR (s.app = sqlc.narg('app')::text AND s.sla = sqlc.narg('sla')::text))
  AND (sqlc.narg('group_id')::uuid IS NULL OR t.assignment_group_id = sqlc.narg('group_id')::uuid)
  AND (NOT sqlc.arg('no_group')::bool OR t.assignment_group_id IS NULL)
  AND (sqlc.narg('priority')::int IS NULL OR t.priority = sqlc.narg('priority')::int)
GROUP BY s.app, s.sla, 3
ORDER BY 3;

-- name: SLAReportOpen :many
-- Clocks running or paused now, with their records, for what is at risk and what is still running.
SELECT s.id, s.app, s.sla, coalesce(s.definition ->> 'label', s.sla)::text AS label, s.stage, s.target_ms, s.elapsed_ms,
       s.running_since, s.due_at, s.breached_at, s.calendar,
       t.id AS task_id, (tt.prefix || t.number::text)::text AS reference, t.title, t.type, t.priority,
       t.assignment_group_id, coalesce(g.name, '')::text AS group_name
FROM task_slas s
JOIN tasks t ON t.id = s.task_id
JOIN task_types tt ON tt.type = t.type
LEFT JOIN groups g ON g.id = t.assignment_group_id
WHERE s.stage IN ('running', 'paused')
  AND (sqlc.narg('type')::text IS NULL OR t.type = sqlc.narg('type')::text)
  AND (sqlc.narg('app')::text IS NULL OR (s.app = sqlc.narg('app')::text AND s.sla = sqlc.narg('sla')::text))
  AND (sqlc.narg('group_id')::uuid IS NULL OR t.assignment_group_id = sqlc.narg('group_id')::uuid)
  AND (NOT sqlc.arg('no_group')::bool OR t.assignment_group_id IS NULL)
  AND (sqlc.narg('priority')::int IS NULL OR t.priority = sqlc.narg('priority')::int)
ORDER BY s.due_at NULLS LAST
LIMIT sqlc.arg('max');

-- name: SLAReportBreaches :many
-- The latest clocks to breach in [from, to), with their records.
SELECT s.id, s.app, s.sla, coalesce(s.definition ->> 'label', s.sla)::text AS label, s.stage, s.target_ms, s.elapsed_ms,
       s.running_since, s.breached_at, s.calendar,
       t.id AS task_id, (tt.prefix || t.number::text)::text AS reference, t.title, t.type, t.priority,
       coalesce(g.name, '')::text AS group_name
FROM task_slas s
JOIN tasks t ON t.id = s.task_id
JOIN task_types tt ON tt.type = t.type
LEFT JOIN groups g ON g.id = t.assignment_group_id
WHERE s.stage <> 'cancelled' AND s.breached_at >= sqlc.arg('from') AND s.breached_at < sqlc.arg('to')
  AND (sqlc.narg('type')::text IS NULL OR t.type = sqlc.narg('type')::text)
  AND (sqlc.narg('app')::text IS NULL OR (s.app = sqlc.narg('app')::text AND s.sla = sqlc.narg('sla')::text))
  AND (sqlc.narg('group_id')::uuid IS NULL OR t.assignment_group_id = sqlc.narg('group_id')::uuid)
  AND (NOT sqlc.arg('no_group')::bool OR t.assignment_group_id IS NULL)
  AND (sqlc.narg('priority')::int IS NULL OR t.priority = sqlc.narg('priority')::int)
ORDER BY s.breached_at DESC
LIMIT sqlc.arg('max');

-- name: SLAReportClocks :many
-- Every clock that ended or breached in [from, to), or is running or paused now, for the CSV export.
SELECT s.app, s.sla, coalesce(s.definition ->> 'label', s.sla)::text AS label, s.stage, s.target_ms, s.elapsed_ms,
       s.running_since, s.started_at, s.stopped_at, s.breached_at, s.calendar,
       (tt.prefix || t.number::text)::text AS reference, t.title, t.type, t.priority, coalesce(g.name, '')::text AS group_name
FROM task_slas s
JOIN tasks t ON t.id = s.task_id
JOIN task_types tt ON tt.type = t.type
LEFT JOIN groups g ON g.id = t.assignment_group_id
WHERE s.stage <> 'cancelled'
  AND (s.stage IN ('running', 'paused') OR (s.stopped_at >= sqlc.arg('from') AND s.stopped_at < sqlc.arg('to'))
       OR (s.breached_at >= sqlc.arg('from') AND s.breached_at < sqlc.arg('to')))
  AND (sqlc.narg('type')::text IS NULL OR t.type = sqlc.narg('type')::text)
  AND (sqlc.narg('app')::text IS NULL OR (s.app = sqlc.narg('app')::text AND s.sla = sqlc.narg('sla')::text))
  AND (sqlc.narg('group_id')::uuid IS NULL OR t.assignment_group_id = sqlc.narg('group_id')::uuid)
  AND (NOT sqlc.arg('no_group')::bool OR t.assignment_group_id IS NULL)
  AND (sqlc.narg('priority')::int IS NULL OR t.priority = sqlc.narg('priority')::int)
ORDER BY s.started_at
LIMIT sqlc.arg('max');
