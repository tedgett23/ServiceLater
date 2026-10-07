-- +goose Up

-- Custom roles make a few permissions worth more than they look, so they need guards against escalation:
--
-- A role granted to a group goes to everyone in it. Whoever can change that group's members could give themselves
-- the role, so changing the members of a group that holds any role needs roles:manage as well as groups:manage.
ALTER POLICY group_members_insert ON group_members
    WITH CHECK (app_can('groups:manage')
                AND (app_can('roles:manage') OR NOT EXISTS (SELECT 1 FROM role_grants g WHERE g.group_id = group_members.group_id)));
ALTER POLICY group_members_delete ON group_members
    USING (app_can('groups:manage')
           AND (app_can('roles:manage') OR NOT EXISTS (SELECT 1 FROM role_grants g WHERE g.group_id = group_members.group_id)));

-- users:manage can set a base role, and admin is a base role that has every permission. Making someone an admin, or
-- taking it away, needs roles:manage too, so a people manager cannot make themselves an admin.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION users_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF current_user = 'servicelater_app' THEN
        IF NOT app_can('users:manage')
           AND (NEW.role IS DISTINCT FROM OLD.role OR NEW.deactivated_at IS DISTINCT FROM OLD.deactivated_at) THEN
            RAISE EXCEPTION 'only an admin may change a role or deactivate someone' USING ERRCODE = '42501';
        END IF;
        IF NEW.role IS DISTINCT FROM OLD.role AND 'admin' IN (NEW.role, OLD.role) AND NOT app_can('roles:manage') THEN
            RAISE EXCEPTION 'making someone an admin, or taking it away, needs roles:manage' USING ERRCODE = '42501';
        END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd

-- The same for a new person created as an admin.
-- +goose StatementBegin
CREATE FUNCTION users_guard_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF current_user = 'servicelater_app' AND NEW.role = 'admin' AND NOT app_can('roles:manage') THEN
        RAISE EXCEPTION 'making someone an admin needs roles:manage' USING ERRCODE = '42501';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER users_guard_insert BEFORE INSERT ON users FOR EACH ROW EXECUTE FUNCTION users_guard_insert();

-- +goose Down
DROP TRIGGER users_guard_insert ON users;
DROP FUNCTION users_guard_insert();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION users_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF current_user = 'servicelater_app' AND NOT app_can('users:manage')
       AND (NEW.role IS DISTINCT FROM OLD.role OR NEW.deactivated_at IS DISTINCT FROM OLD.deactivated_at) THEN
        RAISE EXCEPTION 'only an admin may change a role or deactivate someone' USING ERRCODE = '42501';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
ALTER POLICY group_members_delete ON group_members USING (app_can('groups:manage'));
ALTER POLICY group_members_insert ON group_members WITH CHECK (app_can('groups:manage'));
