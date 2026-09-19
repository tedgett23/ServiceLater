-- name: CreateTicket :one
INSERT INTO tickets (title, description, priority, requester_id)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetTicket :one
SELECT * FROM tickets WHERE id = $1;

-- name: GetTicketByNumber :one
SELECT * FROM tickets WHERE number = $1;

-- Ticket plus display names. Plain joins, not a view: views run with their owner's rights and skip RLS.
-- name: GetTicketView :one
SELECT sqlc.embed(t), r.name AS requester_name, a.name AS assignee_name
FROM tickets t
JOIN users r ON r.id = t.requester_id
LEFT JOIN users a ON a.id = t.assignee_id
WHERE t.id = $1;

-- name: GetTicketViewByNumber :one
SELECT sqlc.embed(t), r.name AS requester_name, a.name AS assignee_name
FROM tickets t
JOIN users r ON r.id = t.requester_id
LEFT JOIN users a ON a.id = t.assignee_id
WHERE t.number = $1;

-- name: GetTicketForUpdate :one
SELECT * FROM tickets WHERE id = $1 FOR UPDATE;

-- name: ListTickets :many
SELECT sqlc.embed(t), r.name AS requester_name, a.name AS assignee_name
FROM tickets t
JOIN users r ON r.id = t.requester_id
LEFT JOIN users a ON a.id = t.assignee_id
WHERE (sqlc.narg('status')::text IS NULL OR t.status = sqlc.narg('status'))
  AND (sqlc.narg('priority')::smallint IS NULL OR t.priority = sqlc.narg('priority'))
  AND (sqlc.narg('assignee_id')::uuid IS NULL OR t.assignee_id = sqlc.narg('assignee_id'))
  AND (sqlc.narg('requester_id')::uuid IS NULL OR t.requester_id = sqlc.narg('requester_id'))
  AND (NOT sqlc.arg('open_only')::bool OR t.status NOT IN ('resolved', 'closed'))
  AND (NOT sqlc.arg('unassigned')::bool OR t.assignee_id IS NULL)
  AND (sqlc.narg('search')::text IS NULL OR t.title ILIKE '%' || sqlc.narg('search') || '%' ESCAPE '\')
ORDER BY t.created_at DESC, t.id DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: UpdateTicket :one
UPDATE tickets SET
    title       = COALESCE(sqlc.narg('title'), title),
    description = COALESCE(sqlc.narg('description'), description),
    priority    = COALESCE(sqlc.narg('priority'), priority),
    assignee_id = CASE WHEN sqlc.arg('set_assignee')::bool THEN sqlc.narg('assignee_id') ELSE assignee_id END
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: UpdateTicketStatus :one
UPDATE tickets SET status = $2 WHERE id = $1 RETURNING *;

-- name: CreateComment :one
INSERT INTO ticket_comments (ticket_id, author_id, body, internal)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetCommentView :one
SELECT sqlc.embed(c), u.name AS author_name
FROM ticket_comments c JOIN users u ON u.id = c.author_id
WHERE c.id = $1;

-- name: ListComments :many
SELECT sqlc.embed(c), u.name AS author_name
FROM ticket_comments c JOIN users u ON u.id = c.author_id
WHERE c.ticket_id = $1
ORDER BY c.created_at, c.id;
