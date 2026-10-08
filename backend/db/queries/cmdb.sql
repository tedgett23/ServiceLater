-- name: ListCIs :many
-- Items by name, filtered. classes is the class asked for and every class that extends it.
SELECT c.id, c.class, c.name, c.status, c.owner_group_id, og.name AS owner_group_name,
       c.support_group_id, sg.name AS support_group_name, c.updated_at
FROM cis c
LEFT JOIN groups og ON og.id = c.owner_group_id
LEFT JOIN groups sg ON sg.id = c.support_group_id
WHERE (sqlc.narg('classes')::text[] IS NULL OR c.class = ANY (sqlc.narg('classes')::text[]))
  AND (sqlc.narg('status')::text IS NULL OR c.status = sqlc.narg('status'))
  AND (NOT sqlc.arg('in_use')::bool OR c.status <> 'retired')
  AND (sqlc.narg('support_group_id')::uuid IS NULL OR c.support_group_id = sqlc.narg('support_group_id'))
  AND (sqlc.narg('search')::text IS NULL OR c.name ILIKE '%' || sqlc.narg('search') || '%' ESCAPE '\')
ORDER BY lower(c.name), c.class, c.id
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: GetCI :one
SELECT c.id, c.class, c.name, c.status, c.description, c.owner_group_id, og.name AS owner_group_name,
       c.support_group_id, sg.name AS support_group_name, c.fields, c.created_by, u.name AS created_by_name,
       c.created_at, c.updated_at
FROM cis c
LEFT JOIN groups og ON og.id = c.owner_group_id
LEFT JOIN groups sg ON sg.id = c.support_group_id
LEFT JOIN users u ON u.id = c.created_by
WHERE c.id = $1;

-- name: CreateCI :one
INSERT INTO cis (class, name, status, description, owner_group_id, support_group_id, fields, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, app_current_user_id())
RETURNING id;

-- name: UpdateCI :one
UPDATE cis SET class = $2, name = $3, status = $4, description = $5, owner_group_id = $6, support_group_id = $7,
    fields = $8, updated_at = now()
WHERE id = $1
RETURNING id;

-- name: DeleteCI :execrows
DELETE FROM cis WHERE id = $1;

-- name: GetCIsByID :many
-- The items with these ids that the reader may see, for impact results.
SELECT c.id, c.class, c.name, c.status, sg.name AS support_group_name
FROM cis c
LEFT JOIN groups sg ON sg.id = c.support_group_id
WHERE c.id = ANY (sqlc.arg('ids')::uuid[]);

-- name: ListCIRelationships :many
-- Every relationship an item is in, either way round, with the item at the other end.
SELECT r.id, r.type, (r.from_ci = sqlc.arg('ci_id'))::bool AS outgoing, o.id AS other_id, o.name AS other_name,
       o.class AS other_class, o.status AS other_status
FROM ci_relationships r
JOIN cis o ON o.id = CASE WHEN r.from_ci = sqlc.arg('ci_id') THEN r.to_ci ELSE r.from_ci END
WHERE r.from_ci = sqlc.arg('ci_id') OR r.to_ci = sqlc.arg('ci_id')
ORDER BY r.type, lower(o.name)
LIMIT 501;

-- name: CreateCIRelationship :one
INSERT INTO ci_relationships (from_ci, to_ci, type, created_by)
VALUES ($1, $2, $3, app_current_user_id())
RETURNING id;

-- name: DeleteCIRelationship :execrows
DELETE FROM ci_relationships WHERE id = sqlc.arg('id') AND sqlc.arg('ci_id')::uuid IN (from_ci, to_ci);

-- name: ImpactStep :many
-- One step of an impact walk: the items that relate, by one of the impact types, to any of these items.
SELECT r.from_ci, r.to_ci
FROM ci_relationships r
WHERE r.to_ci = ANY (sqlc.arg('ids')::uuid[]) AND r.type = ANY (sqlc.arg('types')::text[])
LIMIT 5001;

-- name: ListTaskCIs :many
-- The items linked to a record.
SELECT c.id, c.class, c.name, c.status, sg.name AS support_group_name
FROM task_cis l
JOIN cis c ON c.id = l.ci_id
LEFT JOIN groups sg ON sg.id = c.support_group_id
WHERE l.task_id = $1
ORDER BY lower(c.name), c.id;

-- name: LinkTaskCI :exec
INSERT INTO task_cis (task_id, ci_id, created_by) VALUES ($1, $2, app_current_user_id())
ON CONFLICT DO NOTHING;

-- name: UnlinkTaskCI :execrows
DELETE FROM task_cis WHERE task_id = $1 AND ci_id = $2;

-- name: CountTaskCIs :one
SELECT count(*) FROM task_cis WHERE task_id = $1;

-- name: ListCITasks :many
-- The records an item is linked to that the reader may see: open ones first, then the most recently changed.
SELECT t.id, t.type, tt.prefix, t.number, t.title, t.status, t.state_category, t.priority, t.updated_at
FROM task_cis l
JOIN tasks t ON t.id = l.task_id
JOIN task_types tt ON tt.type = t.type
WHERE l.ci_id = $1
ORDER BY t.state_category IN ('open', 'waiting') DESC, t.updated_at DESC, t.id
LIMIT 101;
