-- Global search. Every query runs as the reader, so row-level security decides what can be found.

-- name: SearchRecordsByNumber :many
-- Records of one type whose number starts with these digits: INC4 finds INC4, then INC40 to INC49, then INC400 and
-- on. Each step is a range on the (type, number) index, never a scan.
SELECT t.id, t.type, tt.label AS type_label, (tt.prefix || t.number::text)::text AS reference, t.title, t.status, t.state_category
FROM generate_series(0, 6) AS e
JOIN LATERAL (
    SELECT k.* FROM tasks k
    WHERE k.type = sqlc.arg('type')
      AND k.number BETWEEN sqlc.arg('digits')::bigint * (10 ^ e)::bigint AND (sqlc.arg('digits')::bigint + 1) * (10 ^ e)::bigint - 1
    ORDER BY k.number
    LIMIT sqlc.arg('limit')
) t ON true
JOIN task_types tt ON tt.type = t.type
ORDER BY e, t.number
LIMIT sqlc.arg('limit');

-- name: LatestRecordsOfType :many
-- The newest records of a type: what a prefix alone (INC) offers while the number is still to come.
SELECT t.id, t.type, tt.label AS type_label, (tt.prefix || t.number::text)::text AS reference, t.title, t.status, t.state_category
FROM tasks t JOIN task_types tt ON tt.type = t.type
WHERE t.type = sqlc.arg('type')
ORDER BY t.number DESC
LIMIT sqlc.arg('limit');

-- name: SearchRecords :many
-- Records whose title contains the text, then those whose title has a word that looks like it (a typo, a part of a
-- word), the closest first. Retired types are left out, as everywhere.
SELECT t.id, t.type, tt.label AS type_label, (tt.prefix || t.number::text)::text AS reference, t.title, t.status, t.state_category
FROM tasks t JOIN task_types tt ON tt.type = t.type
WHERE tt.retired_at IS NULL
  AND (t.title ILIKE '%' || sqlc.arg('contains')::text || '%' ESCAPE '\' OR sqlc.arg('q')::text <% t.title)
ORDER BY (t.title ILIKE '%' || sqlc.arg('contains')::text || '%' ESCAPE '\') DESC,
         word_similarity(sqlc.arg('q')::text, t.title) DESC, t.updated_at DESC, t.id
LIMIT sqlc.arg('limit');

-- name: SearchArticlesByNumber :many
-- Articles whose number starts with these digits (KB1: KB1, KB10 ...), or the latest when there are none yet.
SELECT a.id, a.number, a.title, a.status
FROM kb_articles a
WHERE sqlc.arg('digits')::text = '' OR a.number::text LIKE sqlc.arg('digits')::text || '%'
ORDER BY length(a.number::text), a.number DESC
LIMIT sqlc.arg('limit');

-- name: SearchArticlesAnywhere :many
-- Articles by title (contains, then looks like), then by their text.
SELECT a.id, a.number, a.title, a.status
FROM kb_articles a
WHERE a.title ILIKE '%' || sqlc.arg('contains')::text || '%' ESCAPE '\'
   OR sqlc.arg('q')::text <% a.title
   OR a.search @@ websearch_to_tsquery('english', sqlc.arg('q')::text)
ORDER BY (a.title ILIKE '%' || sqlc.arg('contains')::text || '%' ESCAPE '\') DESC,
         word_similarity(sqlc.arg('q')::text, a.title) DESC, a.updated_at DESC, a.id
LIMIT sqlc.arg('limit');

-- name: SearchCIs :many
-- Configuration items by name (contains, then looks like), for those who read the CMDB.
SELECT c.id, c.name, c.class, c.status
FROM cis c
WHERE c.name ILIKE '%' || sqlc.arg('contains')::text || '%' ESCAPE '\' OR sqlc.arg('q')::text <% c.name
ORDER BY (c.name ILIKE '%' || sqlc.arg('contains')::text || '%' ESCAPE '\') DESC,
         word_similarity(sqlc.arg('q')::text, c.name) DESC, c.name, c.id
LIMIT sqlc.arg('limit');
