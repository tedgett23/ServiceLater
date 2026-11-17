-- name: ListHistory :many
-- A record's or an item's history, newest first, with who did it (an app's principal has app_name).
SELECT h.id, h.at, h.actor_id, a.name AS actor_name, a.app_name AS actor_app, h.on_behalf_of, b.name AS on_behalf_of_name,
       h.action, h.field, h.old, h.new
FROM record_history h
LEFT JOIN users a ON a.id = h.actor_id
LEFT JOIN users b ON b.id = h.on_behalf_of
WHERE h.subject = sqlc.arg('subject') AND h.subject_id = sqlc.arg('subject_id')
ORDER BY h.id DESC
LIMIT sqlc.arg('limit');

-- name: UserNamesByID :many
SELECT id, name FROM users WHERE id = ANY (sqlc.arg('ids')::uuid[]);

-- name: GroupNamesByID :many
SELECT id, name FROM groups WHERE id = ANY (sqlc.arg('ids')::uuid[]);

-- name: ArticleTitlesByID :many
-- The articles the reader may see.
SELECT id, number, title FROM kb_articles WHERE id = ANY (sqlc.arg('ids')::uuid[]);

-- name: TaskRefsByID :many
-- The records the reader may see, by reference.
SELECT t.id, (tt.prefix || t.number::text)::text AS reference, t.title FROM tasks t JOIN task_types tt ON tt.type = t.type WHERE t.id = ANY (sqlc.arg('ids')::uuid[]);

-- name: DeleteHistoryExclusions :exec
-- System only: an install replaces the app's history exclusions.
DELETE FROM history_exclusions WHERE app_name = $1;

-- name: InsertHistoryExclusion :exec
INSERT INTO history_exclusions (app_name, subject, kind, field) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING;

-- name: ListHistoryExclusions :many
SELECT app_name, subject, kind, field FROM history_exclusions ORDER BY subject, kind, field;

-- name: ListSettingHistory :many
-- Changes to the settings recorded in history (who turned what on or off), newest first.
SELECT h.id, h.at, a.name AS actor_name, h.field, h.old, h.new
FROM record_history h LEFT JOIN users a ON a.id = h.actor_id
WHERE h.subject = 'setting' AND h.field LIKE sqlc.arg('prefix')::text || '%'
ORDER BY h.id DESC
LIMIT 100;
