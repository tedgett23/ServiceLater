-- +goose Up

-- Global search (GET /search): finding a record, article or item by a few letters of its name, typos included.
-- pg_trgm's indexes serve both "contains" (ILIKE) and "looks like" (word similarity); it ships with Postgres and is a
-- trusted extension, so the database's owner may create it.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX tasks_title_trgm_idx ON tasks USING gin (title gin_trgm_ops);
CREATE INDEX kb_articles_title_trgm_idx ON kb_articles USING gin (title gin_trgm_ops);
CREATE INDEX cis_name_trgm_idx ON cis USING gin (name gin_trgm_ops);

-- +goose Down
DROP INDEX cis_name_trgm_idx;
DROP INDEX kb_articles_title_trgm_idx;
DROP INDEX tasks_title_trgm_idx;
DROP EXTENSION pg_trgm;
