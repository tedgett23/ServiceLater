-- Groups and their members. Everyone signed in may read them; RLS lets only admins write them.

-- name: ListGroups :many
SELECT g.*, (SELECT count(*) FROM group_members m WHERE m.group_id = g.id) AS member_count
FROM groups g
ORDER BY lower(g.name);

-- name: GetGroup :one
SELECT g.*, (SELECT count(*) FROM group_members m WHERE m.group_id = g.id) AS member_count
FROM groups g WHERE g.id = $1;

-- name: GetGroupByName :one
SELECT * FROM groups WHERE lower(name) = lower(sqlc.arg('name')::text);

-- name: CreateGroup :one
INSERT INTO groups (name, description) VALUES ($1, $2) RETURNING *;

-- name: UpdateGroup :one
UPDATE groups SET
    name        = COALESCE(sqlc.narg('name'), name),
    description = COALESCE(sqlc.narg('description'), description)
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: DeleteGroup :execrows
DELETE FROM groups WHERE id = $1;

-- name: ListGroupMembers :many
SELECT u.id, u.email, u.name, u.role, u.deactivated_at, m.added_at
FROM group_members m JOIN users u ON u.id = m.user_id
WHERE m.group_id = $1
ORDER BY u.name, u.id;

-- Adding someone already in the group changes nothing.
-- name: AddGroupMember :exec
INSERT INTO group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: RemoveGroupMember :execrows
DELETE FROM group_members WHERE group_id = $1 AND user_id = $2;

-- name: IsGroupMember :one
SELECT EXISTS (SELECT 1 FROM group_members WHERE group_id = $1 AND user_id = $2);

-- What group('name') in an expression resolves to. Deactivated members are not asked anything.
-- name: ListGroupMemberIDsByName :many
SELECT m.user_id FROM group_members m JOIN groups g ON g.id = m.group_id JOIN users u ON u.id = m.user_id
WHERE lower(g.name) = lower(sqlc.arg('name')::text) AND u.deactivated_at IS NULL
ORDER BY m.user_id;

-- name: ListUserGroups :many
SELECT g.* FROM groups g JOIN group_members m ON m.group_id = g.id
WHERE m.user_id = $1
ORDER BY lower(g.name);

-- Who a notify action's assignment_group reaches: the group's members who can still sign in.
-- name: ListActiveGroupMemberIDs :many
SELECT m.user_id FROM group_members m JOIN users u ON u.id = m.user_id
WHERE m.group_id = $1 AND u.deactivated_at IS NULL
ORDER BY m.user_id;
