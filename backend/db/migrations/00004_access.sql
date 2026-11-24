-- +goose Up
-- Who may do what (docs/apps-platform.md §11). RLS and Go both ask for a named permission (app_can, p.Can), never a
-- role name (TestNothingChecksRolesInSQL). A role is a set of permissions: a person holds their base role (users.role)
-- plus roles granted to them or to a group they are in; an app's principal holds what its manifest declares. Record
-- permissions are per type: records:<verb>:<type>, or * for every type.

CREATE TABLE roles (
    name        text PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9_-]{1,62}$'),
    label       text NOT NULL CHECK (length(btrim(label)) BETWEEN 1 AND 100),
    description text NOT NULL DEFAULT '',
    permissions text[] NOT NULL DEFAULT '{}',
    -- The app that defines it (roles/*.yaml); NULL for the built-in ones. Removing the app removes its roles, and
    -- with them every grant of them.
    app_name    text REFERENCES apps (name) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Extra roles on top of someone's base role, granted to a person or to a group (everyone in it). The base roles are
-- not grantable: a person has exactly one, set on the user.
CREATE TABLE role_grants (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    role       text NOT NULL REFERENCES roles (name) ON DELETE CASCADE
               CHECK (role NOT IN ('admin', 'agent', 'end_user')),
    user_id    uuid REFERENCES users (id) ON DELETE CASCADE,
    group_id   uuid REFERENCES groups (id) ON DELETE CASCADE,
    granted_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((user_id IS NULL) <> (group_id IS NULL))
);
CREATE UNIQUE INDEX role_grants_user_key ON role_grants (role, user_id) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX role_grants_group_key ON role_grants (role, group_id) WHERE group_id IS NOT NULL;
CREATE INDEX role_grants_group_idx ON role_grants (group_id) WHERE group_id IS NOT NULL;

-- Roles are the platform's and the apps' (written by install, as the owner); grants are what admins change.
-- Everyone signed in may read both, like the directory: the UI shows who has which role.
ALTER TABLE roles ENABLE ROW LEVEL SECURITY;
CREATE POLICY roles_select ON roles FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
ALTER TABLE role_grants ENABLE ROW LEVEL SECURITY;
CREATE POLICY role_grants_select ON role_grants FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
CREATE POLICY role_grants_insert ON role_grants FOR INSERT TO servicelater_app WITH CHECK (app_can('roles:manage'));
CREATE POLICY role_grants_delete ON role_grants FOR DELETE TO servicelater_app USING (app_can('roles:manage'));

-- Joining or leaving a group that holds a role, or a restricted group, needs roles:manage as well as groups:manage:
-- otherwise managing groups would be a way to hand out roles, or a view of restricted records.
CREATE POLICY group_members_insert ON group_members FOR INSERT TO servicelater_app
    WITH CHECK (app_can('groups:manage')
                AND (app_can('roles:manage')
                     OR (NOT EXISTS (SELECT 1 FROM role_grants g WHERE g.group_id = group_members.group_id)
                         AND NOT EXISTS (SELECT 1 FROM groups r WHERE r.id = group_members.group_id AND r.restricted))));
CREATE POLICY group_members_delete ON group_members FOR DELETE TO servicelater_app
    USING (app_can('groups:manage')
           AND (app_can('roles:manage')
                OR (NOT EXISTS (SELECT 1 FROM role_grants g WHERE g.group_id = group_members.group_id)
                    AND NOT EXISTS (SELECT 1 FROM groups r WHERE r.id = group_members.group_id AND r.restricted))));

-- The built-in roles, as one function so the migration and the test reset (dbtest truncates apps, which cascades
-- here) write the same rows. A later migration that changes a built-in role replaces this function and calls it.
-- Admins run SQL queries (query:run); others get it through a role (core-itsm's analyst).
-- +goose StatementBegin
CREATE FUNCTION app_seed_builtin_roles() RETURNS void
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

-- A person's permissions: their base role, roles granted to them, and roles granted to their groups; an app
-- principal's come from its manifest (catalog names; the notify: and http: capabilities are declared there too, and
-- checked by the steps that use them). None when deactivated. SECURITY DEFINER because it is asked about other people
-- too (who may be assigned a ticket).
-- +goose StatementBegin
CREATE FUNCTION app_permissions_of(p_user uuid) RETURNS text[]
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$
    SELECT coalesce(array_agg(DISTINCT perm ORDER BY perm), '{}')
    FROM users u
    JOIN LATERAL (
        SELECT unnest(r.permissions) AS perm FROM roles r WHERE r.name = u.role AND u.app_name IS NULL
        UNION
        SELECT unnest(r.permissions) FROM role_grants g JOIN roles r ON r.name = g.role
        WHERE u.app_name IS NULL
          AND (g.user_id = u.id OR g.group_id IN (SELECT m.group_id FROM group_members m WHERE m.user_id = u.id))
        UNION
        SELECT jsonb_array_elements_text(a.manifest -> 'permissions') FROM apps a
        WHERE a.name = u.app_name AND jsonb_typeof(a.manifest -> 'permissions') = 'array'
    ) p ON true
    WHERE u.id = p_user AND u.deactivated_at IS NULL;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_user_can(p_user uuid, p_permission text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$ SELECT p_permission = ANY (app_permissions_of(p_user)) $$;
-- +goose StatementEnd

-- The check every record policy uses: the exact permission, or the same verb for every type.
-- +goose StatementBegin
CREATE FUNCTION app_can_record(p_verb text, p_type text) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT app_can('records:' || p_verb || ':' || p_type) OR app_can('records:' || p_verb || ':*') $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_user_can_record(p_user uuid, p_verb text, p_type text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$
    SELECT perms && ARRAY['records:' || p_verb || ':' || p_type, 'records:' || p_verb || ':*']
    FROM (SELECT app_permissions_of(p_user) AS perms) x
$$;
-- +goose StatementEnd

-- Whether someone has a verb on any type at all (who is offered as an assignee when no type is given).
-- +goose StatementBegin
CREATE FUNCTION app_user_can_record_any(p_user uuid, p_verb text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$ SELECT EXISTS (SELECT 1 FROM unnest(app_permissions_of(p_user)) p WHERE p LIKE 'records:' || p_verb || ':%') $$;
-- +goose StatementEnd

-- The restricted groups a person cannot see into: those they are not in, unless they hold restricted:read. Worked out
-- once per transaction by app_begin, like the permissions, so the read policy reads a setting rather than joining groups.
-- +goose StatementBegin
CREATE FUNCTION app_hidden_groups_of(p_user uuid) RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$
    SELECT CASE WHEN app_user_can(p_user, 'restricted:read') THEN ','
                ELSE ',' || coalesce(string_agg(g.id::text, ','), '') || ',' END
    FROM groups g
    WHERE g.restricted
      AND NOT EXISTS (SELECT 1 FROM group_members m WHERE m.group_id = g.id AND m.user_id = p_user);
$$;
-- +goose StatementEnd

-- How db.WithUser starts a transaction for a person: who they are, their permissions and the restricted groups hidden
-- from them, loaded once into transaction-local settings so every policy check after is a string search, not a join.
-- It also clears any on_behalf_of left by a switch back from a principal, and what the transaction is saving.
-- +goose StatementBegin
CREATE FUNCTION app_begin(p_user uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM set_config('app.current_user_id', p_user::text, true);
    PERFORM set_config('app.permissions', ',' || array_to_string(app_permissions_of(p_user), ',') || ',', true);
    PERFORM set_config('app.on_behalf_of', '', true);
    PERFORM set_config('app.hidden_groups', app_hidden_groups_of(p_user), true);
    PERFORM set_config('app.saving', ',', true);
END
$$;
-- +goose StatementEnd

-- How an app's principal starts one (db.WithPrincipal): as itself, with its permissions, on behalf of someone. A
-- principal is not held back by restricted groups: it acts by record type, as its manifest allows, and its SLA alerts
-- and catalog workflows must reach the records routed to HR.
-- +goose StatementBegin
CREATE FUNCTION app_begin_as(p_principal uuid, p_on_behalf_of uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM users WHERE id = p_principal AND app_name IS NOT NULL) THEN
        RAISE EXCEPTION 'only an app principal acts on behalf of someone' USING ERRCODE = '42501';
    END IF;
    PERFORM app_begin(p_principal);
    PERFORM set_config('app.on_behalf_of', coalesce(p_on_behalf_of::text, ''), true);
    PERFORM set_config('app.hidden_groups', ',', true);
END
$$;
-- +goose StatementEnd

-- users_update lets anyone update their own row (their theme, their email preference). Their role, deactivation and
-- manager are not theirs to change: only users:manage may. Making someone an admin, or taking it away, needs
-- roles:manage too, so a people manager cannot make themselves an admin.
-- +goose StatementBegin
CREATE FUNCTION users_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF current_user = 'servicelater_app' THEN
        IF NOT app_can('users:manage')
           AND (NEW.role IS DISTINCT FROM OLD.role OR NEW.deactivated_at IS DISTINCT FROM OLD.deactivated_at) THEN
            RAISE EXCEPTION 'only an admin may change a role or deactivate someone' USING ERRCODE = '42501';
        END IF;
        IF NOT app_can('users:manage') AND NEW.manager_id IS DISTINCT FROM OLD.manager_id THEN
            RAISE EXCEPTION 'only those who manage people may set someone''s manager' USING ERRCODE = '42501';
        END IF;
        IF NEW.role IS DISTINCT FROM OLD.role AND 'admin' IN (NEW.role, OLD.role) AND NOT app_can('roles:manage') THEN
            RAISE EXCEPTION 'making someone an admin, or taking it away, needs roles:manage' USING ERRCODE = '42501';
        END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER users_guard BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION users_guard();

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

-- Restricting a group, or lifting it, changes who sees its records: it needs roles:manage.
-- +goose StatementBegin
CREATE FUNCTION groups_guard_restricted() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF current_user = 'servicelater_app' AND NEW.restricted IS DISTINCT FROM
       (CASE WHEN TG_OP = 'INSERT' THEN false ELSE OLD.restricted END) AND NOT app_can('roles:manage') THEN
        RAISE EXCEPTION 'restricting a group, or lifting it, needs roles:manage' USING ERRCODE = '42501';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER groups_guard_restricted BEFORE INSERT OR UPDATE ON groups
    FOR EACH ROW EXECUTE FUNCTION groups_guard_restricted();

-- +goose Down
DROP TRIGGER groups_guard_restricted ON groups;
DROP FUNCTION groups_guard_restricted();
DROP TRIGGER users_guard_insert ON users;
DROP FUNCTION users_guard_insert();
DROP TRIGGER users_guard ON users;
DROP FUNCTION users_guard();
DROP FUNCTION app_begin_as(uuid, uuid);
DROP FUNCTION app_begin(uuid);
DROP FUNCTION app_hidden_groups_of(uuid);
DROP FUNCTION app_user_can_record_any(uuid, text);
DROP FUNCTION app_user_can_record(uuid, text, text);
DROP FUNCTION app_can_record(text, text);
DROP FUNCTION app_user_can(uuid, text);
DROP FUNCTION app_permissions_of(uuid);
DROP FUNCTION app_seed_builtin_roles();
DROP POLICY group_members_delete ON group_members;
DROP POLICY group_members_insert ON group_members;
DROP TABLE role_grants;
DROP TABLE roles;
