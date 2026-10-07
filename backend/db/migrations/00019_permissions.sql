-- +goose Up

-- Permissions under roles (docs/apps-platform.md §11, open question 3). Until now RLS asked "is this person staff"
-- (app_is_staff) or "an admin" (app_is_admin). It now asks for a named permission (app_can), and a role is a set of
-- them, so custom roles and app service principals use the same model. The three built-in roles keep exactly what
-- they could do before: this migration changes no one's access.

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

-- The built-in roles, as one function so the migration and the test reset (dbtest truncates apps, which cascades
-- here) write the same rows. A later migration that changes a built-in role replaces this function and calls it.
-- +goose StatementBegin
CREATE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{tickets:read_all,tickets:update,tickets:delete,comments:internal,comments:delete,workflows:read_all,workflows:run,users:read,users:manage,groups:manage,roles:manage}'),
        ('agent', 'Agent', 'Works tickets: sees and edits all of them, writes internal notes, runs workflows.',
         '{tickets:read_all,tickets:update,comments:internal,workflows:read_all,workflows:run,users:read}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();

-- Extra roles on top of someone's base role (users.role), granted to a person or to a group (everyone in it). The
-- base roles are not grantable: a person has exactly one, set on the user.
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

-- A person's permissions: their base role, roles granted to them, and roles granted to their groups. None when
-- deactivated. SECURITY DEFINER because it is asked about other people too (who may be assigned a ticket).
-- +goose StatementBegin
CREATE FUNCTION app_permissions_of(p_user uuid) RETURNS text[]
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$
    SELECT coalesce(array_agg(DISTINCT perm ORDER BY perm), '{}')
    FROM users u
    JOIN LATERAL (
        SELECT unnest(r.permissions) AS perm FROM roles r WHERE r.name = u.role
        UNION
        SELECT unnest(r.permissions) FROM role_grants g JOIN roles r ON r.name = g.role
        WHERE g.user_id = u.id
           OR g.group_id IN (SELECT m.group_id FROM group_members m WHERE m.user_id = u.id)
    ) p ON true
    WHERE u.id = p_user AND u.deactivated_at IS NULL;
$$;
-- +goose StatementEnd

-- app_begin is how db.WithUser starts a transaction for someone: who they are, and their permissions, loaded once
-- into transaction-local settings so every policy check after is a string search, not a join.
-- +goose StatementBegin
CREATE FUNCTION app_begin(p_user uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM set_config('app.current_user_id', p_user::text, true);
    PERFORM set_config('app.permissions', ',' || array_to_string(app_permissions_of(p_user), ',') || ',', true);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_can(p_permission text) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT position(',' || p_permission || ',' IN coalesce(current_setting('app.permissions', true), '')) > 0 $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_user_can(p_user uuid, p_permission text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$ SELECT p_permission = ANY (app_permissions_of(p_user)) $$;
-- +goose StatementEnd

-- Roles are the platform's and the apps' (written by install, as the owner); grants are what admins change.
-- Everyone signed in may read both, like the user directory: the UI shows who has which role.
ALTER TABLE roles ENABLE ROW LEVEL SECURITY;
CREATE POLICY roles_select ON roles FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
ALTER TABLE role_grants ENABLE ROW LEVEL SECURITY;
CREATE POLICY role_grants_select ON role_grants FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
CREATE POLICY role_grants_insert ON role_grants FOR INSERT TO servicelater_app WITH CHECK (app_can('roles:manage'));
CREATE POLICY role_grants_delete ON role_grants FOR DELETE TO servicelater_app USING (app_can('roles:manage'));

-- Every policy that asked for staff or admin now asks for the permission it means.
ALTER POLICY tasks_select ON tasks USING (app_can('tickets:read_all') OR requester_id = app_current_user_id());
ALTER POLICY tasks_insert ON tasks WITH CHECK (app_can('tickets:update') OR requester_id = app_current_user_id());
ALTER POLICY tasks_update ON tasks USING (app_can('tickets:update')) WITH CHECK (app_can('tickets:update'));
ALTER POLICY tasks_delete ON tasks USING (app_can('tickets:delete'));
ALTER POLICY incidents_update ON incidents
    USING (app_can('tickets:update') AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id))
    WITH CHECK (app_can('tickets:update'));
ALTER POLICY comments_select ON task_comments
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id) AND (NOT internal OR app_can('comments:internal')));
ALTER POLICY comments_insert ON task_comments
    WITH CHECK (author_id = app_current_user_id()
                AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id)
                AND (NOT internal OR app_can('comments:internal')));
