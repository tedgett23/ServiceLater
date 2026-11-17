-- name: SearchArticles :many
-- Articles the reader may see, matching a search (websearch syntax: words, "phrases", -not) ranked best first, or the
-- latest changed without one. statuses limits which (published, unless a writer asks for others). The snippet marks
-- matches with ⸢ and ⸣, which the web turns into highlights without trusting any HTML.
SELECT a.id, a.number, a.title, a.category, a.tags, a.audience, a.status, a.updated_at,
       (CASE WHEN sqlc.narg('q')::text IS NULL THEN 0
             ELSE ts_rank(a.search, websearch_to_tsquery('english', sqlc.narg('q')::text)) END)::real AS rank,
       (CASE WHEN sqlc.narg('q')::text IS NULL THEN left(a.body, 240)
             ELSE ts_headline('english', a.body, websearch_to_tsquery('english', sqlc.narg('q')::text),
                              'StartSel=⸢, StopSel=⸣, MaxFragments=2, MaxWords=28, MinWords=12, FragmentDelimiter=" … "') END)::text AS snippet
FROM kb_articles a
WHERE a.status = ANY (sqlc.arg('statuses')::text[])
  AND (sqlc.narg('q')::text IS NULL OR a.search @@ websearch_to_tsquery('english', sqlc.narg('q')::text))
  AND (sqlc.narg('category')::text IS NULL OR lower(a.category) = lower(sqlc.narg('category')::text))
ORDER BY rank DESC, a.updated_at DESC, a.id
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: GetArticle :one
SELECT a.id, a.number, a.title, a.body, a.category, a.tags, a.audience, a.status, a.version, a.author_id, a.updated_by,
       a.created_at, a.updated_at, a.published_at, au.name AS author_name, ub.name AS updated_by_name
FROM kb_articles a
LEFT JOIN users au ON au.id = a.author_id
LEFT JOIN users ub ON ub.id = a.updated_by
WHERE a.id = $1;

-- name: GetArticleIDByNumber :one
SELECT id FROM kb_articles WHERE number = $1;

-- name: CreateArticle :one
INSERT INTO kb_articles (title, body, category, tags, audience, status, author_id, updated_by, published_at)
VALUES ($1, $2, $3, $4, $5, $6, app_current_user_id(), app_current_user_id(),
        CASE WHEN $6 = 'published' THEN now() END)
RETURNING id;

-- name: UpdateArticleContent :one
UPDATE kb_articles SET title = $2, body = $3, category = $4, tags = $5, audience = $6, version = version + 1,
    updated_by = app_current_user_id(), updated_at = now()
WHERE id = $1
RETURNING version;

-- name: SetArticleStatus :one
UPDATE kb_articles SET status = sqlc.arg('status'), updated_by = app_current_user_id(), updated_at = now(),
    published_at = CASE WHEN sqlc.arg('status') = 'published' THEN now() ELSE published_at END
WHERE id = sqlc.arg('id')
RETURNING id;

-- name: DeleteArticle :execrows
DELETE FROM kb_articles WHERE id = $1;

-- name: SaveArticleVersion :exec
-- The article's content as it now is, as the next version.
INSERT INTO kb_article_versions (article_id, version, title, body, category, tags, audience, status, saved_by)
SELECT a.id, a.version, a.title, a.body, a.category, a.tags, a.audience, a.status, app_current_user_id()
FROM kb_articles a WHERE a.id = $1;

-- name: ListArticleVersions :many
SELECT v.version, v.title, v.status, v.saved_at, u.name AS saved_by_name
FROM kb_article_versions v
LEFT JOIN users u ON u.id = v.saved_by
WHERE v.article_id = $1
ORDER BY v.version DESC
LIMIT 200;

-- name: GetArticleVersion :one
SELECT v.*, u.name AS saved_by_name
FROM kb_article_versions v
LEFT JOIN users u ON u.id = v.saved_by
WHERE v.article_id = $1 AND v.version = $2;

-- name: ListArticleCategories :many
-- The categories of the published articles the reader may see, with how many each has.
SELECT a.category, count(*) AS articles
FROM kb_articles a
WHERE a.status = 'published' AND a.category <> ''
GROUP BY a.category
ORDER BY lower(a.category)
LIMIT 200;

-- name: ListTaskArticles :many
-- The articles linked to a record that the reader may see.
SELECT a.id, a.number, a.title, a.status, a.audience
FROM task_articles l
JOIN kb_articles a ON a.id = l.article_id
WHERE l.task_id = $1
ORDER BY l.created_at, a.id;

-- name: LinkTaskArticle :exec
INSERT INTO task_articles (task_id, article_id, created_by) VALUES ($1, $2, app_current_user_id())
ON CONFLICT DO NOTHING;

-- name: UnlinkTaskArticle :execrows
DELETE FROM task_articles WHERE task_id = $1 AND article_id = $2;

-- name: ListArticleTasks :many
-- The records an article is linked to that the reader may see, latest first.
SELECT t.id, (tt.prefix || t.number::text)::text AS reference, t.number, t.type, t.title, t.status, t.state_category
FROM task_articles l
JOIN tasks t ON t.id = l.task_id
JOIN task_types tt ON tt.type = t.type
WHERE l.article_id = $1 AND tt.retired_at IS NULL
ORDER BY l.created_at DESC
LIMIT 50;
