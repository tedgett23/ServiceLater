-- +goose Up
-- Finding things: global search (GET /search: by reference, then titles that contain or look like the words, then
-- form fields and descriptions), and suggested links between incidents and problems. Everything runs as the caller,
-- under RLS; these are the functions and indexes that make it fast.

-- Titles and names that contain a few letters, or look like them (pg_trgm, foundation).
CREATE INDEX tasks_title_trgm_idx ON tasks USING gin (title gin_trgm_ops);
CREATE INDEX kb_articles_title_trgm_idx ON kb_articles USING gin (title gin_trgm_ops);
CREATE INDEX cis_name_trgm_idx ON cis USING gin (name gin_trgm_ops);

-- Suggested links between incidents and problems (docs/apps-platform.md §9.7): how alike two titles are, from 0 to 1.
-- The better of two measures: the share of words (stemmed, stop words left out) the shorter title has in common with
-- the other ("VPN drops every 20 minutes" and "VPN concentrator drops idle sessions" share vpn and drop), and trigram
-- word similarity, which catches near spellings ("print" and "Printers"). Callers add their own signals on top
-- (records linked to the same configuration item).
-- +goose StatementBegin
CREATE FUNCTION app_match_score(a text, b text) RETURNS float8
    LANGUAGE sql IMMUTABLE PARALLEL SAFE SET search_path = public
    AS $$
    SELECT GREATEST(
        COALESCE((SELECT count(*) FROM unnest(s.wa) w WHERE w = ANY (s.wb))::float8
                 / NULLIF(LEAST(cardinality(s.wa), cardinality(s.wb)), 0), 0),
        word_similarity(a, b), word_similarity(b, a))
    FROM (SELECT tsvector_to_array(to_tsvector('english'::regconfig, a)) AS wa,
                 tsvector_to_array(to_tsvector('english'::regconfig, b)) AS wb) s
$$;
-- +goose StatementEnd

-- Any of the words of a text, as a query: what narrows the candidates before they are scored.
-- +goose StatementBegin
CREATE FUNCTION app_any_word_query(q text) RETURNS tsquery
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
    SELECT websearch_to_tsquery('english'::regconfig,
        array_to_string(regexp_split_to_array(btrim(regexp_replace(q, '[^[:alnum:]]+', ' ', 'g')), '\s+'), ' or '))
$$;
-- +goose StatementEnd

-- Finds titles sharing a word without reading every record.
CREATE INDEX tasks_title_words_idx ON tasks USING gin (to_tsvector('english'::regconfig, title));

-- Global search reads more than titles (docs/apps-platform.md §9.5): a record's description and the text of its form's
-- fields (an app type's own fields, a problem's root cause and workaround, a serial number). Only text values count:
-- numbers, dates and yes/no answers say nothing to search for.

-- The text values of a record's extra fields, in field-name order. IMMUTABLE so it can be indexed.
-- +goose StatementBegin
CREATE FUNCTION app_fields_text(p_fields jsonb) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
    SELECT coalesce(string_agg(value #>> '{}', ' ' ORDER BY key), '')
    FROM jsonb_each(coalesce(p_fields, '{}'::jsonb))
    WHERE jsonb_typeof(value) = 'string'
$$;
-- +goose StatementEnd

-- Everything but the title, as words: what a search for words finds in long text.
-- +goose StatementBegin
CREATE FUNCTION app_record_words(p_description text, p_fields jsonb) RETURNS tsvector
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    -- Schema-qualified: Postgres 17 builds and maintains indexes with a search path of pg_catalog only.
    -- The SET clause keeps Postgres from inlining it: it inlines a SQL function in an index expression while planning
    -- any query on the table, as whoever runs it, and the query role (00017_query) may not use the public schema, so
    -- every query over tasks failed with "permission denied for schema public". TestEveryQueryViewCanBeRead finds any
    -- other function like it.
    SET search_path = pg_catalog
    AS $$ SELECT to_tsvector('english'::regconfig, coalesce(p_description, '') || ' ' || public.app_fields_text(p_fields)) $$;
-- +goose StatementEnd

-- Words in descriptions and fields; parts of field values ("4412" in SN-4412), typos included.
CREATE INDEX tasks_text_words_idx ON tasks USING gin (app_record_words(description, fields));
CREATE INDEX tasks_fields_trgm_idx ON tasks USING gin (app_fields_text(fields) gin_trgm_ops);

-- +goose Down
DROP INDEX tasks_fields_trgm_idx;
DROP INDEX tasks_text_words_idx;
DROP FUNCTION app_record_words(text, jsonb);
DROP FUNCTION app_fields_text(jsonb);
DROP INDEX tasks_title_words_idx;
DROP FUNCTION app_any_word_query(text);
DROP FUNCTION app_match_score(text, text);
DROP INDEX cis_name_trgm_idx;
DROP INDEX kb_articles_title_trgm_idx;
DROP INDEX tasks_title_trgm_idx;
