-- +goose Up

-- Restricted groups. A group marked restricted (HR, Security) keeps the records routed to it to its members: other
-- staff who may read the record type no longer see them. The requester always sees their own, the assignee sees
-- what is assigned to them, and anyone holding restricted:read (admins by default, grantable through a role) sees
-- past the restriction. Everything not routed to a restricted group is visible as before.
ALTER TABLE groups ADD COLUMN restricted boolean NOT NULL DEFAULT false;

-- The restricted groups a person cannot see into: those they are not in, unless they hold restricted:read. Worked out
-- once per transaction by app_begin, like the permissions, so the policy reads a setting rather than joining groups.
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_begin(p_user uuid) RETURNS void
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

-- An app's principal is not held back by restricted groups: it acts by record type, as its manifest allows, and its
-- SLA alerts and catalog workflows must reach the records routed to HR.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_begin_as(p_principal uuid, p_on_behalf_of uuid) RETURNS void
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

-- A record the caller is saving stays visible to them until the transaction ends, even when the save routes it to a
-- restricted group they are not in (handing a misfiled ticket to HR): without this, Postgres would refuse the save,
-- because the row it returns would no longer pass the read policy. Only the save path (internal/records) marks a
-- record, after reading it under RLS, or for a record it is about to create.
-- +goose StatementBegin
CREATE FUNCTION app_mark_saving(p_task uuid) RETURNS void
    LANGUAGE sql VOLATILE
    AS $$ SELECT set_config('app.saving', coalesce(current_setting('app.saving', true), ',') || p_task::text || ',', true) $$;
-- +goose StatementEnd

-- Whether the caller sees this record, by the rules above: the read policy, and the same check for SECURITY DEFINER
-- code that must judge a record for the caller. saving says whether a record being saved counts.
-- +goose StatementBegin
CREATE FUNCTION app_sees_task(p_id uuid, p_type text, p_requester uuid, p_assignee uuid, p_group uuid, p_saving boolean)
    RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT p_requester = app_current_user_id()
        OR (p_saving AND position(',' || p_id::text || ',' IN coalesce(current_setting('app.saving', true), ',')) > 0)
        OR (app_can_record('read', p_type)
            AND (p_group IS NULL
                 OR p_assignee = app_current_user_id()
                 OR position(',' || p_group::text || ',' IN coalesce(current_setting('app.hidden_groups', true), ',')) = 0));
$$;
-- +goose StatementEnd

-- The same, for someone other than the caller (who gets a notification, say). Works it out from scratch.
-- +goose StatementBegin
CREATE FUNCTION app_user_sees_task(p_user uuid, p_type text, p_requester uuid, p_assignee uuid, p_group uuid)
    RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$
    SELECT p_user = p_requester
        OR (app_user_can_record(p_user, 'read', p_type)
            AND (p_group IS NULL
                 OR p_assignee = p_user
                 OR NOT EXISTS (SELECT 1 FROM groups g WHERE g.id = p_group AND g.restricted)
                 OR EXISTS (SELECT 1 FROM group_members m WHERE m.group_id = p_group AND m.user_id = p_user)
                 OR app_user_can(p_user, 'restricted:read')));
$$;
-- +goose StatementEnd

ALTER POLICY tasks_select ON tasks
    USING (app_sees_task(id, type, requester_id, assignee_id, assignment_group_id, true));
-- Updating needs the record in view (or being saved); the new row need not be, so routing to HR is allowed.
ALTER POLICY tasks_update ON tasks
    USING (app_can_record('update', type) AND app_sees_task(id, type, requester_id, assignee_id, assignment_group_id, true))
    WITH CHECK (app_can_record('update', type));
ALTER POLICY tasks_delete ON tasks
    USING (app_can_record('delete', type) AND app_sees_task(id, type, requester_id, assignee_id, assignment_group_id, false));

-- Restricting a group, or lifting it, changes who sees its records; so does joining or leaving a restricted group.
-- Both need roles:manage (admins), like the members of a group that holds a role.
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

ALTER POLICY group_members_insert ON group_members
    WITH CHECK (app_can('groups:manage')
                AND (app_can('roles:manage')
                     OR (NOT EXISTS (SELECT 1 FROM role_grants g WHERE g.group_id = group_members.group_id)
                         AND NOT EXISTS (SELECT 1 FROM groups r WHERE r.id = group_members.group_id AND r.restricted))));
ALTER POLICY group_members_delete ON group_members
    USING (app_can('groups:manage')
           AND (app_can('roles:manage')
                OR (NOT EXISTS (SELECT 1 FROM role_grants g WHERE g.group_id = group_members.group_id)
                    AND NOT EXISTS (SELECT 1 FROM groups r WHERE r.id = group_members.group_id AND r.restricted))));

