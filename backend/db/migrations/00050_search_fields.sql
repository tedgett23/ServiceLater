-- +goose Up
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

-- Everything but the title, as words: what a search for words finds in long text.
CREATE FUNCTION app_record_words(p_description text, p_fields jsonb) RETURNS tsvector
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    -- Schema-qualified: Postgres 17 builds and maintains indexes with a search path of pg_catalog only.
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