ALTER POLICY comments_delete ON task_comments USING (app_can('comments:delete'));
ALTER POLICY users_insert ON users WITH CHECK (app_can('users:manage'));
ALTER POLICY users_update ON users
    USING (id = app_current_user_id() OR app_can('users:manage'))
    WITH CHECK (id = app_current_user_id() OR app_can('users:manage'));
ALTER POLICY users_delete ON users USING (app_can('users:manage'));
ALTER POLICY groups_insert ON groups WITH CHECK (app_can('groups:manage'));
ALTER POLICY groups_update ON groups USING (app_can('groups:manage')) WITH CHECK (app_can('groups:manage'));
ALTER POLICY groups_delete ON groups USING (app_can('groups:manage'));
ALTER POLICY group_members_insert ON group_members WITH CHECK (app_can('groups:manage'));
ALTER POLICY group_members_delete ON group_members USING (app_can('groups:manage'));
ALTER POLICY workflow_executions_select ON workflow_executions
    USING (app_can('workflows:read_all') OR started_by = app_current_user_id() OR app_has_approval_on(id));
ALTER POLICY workflow_executions_update ON workflow_executions
    USING (started_by = app_current_user_id() OR app_can('workflows:run'))
    WITH CHECK (started_by = app_current_user_id() OR app_can('workflows:run'));
ALTER POLICY workflow_approvals_select ON workflow_approvals
    USING (approver_id = app_current_user_id() OR app_can('workflows:read_all')
        OR EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id AND e.started_by = app_current_user_id()));

-- The functions that checked a role.
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_set_password(p_user_id uuid, p_hash text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT app_can('users:manage') THEN
        RAISE EXCEPTION 'only an admin may set a password' USING ERRCODE = '42501';
    END IF;
    INSERT INTO user_credentials (user_id, password_hash) VALUES (p_user_id, p_hash)
    ON CONFLICT (user_id) DO UPDATE SET password_hash = EXCLUDED.password_hash, updated_at = now();
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_revoke_sessions(p_user_id uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT app_can('users:manage') THEN
        RAISE EXCEPTION 'only an admin may sign someone out' USING ERRCODE = '42501';
    END IF;
    DELETE FROM sessions WHERE user_id = p_user_id;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_notify(p_users uuid[], p_task uuid, p_app text, p_template text) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    actor uuid := app_current_user_id();
    recipient uuid;
    sent integer := 0;
BEGIN
    IF actor IS NULL OR NOT EXISTS (
        SELECT 1 FROM tasks t WHERE t.id = p_task AND (app_can('tickets:read_all') OR t.requester_id = actor)) THEN
        RAISE EXCEPTION 'not allowed to notify about this record' USING ERRCODE = '42501';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM app_definitions
                   WHERE app_name = p_app AND kind = 'notification' AND name = p_template AND current) THEN
        RAISE EXCEPTION 'no notification template %/%', p_app, p_template USING ERRCODE = '22023';
    END IF;
    IF coalesce(array_length(p_users, 1), 0) > 1000 THEN
        RAISE EXCEPTION 'a notification may go to at most 1000 people' USING ERRCODE = '22023';
    END IF;
    FOR recipient IN
        SELECT DISTINCT u.id
          FROM unnest(p_users) AS r(id)
          JOIN users u ON u.id = r.id
          JOIN tasks t ON t.id = p_task
         WHERE u.id <> actor
           AND u.deactivated_at IS NULL
           AND (u.id = t.requester_id OR app_user_can(u.id, 'tickets:read_all'))
    LOOP
        INSERT INTO notifications (user_id, kind, task_id, app, template, actor_id)
        VALUES (recipient, 'template', p_task, p_app, p_template, actor);
        PERFORM pg_notify('notification_changes', json_build_object('user_id', recipient)::text);
        sent := sent + 1;
    END LOOP;
    RETURN sent;
END
$$;
-- +goose StatementEnd

-- app_is_staff and app_is_admin stay defined (additive), now meaning what they meant in permissions, for anything
-- outside this repository that still calls them. Nothing here does any more (TestNothingChecksRolesInSQL).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_is_staff() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT app_can('tickets:read_all') $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_is_admin() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT app_can('users:manage') $$;
-- +goose StatementEnd

-- +goose Down
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_notify(p_users uuid[], p_task uuid, p_app text, p_template text) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    actor uuid := app_current_user_id();
    recipient uuid;
    sent integer := 0;
BEGIN
    IF actor IS NULL OR NOT EXISTS (
        SELECT 1 FROM tasks t WHERE t.id = p_task AND (app_is_staff() OR t.requester_id = actor)) THEN
        RAISE EXCEPTION 'not allowed to notify about this record' USING ERRCODE = '42501';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM app_definitions
                   WHERE app_name = p_app AND kind = 'notification' AND name = p_template AND current) THEN
        RAISE EXCEPTION 'no notification template %/%', p_app, p_template USING ERRCODE = '22023';
    END IF;
    IF coalesce(array_length(p_users, 1), 0) > 1000 THEN
        RAISE EXCEPTION 'a notification may go to at most 1000 people' USING ERRCODE = '22023';
    END IF;
    FOR recipient IN
        SELECT DISTINCT u.id
          FROM unnest(p_users) AS r(id)
          JOIN users u ON u.id = r.id
          JOIN tasks t ON t.id = p_task
         WHERE u.id <> actor
           AND u.deactivated_at IS NULL
           AND (u.role IN ('admin', 'agent') OR u.id = t.requester_id)
    LOOP
        INSERT INTO notifications (user_id, kind, task_id, app, template, actor_id)
        VALUES (recipient, 'template', p_task, p_app, p_template, actor);
        PERFORM pg_notify('notification_changes', json_build_object('user_id', recipient)::text);
        sent := sent + 1;
    END LOOP;
    RETURN sent;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_revoke_sessions(p_user_id uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT app_is_admin() THEN
        RAISE EXCEPTION 'only an admin may sign someone out' USING ERRCODE = '42501';
    END IF;
    DELETE FROM sessions WHERE user_id = p_user_id;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_set_password(p_user_id uuid, p_hash text) RETURNS void
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
CREATE OR REPLACE FUNCTION users_guard() RETURNS trigger
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
ALTER POLICY workflow_approvals_select ON workflow_approvals
    USING (approver_id = app_current_user_id() OR app_is_staff()
        OR EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id AND e.started_by = app_current_user_id()));
