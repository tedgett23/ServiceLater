-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Runtime role that RLS applies to. The connection role (table owner, used for migrations and explicit
-- system operations) switches to it per transaction with SET LOCAL ROLE; see internal/db/tx.go.
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

-- The authenticated user for this transaction, or NULL when none was set.
-- +goose StatementBegin
CREATE FUNCTION app_current_user_id() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$ SELECT nullif(current_setting('app.current_user_id', true), '')::uuid $$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION app_current_user_id();
