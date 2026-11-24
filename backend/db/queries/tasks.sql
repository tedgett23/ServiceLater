-- Tasks are the shared base of every record type; a type with typed columns adds an extension table
-- (incidents so far). See docs/apps-platform.md §9.2. Callers pass the acting user through db.WithUser, so RLS
-- on tasks (and, through it, on the extension tables and comments) decides what is visible.

-- The assignment group is written with the row, not after it: a before rule routes an end user's ticket as it is
-- filed, and only staff may update a task.
-- The id is chosen by the save path, which marks it as being saved first (app_mark_saving): a record filed into a
-- restricted group the filer is not in must still come back to them.
-- name: CreateTask :one
INSERT INTO tasks (id, type, title, description, priority, requester_id, status, state_category, assignment_group_id, fields, parent_id)
VALUES (COALESCE(sqlc.narg('id')::uuid, gen_random_uuid()), sqlc.arg('type'), sqlc.arg('title'), sqlc.arg('description'), sqlc.arg('priority'), sqlc.arg('requester_id'),
        sqlc.arg('status'), sqlc.arg('state_category'), sqlc.narg('assignment_group_id'),
        COALESCE(sqlc.narg('fields')::jsonb, '{}'::jsonb), sqlc.narg('parent_id'))
RETURNING *;

-- A record the caller is saving stays visible to them for the rest of the transaction (migration 00044).
-- name: MarkSaving :exec
SELECT app_mark_saving(sqlc.arg('id')::uuid);

-- Whether the caller sees a record now, leaving out the one-transaction allowance for records they are saving: after
-- a save that routed it to a restricted group they are not in, false.
-- name: StillSees :one
SELECT EXISTS (SELECT 1 FROM tasks t WHERE t.id = sqlc.arg('id')
               AND app_sees_task(t.id, t.type, t.requester_id, t.assignee_id, t.assignment_group_id, false))::bool;

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
       (SELECT count(*) FROM tasks k WHERE k.parent_id = t.id AND k.state_category IN ('open', 'waiting')) AS open_tasks,
       COALESCE(i.problem_id, c.problem_id) AS problem_id, COALESCE(prt.prefix || pr.number, '')::text AS problem_reference, COALESCE(pr.title, '')::text AS problem_title,
       (SELECT count(*) FROM incidents x JOIN tasks y ON y.id = x.task_id
         WHERE x.problem_id = t.id AND y.state_category IN ('open', 'waiting')) AS open_incidents
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
LEFT JOIN tasks pr ON pr.id = COALESCE(i.problem_id, c.problem_id)
LEFT JOIN task_types prt ON prt.type = pr.type
WHERE t.id = $1;

-- The queue works on tasks alone, whatever their type, so it never needs the extension tables.
-- Each row carries its SLA summary: the soonest due time of its running clocks, and whether any clock in force has
-- breached.
-- name: ListTickets :many
SELECT sqlc.embed(t), tt.prefix AS type_prefix, (tt.prefix || t.number::text)::text AS reference, r.name AS requester_name, a.name AS assignee_name,
       g.name AS assignment_group_name, sla.due_at::timestamptz AS sla_due_at, COALESCE(sla.breached, false)::bool AS sla_breached,
       -- A problem's open incidents and the changes made to fix it (the CASE keeps other rows from counting).
       (CASE WHEN t.type = 'problem' THEN (SELECT count(*) FROM incidents x JOIN tasks y ON y.id = x.task_id
              WHERE x.problem_id = t.id AND y.state_category IN ('open', 'waiting')) ELSE 0 END)::bigint AS open_incidents,
       (CASE WHEN t.type = 'problem' THEN (SELECT count(*) FROM changes x JOIN tasks y ON y.id = x.task_id
              WHERE x.problem_id = t.id) ELSE 0 END)::bigint AS linked_changes
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
  AND (sqlc.narg('fields')::jsonb IS NULL OR t.fields @> sqlc.narg('fields')::jsonb)
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

