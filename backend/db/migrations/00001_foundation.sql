-- +goose Up
-- What every other migration builds on: extensions, the role row-level security applies to, and the session settings
-- a transaction carries (who is acting, for whom, and with which permissions; see internal/db/tx.go).

-- Global search's "contains" (ILIKE) and "looks like" (word similarity) indexes. It ships with Postgres and is a
-- trusted extension, so the database's owner may create it.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
-- hmac() for checking the signed identity SQL queries carry (00017_query). Also ships with Postgres and is trusted.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- The runtime role RLS applies to. The connection role (table owner, used for migrations and explicit system
-- operations, db.WithSystem) switches to it per transaction with SET LOCAL ROLE. Every table the owner creates is
-- readable and writable by it unless a migration revokes that (owner-only tables do).
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'servicelater_app') THEN
        CREATE ROLE servicelater_app NOLOGIN;
    END IF;
    EXECUTE format('GRANT servicelater_app TO %I', current_user);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO servicelater_app', current_user);
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO servicelater_app', current_user);
END
$$;
-- +goose StatementEnd

-- Everyone may use the public schema by default. Only the owner and the app role should be able to: the login role
-- SQL queries run as (00017_query) must not reach the tables' functions (app_permissions_of tells anyone's
-- permissions, for one) or anything else in it. The query views still call into public; they run as their owner.
REVOKE USAGE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO servicelater_app;

-- The person acting in this transaction, or NULL when none was set.
-- +goose StatementBegin
CREATE FUNCTION app_current_user_id() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$ SELECT nullif(current_setting('app.current_user_id', true), '')::uuid $$;
-- +goose StatementEnd

-- The person an app's principal is acting for in this transaction, or NULL when someone is acting for themselves.
-- +goose StatementBegin
CREATE FUNCTION app_on_behalf_of() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$ SELECT nullif(current_setting('app.on_behalf_of', true), '')::uuid $$;
-- +goose StatementEnd

-- Whether the person acting holds a permission. app_begin (access) loads their permissions into app.permissions once
-- per transaction as ",perm,perm,"; policies and functions ask here. Code checks permissions, never role names.
-- +goose StatementBegin
CREATE FUNCTION app_can(p_permission text) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT position(',' || p_permission || ',' IN coalesce(current_setting('app.permissions', true), '')) > 0 $$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION app_can(text);
DROP FUNCTION app_on_behalf_of();
DROP FUNCTION app_current_user_id();
GRANT USAGE ON SCHEMA public TO PUBLIC;
REVOKE USAGE ON SCHEMA public FROM servicelater_app;
DROP EXTENSION pgcrypto;
DROP EXTENSION pg_trgm;
