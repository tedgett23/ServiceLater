-- +goose Up
-- The two functions behind the search indexes on tasks move to a schema of their own, so the word index's function
-- can be inlined again. Postgres inlines a SQL function in an index expression while planning any query on the table,
-- as whoever runs it. The query role (00017_query) may not use the public schema, and app_record_words named
-- public.app_fields_text, so 00015 gave it SET search_path, which stops inlining, but costs a settings save and
-- restore on every call: about twice the work for every save of a ticket and every row of a restore or import.
-- searchfn holds only these pure text functions (they read no table and reveal nothing), so every role may use it,
-- the query role included, and the public schema stays closed to it. Moving a function keeps its id, so the indexes
-- that use them are untouched. TestEveryQueryViewCanBeRead fails if a query view cannot be read again.
CREATE SCHEMA searchfn;
GRANT USAGE ON SCHEMA searchfn TO PUBLIC;
ALTER FUNCTION public.app_fields_text(jsonb) SET SCHEMA searchfn;
ALTER FUNCTION public.app_record_words(text, jsonb) SET SCHEMA searchfn;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION searchfn.app_record_words(p_description text, p_fields jsonb) RETURNS tsvector
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    -- Schema-qualified: Postgres 17 builds and maintains indexes with a search path of pg_catalog only.
    AS $$ SELECT to_tsvector('english'::regconfig, coalesce(p_description, '') || ' ' || searchfn.app_fields_text(p_fields)) $$;
-- +goose StatementEnd
ALTER FUNCTION searchfn.app_record_words(text, jsonb) RESET ALL;

-- +goose Down
ALTER FUNCTION searchfn.app_fields_text(jsonb) SET SCHEMA public;
ALTER FUNCTION searchfn.app_record_words(text, jsonb) SET SCHEMA public;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.app_record_words(p_description text, p_fields jsonb) RETURNS tsvector
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    SET search_path = pg_catalog
    AS $$ SELECT to_tsvector('english'::regconfig, coalesce(p_description, '') || ' ' || public.app_fields_text(p_fields)) $$;
-- +goose StatementEnd
DROP SCHEMA searchfn;