-- name: SetChangeProblem :exec
UPDATE changes SET problem_id = sqlc.narg('problem_id') WHERE task_id = sqlc.arg('task_id');

-- The changes made to fix a problem, as the reader may see them, oldest first.
-- name: ListProblemChanges :many
SELECT sqlc.embed(t), (tt.prefix || t.number::text)::text AS reference, a.name AS assignee_name, g.name AS assignment_group_name
FROM changes c
JOIN tasks t ON t.id = c.task_id
JOIN task_types tt ON tt.type = t.type
LEFT JOIN users a ON a.id = t.assignee_id
LEFT JOIN groups g ON g.id = t.assignment_group_id
WHERE c.problem_id = $1
ORDER BY t.created_at, t.number
LIMIT 500;

-- name: SetIncidentProblem :exec
UPDATE incidents SET problem_id = sqlc.narg('problem_id') WHERE task_id = sqlc.arg('task_id');

-- The incidents linked to a problem, as the reader may see them, oldest first.
-- name: ListProblemIncidents :many
SELECT sqlc.embed(t), (tt.prefix || t.number::text)::text AS reference, a.name AS assignee_name, g.name AS assignment_group_name
FROM incidents i
JOIN tasks t ON t.id = i.task_id
JOIN task_types tt ON tt.type = t.type
LEFT JOIN users a ON a.id = t.assignee_id
LEFT JOIN groups g ON g.id = t.assignment_group_id
WHERE i.problem_id = $1
ORDER BY t.created_at, t.number
LIMIT 500;

-- The open and waiting incidents linked to a problem, as the reader may see them: what resolving it acts on.
-- name: ListOpenProblemChangeIDs :many
SELECT t.id
FROM changes c
JOIN tasks t ON t.id = c.task_id
WHERE c.problem_id = $1 AND t.state_category IN ('open', 'waiting')
ORDER BY t.number;

-- name: ListOpenProblemIncidentIDs :many
SELECT t.id
FROM incidents i
JOIN tasks t ON t.id = i.task_id
WHERE i.problem_id = $1 AND t.state_category IN ('open', 'waiting')
ORDER BY t.number;

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

-- Who of these people may read a comment on the record (internal notes only with comments:internal): those a
-- comment may mention. Nobody deactivated, and no app.
-- name: MentionablePeople :many
SELECT u.id, u.name
FROM users u, tasks t
WHERE t.id = sqlc.arg('task_id') AND u.id = ANY (sqlc.arg('user_ids')::uuid[])
  AND u.app_name IS NULL AND u.deactivated_at IS NULL
  AND app_user_sees_task(u.id, t.type, t.requester_id, t.assignee_id, t.assignment_group_id)
  AND (NOT sqlc.arg('internal')::boolean OR app_user_can(u.id, 'comments:internal'))
ORDER BY u.name, u.id;