-- Notifications decide their recipients as the records' readers would: nobody hears about a restricted record they
-- cannot see.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_notify(p_users uuid[], p_task uuid, p_app text, p_template text) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    actor uuid := app_current_user_id();
    behalf uuid := app_on_behalf_of();
    recipient uuid;
    sent integer := 0;
BEGIN
    IF actor IS NULL OR NOT EXISTS (
        SELECT 1 FROM tasks t WHERE t.id = p_task
           AND app_sees_task(t.id, t.type, t.requester_id, t.assignee_id, t.assignment_group_id, true)) THEN
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
           AND (u.id IS DISTINCT FROM behalf OR current_setting('app.workflow_step', true) = 'on')
           AND u.app_name IS NULL
           AND u.deactivated_at IS NULL
           AND app_user_sees_task(u.id, t.type, t.requester_id, t.assignee_id, t.assignment_group_id)
    LOOP
        INSERT INTO notifications (user_id, kind, task_id, app, template, actor_id)
        VALUES (recipient, 'template', p_task, p_app, p_template, coalesce(behalf, actor));
        PERFORM pg_notify('notification_changes', json_build_object('user_id', recipient)::text);
        sent := sent + 1;
    END LOOP;
    RETURN sent;
END
$$;
-- +goose StatementEnd

-- Admins see past restricted groups.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,catalog:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,freezes:manage,groups:manage,knowledge:publish,knowledge:read_internal,knowledge:write,records:create:*,records:delete:*,records:read:*,records:update:*,restricted:read,roles:manage,settings:manage,users:manage,users:read,workflows:edit,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
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
         '{calendars:manage,catalog:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,freezes:manage,groups:manage,knowledge:publish,knowledge:read_internal,knowledge:write,records:create:*,records:delete:*,records:read:*,records:update:*,roles:manage,settings:manage,users:manage,users:read,workflows:edit,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_notify(p_users uuid[], p_task uuid, p_app text, p_template text) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    actor uuid := app_current_user_id();
    behalf uuid := app_on_behalf_of();
    recipient uuid;
    sent integer := 0;
BEGIN
    IF actor IS NULL OR NOT EXISTS (
        SELECT 1 FROM tasks t WHERE t.id = p_task AND (app_can_record('read', t.type) OR t.requester_id = actor)) THEN
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
           AND (u.id IS DISTINCT FROM behalf OR current_setting('app.workflow_step', true) = 'on')
           AND u.app_name IS NULL
           AND u.deactivated_at IS NULL
           AND (u.id = t.requester_id OR app_user_can_record(u.id, 'read', t.type))
    LOOP
        INSERT INTO notifications (user_id, kind, task_id, app, template, actor_id)
        VALUES (recipient, 'template', p_task, p_app, p_template, coalesce(behalf, actor));
        PERFORM pg_notify('notification_changes', json_build_object('user_id', recipient)::text);
        sent := sent + 1;
    END LOOP;
    RETURN sent;
END
$$;
-- +goose StatementEnd
ALTER POLICY group_members_delete ON group_members
    USING (app_can('groups:manage')
           AND (app_can('roles:manage') OR NOT EXISTS (SELECT 1 FROM role_grants g WHERE g.group_id = group_members.group_id)));
ALTER POLICY group_members_insert ON group_members
    WITH CHECK (app_can('groups:manage')
                AND (app_can('roles:manage') OR NOT EXISTS (SELECT 1 FROM role_grants g WHERE g.group_id = group_members.group_id)));
DROP TRIGGER groups_guard_restricted ON groups;
DROP FUNCTION groups_guard_restricted();
ALTER POLICY tasks_delete ON tasks USING (app_can_record('delete', type));
ALTER POLICY tasks_update ON tasks USING (app_can_record('update', type)) WITH CHECK (app_can_record('update', type));
ALTER POLICY tasks_select ON tasks USING (app_can_record('read', type) OR requester_id = app_current_user_id());
DROP FUNCTION app_user_sees_task(uuid, text, uuid, uuid, uuid);
DROP FUNCTION app_sees_task(uuid, text, uuid, uuid, uuid, boolean);
DROP FUNCTION app_mark_saving(uuid);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_begin_as(p_principal uuid, p_on_behalf_of uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM users WHERE id = p_principal AND app_name IS NOT NULL) THEN
        RAISE EXCEPTION 'only an app principal acts on behalf of someone' USING ERRCODE = '42501';
    END IF;
    PERFORM app_begin(p_principal);
    PERFORM set_config('app.on_behalf_of', coalesce(p_on_behalf_of::text, ''), true);
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_begin(p_user uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM set_config('app.current_user_id', p_user::text, true);
    PERFORM set_config('app.permissions', ',' || array_to_string(app_permissions_of(p_user), ',') || ',', true);
    PERFORM set_config('app.on_behalf_of', '', true);
END
$$;
-- +goose StatementEnd
DROP FUNCTION app_hidden_groups_of(uuid);
ALTER TABLE groups DROP COLUMN restricted;
