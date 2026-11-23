-- +goose Up
-- SQL queries people write themselves (internal/query, docs/query.md). They run on a separate login role that may
-- read only the views in the `query` schema. Those views are not trusting: each filters its rows by an identity it
-- checks itself (app_query_identity), signed by the server. The session settings that RLS trusts (app.permissions and
-- the rest) mean nothing to them. Even if a forbidden function got past the server's check of the SQL (set_config,
-- say), the person would still see only what they may see.

-- hmac() for checking the signature. Ships with Postgres and is trusted, so the database's owner may create it.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- The login role queries run as. Roles belong to the whole cluster, not one database, so each database has its own,
-- named after it. The server sets the role's password from the key below, so every api server agrees on it without
-- any configuration.
-- +goose StatementBegin
CREATE FUNCTION app_query_role() RETURNS text
    LANGUAGE sql STABLE
    AS $$ SELECT left(current_database(), 57) || '_query' $$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE r text := app_query_role();
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = r) THEN
        EXECUTE format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT CONNECTION LIMIT 20', r);
    END IF;
    -- Belt and braces under the server's own per-query settings.
    EXECUTE format('ALTER ROLE %I IN DATABASE %I SET default_transaction_read_only = on', r, current_database());
    EXECUTE format('ALTER ROLE %I IN DATABASE %I SET statement_timeout = %L', r, current_database(), '60s');
    EXECUTE format('ALTER ROLE %I IN DATABASE %I SET idle_in_transaction_session_timeout = %L', r, current_database(), '60s');
    EXECUTE format('ALTER ROLE %I IN DATABASE %I SET search_path = query', r, current_database());
END
$$;
-- +goose StatementEnd

-- Everyone may use the public schema by default. Only the owner and the app role should be able to: the query role
-- must not reach the tables' functions (app_permissions_of tells anyone's permissions, for one) or anything else in
-- it. The query views still call into public; they run as their owner.
REVOKE USAGE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO servicelater_app;

CREATE SCHEMA query;
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA query TO %I', app_query_role());
    -- Every view made in it later is readable by the role, and nothing else is.
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA query GRANT SELECT ON TABLES TO %I', current_user, app_query_role());
END
$$;
-- +goose StatementEnd

-- The key the server signs each query's identity with. Only the owner reads it (the server, as the system).
CREATE TABLE query_keys (
    id         int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    key        bytea NOT NULL CHECK (length(key) = 32),
    created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE query_keys ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON query_keys FROM servicelater_app;
INSERT INTO query_keys (key) VALUES (gen_random_bytes(32));

-- Who a query runs for: the signed setting query.token is "<user id>|<expires, unix seconds>|<scopes>|<hex HMAC-SHA256
-- of what comes before it>". Scopes are an API key's, comma-separated, or * for none. A token that is missing,
-- forged, edited or expired is refused, as is anyone who does not hold query:run. The person's permissions, and the
-- restricted groups hidden from them, are worked out here from the database, never taken from the session. Every
-- query view joins this once.
-- +goose StatementBegin
CREATE FUNCTION app_query_identity(OUT user_id uuid, OUT perms text, OUT hidden text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    parts text[] := string_to_array(current_setting('query.token', true), '|');
    p text[];
BEGIN
    IF coalesce(array_length(parts, 1), 0) <> 4
       OR encode(hmac(convert_to(parts[1] || '|' || parts[2] || '|' || parts[3], 'UTF8'), (SELECT key FROM query_keys), 'sha256'), 'hex') <> parts[4] THEN
        RAISE EXCEPTION 'this query carries no valid identity' USING ERRCODE = '42501';
    END IF;
    IF to_timestamp(parts[2]::bigint) < clock_timestamp() THEN
        RAISE EXCEPTION 'this query''s identity has expired' USING ERRCODE = '42501';
    END IF;
    user_id := parts[1]::uuid;
    p := app_permissions_of(user_id);
    IF parts[3] <> '*' THEN
        p := app_narrow_permissions(p, string_to_array(parts[3], ','));
    END IF;
    IF NOT 'query:run' = ANY (p) THEN
        RAISE EXCEPTION 'you may not run queries' USING ERRCODE = '42501';
    END IF;
    perms := ',' || array_to_string(p, ',') || ',';
    hidden := CASE WHEN 'restricted:read' = ANY (p) THEN ','
                   ELSE ',' || coalesce((SELECT string_agg(g.id::text, ',') FROM groups g
                                          WHERE g.restricted
                                            AND NOT EXISTS (SELECT 1 FROM group_members m
                                                             WHERE m.group_id = g.id AND m.user_id = parts[1]::uuid)), '') || ','
              END;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_query_identity() FROM PUBLIC;
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('GRANT EXECUTE ON FUNCTION app_query_identity() TO %I', app_query_role());
END
$$;
-- +goose StatementEnd

-- Every query someone ran or tried to, and how it went. Written by the server as the system, so that a refused or
-- failed query is still recorded. People read their own; those who manage settings read everyone's.
CREATE TABLE query_runs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    api_key_id  uuid REFERENCES api_keys (id) ON DELETE SET NULL,
    sql         text NOT NULL,
    params      jsonb NOT NULL DEFAULT '[]',
    outcome     text NOT NULL CHECK (outcome IN ('ok', 'refused', 'failed', 'timeout')),
    error       text NOT NULL DEFAULT '',
    row_count   int NOT NULL DEFAULT 0,
    truncated   boolean NOT NULL DEFAULT false,
    duration_ms int NOT NULL DEFAULT 0,
    started_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX query_runs_user_idx ON query_runs (user_id, started_at DESC);
CREATE INDEX query_runs_started_idx ON query_runs (started_at DESC);
ALTER TABLE query_runs ENABLE ROW LEVEL SECURITY;
REVOKE INSERT, UPDATE, DELETE ON query_runs FROM servicelater_app;
CREATE POLICY query_runs_select ON query_runs FOR SELECT TO servicelater_app
    USING (user_id = app_current_user_id() OR app_can('settings:manage'));

-- Admins run queries. Others get query:run through a role (core-itsm's analyst).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,catalog:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,forms:manage,freezes:manage,groups:manage,knowledge:publish,knowledge:read_internal,knowledge:write,query:run,records:create:*,records:delete:*,records:read:*,records:update:*,reports:read,restricted:read,roles:manage,scripts:edit,slas:manage,settings:manage,users:manage,users:read,workflows:edit,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,reports:read,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,catalog:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,forms:manage,freezes:manage,groups:manage,knowledge:publish,knowledge:read_internal,knowledge:write,records:create:*,records:delete:*,records:read:*,records:update:*,reports:read,restricted:read,roles:manage,scripts:edit,slas:manage,settings:manage,users:manage,users:read,workflows:edit,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,reports:read,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();
DROP TABLE query_runs;
DROP FUNCTION app_query_identity();
DROP TABLE query_keys;
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA query REVOKE SELECT ON TABLES FROM %I', current_user, app_query_role());
END
$$;
-- +goose StatementEnd
DROP SCHEMA query CASCADE;
GRANT USAGE ON SCHEMA public TO PUBLIC;
REVOKE USAGE ON SCHEMA public FROM servicelater_app;
-- +goose StatementBegin
DO $$
DECLARE r text := app_query_role();
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = r) THEN
        EXECUTE format('ALTER ROLE %I IN DATABASE %I RESET ALL', r, current_database());
        EXECUTE format('DROP ROLE %I', r);
    END IF;
END
$$;
-- +goose StatementEnd
DROP FUNCTION app_query_role();
DROP EXTENSION pgcrypto;
