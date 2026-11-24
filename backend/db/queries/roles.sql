-- Roles and who has them (migration 00004_access). Roles are written by app install (as the owner); grants by admins
-- under RLS.

-- name: UpsertAppRole :exec
INSERT INTO roles (name, label, description, permissions, app_name)
VALUES (sqlc.arg('name'), sqlc.arg('label'), sqlc.arg('description'), sqlc.arg('permissions')::text[], sqlc.narg('app_name'))
ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
    permissions = EXCLUDED.permissions, updated_at = now()
WHERE roles.app_name = EXCLUDED.app_name;

-- name: DeleteAppRole :exec
DELETE FROM roles WHERE name = sqlc.arg('name') AND app_name = sqlc.narg('app_name');

-- name: ListRoles :many
SELECT r.name, r.label, r.description, r.permissions, r.app_name,
       (SELECT count(*) FROM role_grants g WHERE g.role = r.name) AS grants
FROM roles r
ORDER BY r.app_name NULLS FIRST, r.label;

-- name: GetRole :one
SELECT name, label, description, permissions, app_name FROM roles WHERE name = $1;

-- name: ListRoleGrants :many
SELECT g.id, g.role, g.user_id, u.name AS user_name, u.email AS user_email, g.group_id, gr.name AS group_name, g.created_at
FROM role_grants g
LEFT JOIN users u ON u.id = g.user_id
LEFT JOIN groups gr ON gr.id = g.group_id
WHERE g.role = $1
ORDER BY gr.name NULLS LAST, u.name;

-- name: GrantRole :one
INSERT INTO role_grants (role, user_id, group_id, granted_by)
VALUES (sqlc.arg('role'), sqlc.narg('user_id'), sqlc.narg('group_id'), app_current_user_id())
RETURNING id, created_at;

-- name: RevokeRoleGrant :execrows
DELETE FROM role_grants WHERE id = sqlc.arg('id') AND role = sqlc.arg('role');

-- A person's grants: their own, and their groups' (group_name set for those).
-- name: ListUserRoles :many
SELECT g.id, g.role, r.label, g.group_id, gr.name AS group_name
FROM role_grants g
JOIN roles r ON r.name = g.role
LEFT JOIN groups gr ON gr.id = g.group_id
WHERE g.user_id = $1 OR g.group_id IN (SELECT m.group_id FROM group_members m WHERE m.user_id = $1)
ORDER BY r.label, gr.name NULLS FIRST;

-- name: ListGroupRoles :many
SELECT g.id, g.role, r.label
FROM role_grants g JOIN roles r ON r.name = g.role
WHERE g.group_id = $1
ORDER BY r.label;

-- What role('name') in a workflow resolves to: active people holding it as their base role, directly, or through a
-- group.
-- name: ListRoleHolderIDs :many
SELECT u.id FROM users u
WHERE u.deactivated_at IS NULL AND u.app_name IS NULL
  AND (u.role = sqlc.arg('role')
       OR EXISTS (SELECT 1 FROM role_grants g WHERE g.role = sqlc.arg('role')
                  AND (g.user_id = u.id OR g.group_id IN (SELECT m.group_id FROM group_members m WHERE m.user_id = u.id))))
ORDER BY u.id;

-- name: InAudience :one
-- Whether a person holds any of the roles (as their base role, directly, or through a group) or is in any of the groups
-- (names lowercased): who may see and order a catalog item.
SELECT EXISTS (
    SELECT 1 FROM users u WHERE u.id = sqlc.arg('user_id') AND (
        u.role = ANY (sqlc.arg('roles')::text[])
        OR EXISTS (SELECT 1 FROM role_grants g WHERE g.role = ANY (sqlc.arg('roles')::text[])
                   AND (g.user_id = u.id OR g.group_id IN (SELECT m.group_id FROM group_members m WHERE m.user_id = u.id)))
        OR EXISTS (SELECT 1 FROM group_members m JOIN groups gr ON gr.id = m.group_id
                   WHERE m.user_id = u.id AND lower(gr.name) = ANY (sqlc.arg('groups')::text[]))));
