-- Tasks are the shared base of every record type; a type with typed columns adds an extension table
-- (incidents so far). See docs/apps-platform.md §9.2. Callers pass the acting user through db.WithUser, so RLS
-- on tasks (and, through it, on the extension tables and comments) decides what is visible.

-- The assignment group is written with the row, not after it: a before rule routes an end user's ticket as it is
-- filed, and only staff may update a task.
-- name: CreateTask :one
INSERT INTO tasks (type, title, description, priority, requester_id, status, state_category, assignment_group_id, fields, parent_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE(sqlc.narg('fields')::jsonb, '{}'::jsonb), sqlc.narg('parent_id'))
RETURNING *;

-- The extension row for an incident. Every incident has one; the composite key guarantees the task really is one.
-- name: CreateIncident :exec
INSERT INTO incidents (task_id) VALUES ($1);

-- The extension row for a change, with its defaults; the save writes what was filed into it after.
-- name: CreateRequest :exec
INSERT INTO requests (task_id, catalog_item) VALUES ($1, $2);

-- name: CreateChange :exec
INSERT INTO changes (task_id) VALUES ($1);

-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1;

-- A reference is a type's prefix and its number, such as INC42.
-- name: GetTaskByReference :one
SELECT t.* FROM tasks t JOIN task_types tt ON tt.type = t.type WHERE tt.prefix = $1 AND t.number = $2;

-- name: GetTaskByTypeNumber :one
SELECT * FROM tasks WHERE type = $1 AND number = $2;

-- name: GetTaskForUpdate :one
SELECT * FROM tasks WHERE id = $1 FOR UPDATE;

-- Types open to everyone (incidents) first: the New ticket dialog starts on the first one the person may file.
-- Retired types (their app was removed) are not listed: nothing offers, grants or files them.
-- name: ListTaskTypes :many
SELECT * FROM task_types WHERE retired_at IS NULL ORDER BY anyone_may_file DESC, type;

-- name: GetTaskType :one
SELECT * FROM task_types WHERE type = $1;

-- The types whose apps were removed: their records still open by reference.
-- name: ListRetiredTaskTypes :many
SELECT type FROM task_types WHERE retired_at IS NOT NULL ORDER BY type;

-- name: GetTaskTypeByPrefix :one
SELECT * FROM task_types WHERE prefix = $1;

-- A prefix someone may type to find a record: a type that is not retired.
-- name: GetLiveTaskTypeByPrefix :one
SELECT * FROM task_types WHERE prefix = $1 AND retired_at IS NULL;

-- An app's record type, written by its install (records keep their numbering across reinstalls). Bringing a type back
-- clears its retirement. name and prefix of a type with records never change: the install refuses that first.
-- name: UpsertAppTaskType :exec
INSERT INTO task_types (type, prefix, label, plural, anyone_may_file, description, owner_app)
VALUES (sqlc.arg('type'), sqlc.arg('prefix'), sqlc.arg('label'), sqlc.arg('plural'),
        sqlc.arg('anyone_may_file'), sqlc.arg('description'), sqlc.arg('owner_app'))
ON CONFLICT (type) DO UPDATE SET prefix = EXCLUDED.prefix, label = EXCLUDED.label, plural = EXCLUDED.plural,
    anyone_may_file = EXCLUDED.anyone_may_file,
    description = EXCLUDED.description, retired_at = NULL;

-- The app's types it no longer defines (all of them, on uninstall) are retired; their records stay.
-- name: RetireAppTaskTypes :many
UPDATE task_types SET retired_at = now()
WHERE owner_app = sqlc.arg('owner_app') AND retired_at IS NULL AND NOT (type = ANY (sqlc.arg('keep')::text[]))
RETURNING type;

-- name: CountTasksOfType :one
SELECT count(*) FROM tasks WHERE type = $1;

