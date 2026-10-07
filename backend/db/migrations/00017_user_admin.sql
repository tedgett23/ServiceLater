-- +goose Up

-- Admins manage people from the web: create, edit, deactivate, reset a password, delete.
-- Deleting is only possible for someone with no history (tickets, comments, approvals keep their FK to users);
-- everyone else is deactivated instead: they cannot sign in, lose any staff or admin power at once, and drop out of
-- pickers and group() approvers, while their name stays on what they did.
ALTER TABLE users ADD COLUMN deactivated_at timestamptz;

-- A deactivated admin or agent stops being one under RLS immediately, not when their access token runs out.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_is_admin() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT EXISTS (SELECT 1 FROM users WHERE id = app_current_user_id() AND role = 'admin' AND deactivated_at IS NULL) $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_is_staff() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT EXISTS (SELECT 1 FROM users WHERE id = app_current_user_id() AND role IN ('admin', 'agent') AND deactivated_at IS NULL) $$;
-- +goose StatementEnd

-- users_update lets anyone update their own row (for a future profile page). Role and deactivation are an admin's
-- to change, never one's own: without this an end user could make themselves an admin through any such endpoint.
-- +goose StatementBegin
CREATE FUNCTION users_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF current_user = 'servicelater_app' AND NOT app_is_admin()
       AND (NEW.role IS DISTINCT FROM OLD.role OR NEW.deactivated_at IS DISTINCT FROM OLD.deactivated_at) THEN
        RAISE EXCEPTION 'only an admin may change a role or deactivate someone' USING ERRCODE = '42501';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER users_guard BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION users_guard();

-- There is always at least one active admin, or nobody could manage anyone again. The lock serialises admin changes
-- so two admins demoting each other at once cannot both succeed; each statement in plpgsql sees what committed
-- before it took the lock.
-- +goose StatementBegin
CREATE FUNCTION users_keep_an_admin() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.role <> 'admin' OR OLD.deactivated_at IS NOT NULL THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.role = 'admin' AND NEW.deactivated_at IS NULL THEN
        RETURN NULL;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('servicelater.admins'));
    IF NOT EXISTS (SELECT 1 FROM users WHERE role = 'admin' AND deactivated_at IS NULL AND id <> OLD.id) THEN
        RAISE EXCEPTION 'there must be at least one active admin'
            USING ERRCODE = '23514', CONSTRAINT = 'users_last_admin';
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER users_keep_an_admin AFTER UPDATE OR DELETE ON users FOR EACH ROW EXECUTE FUNCTION users_keep_an_admin();

-- Credentials and sessions are owner-only tables (00004). An admin setting someone's password, or cutting off a
-- deactivated person's sessions, goes through these instead, in the same transaction as the rest of the change.
-- +goose StatementBegin
CREATE FUNCTION app_set_password(p_user_id uuid, p_hash text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT app_is_admin() THEN
        RAISE EXCEPTION 'only an admin may set a password' USING ERRCODE = '42501';
    END IF;
    INSERT INTO user_credentials (user_id, password_hash) VALUES (p_user_id, p_hash)
    ON CONFLICT (user_id) DO UPDATE SET password_hash = EXCLUDED.password_hash, updated_at = now();
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_revoke_sessions(p_user_id uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT app_is_admin() THEN
        RAISE EXCEPTION 'only an admin may sign someone out' USING ERRCODE = '42501';
    END IF;
    DELETE FROM sessions WHERE user_id = p_user_id;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION app_revoke_sessions(uuid);
DROP FUNCTION app_set_password(uuid, text);
DROP TRIGGER users_keep_an_admin ON users;
DROP FUNCTION users_keep_an_admin();
DROP TRIGGER users_guard ON users;
DROP FUNCTION users_guard();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_is_admin() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT EXISTS (SELECT 1 FROM users WHERE id = app_current_user_id() AND role = 'admin') $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_is_staff() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT EXISTS (SELECT 1 FROM users WHERE id = app_current_user_id() AND role IN ('admin', 'agent')) $$;
-- +goose StatementEnd
ALTER TABLE users DROP COLUMN deactivated_at;
