-- Tasks are the shared base of every record type; a type with typed columns adds an extension table
-- (incidents so far). See docs/apps-platform.md §9.2. Callers pass the acting user through db.WithUser, so RLS
-- on tasks (and, through it, on the extension tables and comments) decides what is visible.

-- name: CreateTask :one
INSERT INTO tasks (type, title, description, priority, requester_id, status, state_category)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- The extension row for an incident. Every incident has one; the composite key guarantees the task really is one.
-- name: CreateIncident :exec
INSERT INTO incidents (task_id) VALUES ($1);

-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1;

-- A reference is a type's prefix and its number, such as INC42.
-- name: GetTaskByReference :one
SELECT t.* FROM tasks t JOIN task_types tt ON tt.type = t.type WHERE tt.prefix = $1 AND t.number = $2;

-- name: GetTaskByTypeNumber :one
SELECT * FROM tasks WHERE type = $1 AND number = $2;

-- name: GetTaskForUpdate :one
SELECT * FROM tasks WHERE id = $1 FOR UPDATE;

-- name: ListTaskTypes :many
SELECT * FROM task_types ORDER BY type;

-- A ticket as clients see it: the task, display names, the type's prefix, and the type's own columns. Plain joins,
-- not a view: views run with their owner's rights and skip RLS. Each type with an extension table adds one LEFT JOIN.
-- name: GetTicketView :one
SELECT sqlc.embed(t), tt.prefix AS type_prefix, r.name AS requester_name, a.name AS assignee_name,
       COALESCE(i.resolution, '')::text AS resolution
FROM tasks t
JOIN task_types tt ON tt.type = t.type
JOIN users r ON r.id = t.requester_id
LEFT JOIN users a ON a.id = t.assignee_id
LEFT JOIN incidents i ON i.task_id = t.id
WHERE t.id = $1;

-- The queue works on tasks alone, whatever their type, so it never needs the extension tables.
-- name: ListTickets :many
SELECT sqlc.embed(t), tt.prefix AS type_prefix, r.name AS requester_name, a.name AS assignee_name
FROM tasks t
JOIN task_types tt ON tt.type = t.type
JOIN users r ON r.id = t.requester_id
LEFT JOIN users a ON a.id = t.assignee_id
WHERE (sqlc.narg('type')::text IS NULL OR t.type = sqlc.narg('type'))
  AND (sqlc.narg('status')::text IS NULL OR t.status = sqlc.narg('status'))
  AND (sqlc.narg('priority')::smallint IS NULL OR t.priority = sqlc.narg('priority'))
  AND (sqlc.narg('assignee_id')::uuid IS NULL OR t.assignee_id = sqlc.narg('assignee_id'))
  AND (sqlc.narg('requester_id')::uuid IS NULL OR t.requester_id = sqlc.narg('requester_id'))
  AND (NOT sqlc.arg('open_only')::bool OR t.state_category IN ('open', 'waiting'))
  AND (NOT sqlc.arg('unassigned')::bool OR t.assignee_id IS NULL)
  AND (sqlc.narg('search')::text IS NULL OR t.title ILIKE '%' || sqlc.narg('search') || '%' ESCAPE '\')
ORDER BY t.created_at DESC, t.id DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: UpdateTask :one
UPDATE tasks SET
    title       = COALESCE(sqlc.narg('title'), title),
    description = COALESCE(sqlc.narg('description'), description),
    priority    = COALESCE(sqlc.narg('priority'), priority),
    assignee_id = CASE WHEN sqlc.arg('set_assignee')::bool THEN sqlc.narg('assignee_id') ELSE assignee_id END
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: UpdateIncident :exec
UPDATE incidents SET resolution = sqlc.arg('resolution') WHERE task_id = sqlc.arg('task_id');

-- name: UpdateTaskStatus :one
UPDATE tasks SET status = $2, state_category = $3 WHERE id = $1 RETURNING *;

-- name: CreateComment :one
INSERT INTO task_comments (task_id, author_id, body, internal)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetCommentView :one
SELECT sqlc.embed(c), u.name AS author_name
FROM task_comments c JOIN users u ON u.id = c.author_id
WHERE c.id = $1;

-- name: ListComments :many
SELECT sqlc.embed(c), u.name AS author_name
FROM task_comments c JOIN users u ON u.id = c.author_id
WHERE c.task_id = $1
ORDER BY c.created_at, c.id;