-- A ticket as clients see it: the task, display names, the type's prefix, and the type's own columns. Plain joins,
-- not a view: views run with their owner's rights and skip RLS. Each type with an extension table adds one LEFT JOIN.
-- name: GetTicketView :one
SELECT sqlc.embed(t), tt.prefix AS type_prefix, (tt.prefix || t.number::text)::text AS reference, r.name AS requester_name, a.name AS assignee_name,
       g.name AS assignment_group_name, COALESCE(i.resolution, '')::text AS resolution,
       c.change_type, COALESCE(c.risk, '')::text AS risk, COALESCE(c.impact, '')::text AS impact,
       c.planned_start, c.planned_end, COALESCE(c.implementation_plan, '')::text AS implementation_plan,
       COALESCE(c.backout_plan, '')::text AS backout_plan, COALESCE(c.test_plan, '')::text AS test_plan,
       COALESCE(rq.catalog_item, '')::text AS catalog_item, r.manager_id AS requester_manager_id, COALESCE(pt.prefix || p.number, '')::text AS parent_reference,
       (SELECT count(*) FROM tasks k WHERE k.parent_id = t.id AND k.state_category IN ('open', 'waiting')) AS open_tasks
FROM tasks t
JOIN task_types tt ON tt.type = t.type
JOIN users r ON r.id = t.requester_id
LEFT JOIN users a ON a.id = t.assignee_id
LEFT JOIN groups g ON g.id = t.assignment_group_id
LEFT JOIN incidents i ON i.task_id = t.id
LEFT JOIN changes c ON c.task_id = t.id
LEFT JOIN requests rq ON rq.task_id = t.id
LEFT JOIN tasks p ON p.id = t.parent_id
LEFT JOIN task_types pt ON pt.type = p.type
WHERE t.id = $1;

-- The queue works on tasks alone, whatever their type, so it never needs the extension tables.
-- Each row carries its SLA summary: the soonest due time of its running clocks, and whether any clock in force has
-- breached.
-- name: ListTickets :many
SELECT sqlc.embed(t), tt.prefix AS type_prefix, (tt.prefix || t.number::text)::text AS reference, r.name AS requester_name, a.name AS assignee_name,
       g.name AS assignment_group_name, sla.due_at::timestamptz AS sla_due_at, COALESCE(sla.breached, false)::bool AS sla_breached