ALTER POLICY workflow_executions_update ON workflow_executions
    USING (started_by = app_current_user_id() OR app_is_staff())
    WITH CHECK (started_by = app_current_user_id() OR app_is_staff());
ALTER POLICY workflow_executions_select ON workflow_executions
    USING (app_is_staff() OR started_by = app_current_user_id() OR app_has_approval_on(id));
ALTER POLICY group_members_delete ON group_members USING (app_is_admin());
ALTER POLICY group_members_insert ON group_members WITH CHECK (app_is_admin());
ALTER POLICY groups_delete ON groups USING (app_is_admin());
ALTER POLICY groups_update ON groups USING (app_is_admin()) WITH CHECK (app_is_admin());
ALTER POLICY groups_insert ON groups WITH CHECK (app_is_admin());
ALTER POLICY users_delete ON users USING (app_is_admin());
ALTER POLICY users_update ON users
    USING (id = app_current_user_id() OR app_is_admin())
    WITH CHECK (id = app_current_user_id() OR app_is_admin());
ALTER POLICY users_insert ON users WITH CHECK (app_is_admin());
ALTER POLICY comments_delete ON task_comments USING (app_is_admin());
ALTER POLICY comments_insert ON task_comments
    WITH CHECK (author_id = app_current_user_id()
                AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id)
                AND (NOT internal OR app_is_staff()));
ALTER POLICY comments_select ON task_comments
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id) AND (NOT internal OR app_is_staff()));
ALTER POLICY incidents_update ON incidents
    USING (app_is_staff() AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id))
    WITH CHECK (app_is_staff());
ALTER POLICY tasks_delete ON tasks USING (app_is_admin());
ALTER POLICY tasks_update ON tasks USING (app_is_staff()) WITH CHECK (app_is_staff());
ALTER POLICY tasks_insert ON tasks WITH CHECK (app_is_staff() OR requester_id = app_current_user_id());
ALTER POLICY tasks_select ON tasks USING (app_is_staff() OR requester_id = app_current_user_id());
DROP TABLE role_grants;
DROP TABLE roles;
DROP FUNCTION app_user_can(uuid, text);
DROP FUNCTION app_can(text);
DROP FUNCTION app_begin(uuid);
DROP FUNCTION app_permissions_of(uuid);
DROP FUNCTION app_seed_builtin_roles();
