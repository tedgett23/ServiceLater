-- +goose Up
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text NOT NULL UNIQUE,
    name          text NOT NULL,
    role          text NOT NULL DEFAULT 'end_user' CHECK (role IN ('admin', 'agent', 'end_user')),
    password_hash text,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementBegin
CREATE FUNCTION app_is_admin() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT EXISTS (SELECT 1 FROM users WHERE id = app_current_user_id() AND role = 'admin') $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_is_staff() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT EXISTS (SELECT 1 FROM users WHERE id = app_current_user_id() AND role IN ('admin', 'agent')) $$;
-- +goose StatementEnd

ALTER TABLE users ENABLE ROW LEVEL SECURITY;

-- Directory: any authenticated user can read users (needed for assignee/requester names).
CREATE POLICY users_select ON users FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
CREATE POLICY users_insert ON users FOR INSERT TO servicelater_app WITH CHECK (app_is_admin());
CREATE POLICY users_update ON users FOR UPDATE TO servicelater_app
    USING (id = app_current_user_id() OR app_is_admin())
    WITH CHECK (id = app_current_user_id() OR app_is_admin());
CREATE POLICY users_delete ON users FOR DELETE TO servicelater_app USING (app_is_admin());

-- +goose Down
DROP TABLE users;
DROP FUNCTION app_is_staff();
DROP FUNCTION app_is_admin();