FROM tasks t
JOIN task_types tt ON tt.type = t.type
JOIN users r ON r.id = t.requester_id
LEFT JOIN users a ON a.id = t.assignee_id
LEFT JOIN groups g ON g.id = t.assignment_group_id
LEFT JOIN LATERAL (
    SELECT min(s.due_at) FILTER (WHERE s.stage = 'running' AND s.breached_at IS NULL) AS due_at,
           bool_or(s.breached_at IS NOT NULL) AS breached
    FROM task_slas s WHERE s.task_id = t.id AND s.stage <> 'cancelled'
) sla ON true
-- A retired type's records (its app was removed) are left out of every list; they open only by reference.
WHERE tt.retired_at IS NULL
  AND (sqlc.narg('type')::text IS NULL OR t.type = sqlc.narg('type'))
  AND (sqlc.narg('status')::text IS NULL OR t.status = sqlc.narg('status'))
  AND (sqlc.narg('priority')::smallint IS NULL OR t.priority = sqlc.narg('priority'))
  AND (sqlc.narg('assignee_id')::uuid IS NULL OR t.assignee_id = sqlc.narg('assignee_id'))
  AND (sqlc.narg('requester_id')::uuid IS NULL OR t.requester_id = sqlc.narg('requester_id'))
  AND (NOT sqlc.arg('open_only')::bool OR t.state_category IN ('open', 'waiting'))
  AND (NOT sqlc.arg('unassigned')::bool OR t.assignee_id IS NULL)
  AND (sqlc.narg('assignment_group_id')::uuid IS NULL OR t.assignment_group_id = sqlc.narg('assignment_group_id'))
  AND (NOT sqlc.arg('no_group')::bool OR t.assignment_group_id IS NULL)
  AND (NOT sqlc.arg('my_groups')::bool OR t.assignment_group_id IN
       (SELECT m.group_id FROM group_members m WHERE m.user_id = app_current_user_id()))
  AND (sqlc.narg('search')::text IS NULL OR t.title ILIKE '%' || sqlc.narg('search') || '%' ESCAPE '\')
  AND (NOT sqlc.arg('breached')::bool OR sla.breached)
-- by_due: breached first, then the soonest due, then the rest newest first.
ORDER BY CASE WHEN sqlc.arg('by_due')::bool THEN NOT COALESCE(sla.breached, false) END,
         CASE WHEN sqlc.arg('by_due')::bool THEN sla.due_at END ASC NULLS LAST, t.created_at DESC, t.id DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: UpdateTask :one
UPDATE tasks SET
    title       = COALESCE(sqlc.narg('title'), title),
    description = COALESCE(sqlc.narg('description'), description),
    priority    = COALESCE(sqlc.narg('priority'), priority),
    assignee_id = CASE WHEN sqlc.arg('set_assignee')::bool THEN sqlc.narg('assignee_id') ELSE assignee_id END,
    assignment_group_id = CASE WHEN sqlc.arg('set_assignment_group')::bool THEN sqlc.narg('assignment_group_id')
                               ELSE assignment_group_id END,
    -- The whole object: the save works out the fields from the form and writes the result.
    fields      = COALESCE(sqlc.narg('fields')::jsonb, fields)
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: UpdateIncident :exec
UPDATE incidents SET resolution = sqlc.arg('resolution') WHERE task_id = sqlc.arg('task_id');

-- The whole change row: the save works out what changed and writes the result.
-- name: UpdateChange :exec
UPDATE changes SET change_type = sqlc.arg('change_type'), risk = sqlc.arg('risk'), impact = sqlc.arg('impact'),
    planned_start = sqlc.narg('planned_start'), planned_end = sqlc.narg('planned_end'),
    implementation_plan = sqlc.arg('implementation_plan'), backout_plan = sqlc.arg('backout_plan'),
    test_plan = sqlc.arg('test_plan')
WHERE task_id = sqlc.arg('task_id');

-- name: UpdateTaskStatus :one
UPDATE tasks SET status = $2, state_category = $3 WHERE id = $1 RETURNING *;

-- name: CreateComment :one
INSERT INTO task_comments (task_id, author_id, body, internal)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetCommentView :one
SELECT sqlc.embed(c), u.name AS author_name, b.name AS on_behalf_of_name
FROM task_comments c JOIN users u ON u.id = c.author_id LEFT JOIN users b ON b.id = c.on_behalf_of
WHERE c.id = $1;

-- name: ListComments :many
SELECT sqlc.embed(c), u.name AS author_name, b.name AS on_behalf_of_name
FROM task_comments c JOIN users u ON u.id = c.author_id LEFT JOIN users b ON b.id = c.on_behalf_of
WHERE c.task_id = $1
ORDER BY c.created_at, c.id;

-- The calendar: records whose planned window overlaps [from, to), as the caller may see them (RLS on tasks). Only
-- changes have a window so far. Ordered by start, so overlaps are found in one pass.
-- name: ListPlannedWindows :many
SELECT t.id, t.type, t.number, tt.prefix AS type_prefix, (tt.prefix || t.number::text)::text AS reference, t.title, t.status, t.state_category, t.priority,
       a.name AS assignee_name, c.change_type, c.risk,
       c.planned_start::timestamptz AS planned_start, c.planned_end::timestamptz AS planned_end
FROM changes c
JOIN tasks t ON t.id = c.task_id
JOIN task_types tt ON tt.type = t.type
LEFT JOIN users a ON a.id = t.assignee_id
WHERE c.planned_start IS NOT NULL AND c.planned_end IS NOT NULL
  AND c.planned_start < sqlc.arg('to')::timestamptz AND c.planned_end > sqlc.arg('from')::timestamptz
ORDER BY c.planned_start, t.number
LIMIT 501;

-- name: ListChildTasks :many
-- The tasks under a record that the reader may see, oldest first.
SELECT sqlc.embed(t), tt.prefix AS type_prefix, (tt.prefix || t.number::text)::text AS reference, a.name AS assignee_name, g.name AS assignment_group_name
FROM tasks t
JOIN task_types tt ON tt.type = t.type
LEFT JOIN users a ON a.id = t.assignee_id
LEFT JOIN groups g ON g.id = t.assignment_group_id
WHERE t.parent_id = $1
ORDER BY t.created_at, t.number;