-- People whose name or email contains q and who may read a comment on the record: whom the comment box offers to
-- mention. q is escaped for LIKE by the caller.
-- name: MentionCandidates :many
SELECT u.id, u.name, u.email
FROM users u, tasks t
WHERE t.id = sqlc.arg('task_id')
  AND (u.name ILIKE '%' || sqlc.arg('q')::text || '%' ESCAPE '\' OR u.email ILIKE '%' || sqlc.arg('q')::text || '%' ESCAPE '\')
  AND u.app_name IS NULL AND u.deactivated_at IS NULL
  AND app_user_sees_task(u.id, t.type, t.requester_id, t.assignee_id, t.assignment_group_id)
  AND (NOT sqlc.arg('internal')::boolean OR app_user_can(u.id, 'comments:internal'))
ORDER BY u.name, u.id
LIMIT 6;

-- name: AddCommentMentions :exec
INSERT INTO task_comment_mentions (comment_id, user_id)
SELECT sqlc.arg('comment_id'), unnest(sqlc.arg('user_ids')::uuid[]);

-- The people a record's comments mention, as the reader may see the comments.
-- name: ListCommentMentions :many
SELECT m.comment_id, m.user_id, u.name
FROM task_comment_mentions m JOIN task_comments c ON c.id = m.comment_id JOIN users u ON u.id = m.user_id
WHERE c.task_id = sqlc.arg('task_id')
ORDER BY u.name, u.id;

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

-- How many records each view of the queue holds, for the reader (RLS decides what they can count). Only open and
-- waiting work is counted, which is what every view but All shows, so this reads the state_category index and never
-- the whole table. on_a_clock: records with an SLA clock running or breached, which is what the Due view puts first.
-- name: CountQueueViews :one
SELECT count(*) AS open,
       count(*) FILTER (WHERE t.assignee_id = app_current_user_id()) AS mine,
       count(*) FILTER (WHERE t.assignee_id IS NULL) AS unassigned,
       count(*) FILTER (WHERE t.assignment_group_id IN
           (SELECT m.group_id FROM group_members m WHERE m.user_id = app_current_user_id())) AS my_groups,
       count(*) FILTER (WHERE EXISTS (
           SELECT 1 FROM task_slas s
           WHERE s.task_id = t.id AND s.stage <> 'cancelled'
             AND (s.breached_at IS NOT NULL OR (s.stage = 'running' AND s.due_at IS NOT NULL)))) AS on_a_clock
FROM tasks t
JOIN task_types tt ON tt.type = t.type
WHERE tt.retired_at IS NULL AND t.state_category IN ('open', 'waiting');

-- Who may hear that a record changed, when it is routed to a restricted group (read by the realtime hub as the
-- system): the group's members and the assignee. restricted is false for every other record, and nothing more is
-- needed then.
-- name: TaskAudience :one
SELECT COALESCE(g.restricted, false)::bool AS restricted, t.assignee_id,
       COALESCE(ARRAY(SELECT m.user_id FROM group_members m WHERE m.group_id = g.id), '{}')::uuid[] AS members
FROM tasks t LEFT JOIN groups g ON g.id = t.assignment_group_id
WHERE t.id = $1;

-- Open records of a type that look related to a text (and, optionally, to some configuration items): candidates share
-- a word, a near spelling or an item; each is scored by app_match_score plus 0.5 for a shared item. Runs as the
-- caller, so it suggests only what they may see. unlinked keeps incidents not linked to a problem yet.
-- name: SuggestRecords :many
SELECT sqlc.embed(t), (tt.prefix || t.number::text)::text AS reference, a.name AS assignee_name, g.name AS assignment_group_name,
       shared.yes::bool AS shared_item,
       (app_match_score(sqlc.arg('text')::text, t.title) + CASE WHEN shared.yes THEN 0.5 ELSE 0 END)::float8 AS score
FROM tasks t
JOIN task_types tt ON tt.type = t.type
LEFT JOIN users a ON a.id = t.assignee_id
LEFT JOIN groups g ON g.id = t.assignment_group_id
CROSS JOIN LATERAL (SELECT EXISTS (SELECT 1 FROM task_cis x WHERE x.task_id = t.id AND x.ci_id = ANY (sqlc.arg('cis')::uuid[])) AS yes) shared
WHERE t.type = sqlc.arg('type')::text AND t.state_category IN ('open', 'waiting')
  AND t.id <> sqlc.arg('exclude')::uuid
  AND (NOT sqlc.arg('unlinked')::bool OR NOT EXISTS (SELECT 1 FROM incidents i WHERE i.task_id = t.id AND i.problem_id IS NOT NULL))
  AND (to_tsvector('english'::regconfig, t.title) @@ app_any_word_query(sqlc.arg('text')::text)
       OR t.title % sqlc.arg('text')::text OR shared.yes)
ORDER BY score DESC, t.number DESC
LIMIT 20;
