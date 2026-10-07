-- +goose Up

-- Record permissions are scoped by record type (docs/apps-platform.md §11.1). Instead of tickets:read_all,
-- tickets:update and tickets:delete, which covered every type, a permission names a verb and a type:
-- records:<verb>:<type>, with * for every type (records:update:*). The verbs are fixed (read, create, update,
-- delete), so adding a record type adds catalog entries, not code. The built-in roles keep exactly what they could do
-- before, as records:<verb>:*. This renames permissions in place, which is allowed until the first release.

-- Whether anyone signed in may file a record of this type as themselves, without records:create:<type>. Incidents are
-- open to everyone, as they always were; a change needs the permission.
ALTER TABLE task_types ADD COLUMN anyone_may_file boolean NOT NULL DEFAULT false;
UPDATE task_types SET anyone_may_file = true WHERE type = 'incident';

-- +goose StatementBegin
CREATE FUNCTION app_record_permission_rename(p text) RETURNS text
    LANGUAGE sql IMMUTABLE
    AS $$
    SELECT CASE p
        WHEN 'tickets:read_all' THEN 'records:read:*'
        WHEN 'tickets:update' THEN 'records:update:*'
        WHEN 'tickets:delete' THEN 'records:delete:*'
        ELSE p END
$$;
-- +goose StatementEnd

-- Roles apps defined, and what app principals were approved for, are renamed too: a role or manifest that named
-- tickets:update meant every type. Reinstalling the app writes its own names over these.
UPDATE roles SET permissions = ARRAY(SELECT DISTINCT app_record_permission_rename(p) FROM unnest(permissions) p ORDER BY 1)
WHERE app_name IS NOT NULL;
UPDATE apps SET manifest = jsonb_set(manifest, '{permissions}',
    (SELECT coalesce(jsonb_agg(DISTINCT app_record_permission_rename(p)), '[]') FROM jsonb_array_elements_text(manifest -> 'permissions') p))
WHERE jsonb_typeof(manifest -> 'permissions') = 'array';

-- Filing a record for someone else is creating it and working it, so agents get records:create:* too.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{comments:delete,comments:internal,groups:manage,records:create:*,records:delete:*,records:read:*,records:update:*,roles:manage,users:manage,users:read,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{comments:internal,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();

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

-- +goose StatementBegin
CREATE FUNCTION app_anyone_may_file(p_type text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$ SELECT coalesce((SELECT anyone_may_file FROM task_types WHERE type = p_type), false) $$;
-- +goose StatementEnd

-- Requesters always see their own records, whatever the type. Filing as yourself needs records:create:<type>, unless
-- the type is open to everyone; filing for someone else needs records:update:<type> as well.
ALTER POLICY tasks_select ON tasks USING (app_can_record('read', type) OR requester_id = app_current_user_id());
ALTER POLICY tasks_insert ON tasks
    WITH CHECK ((app_can_record('create', type) OR app_anyone_may_file(type))
                AND (requester_id = app_current_user_id() OR app_can_record('update', type)));
ALTER POLICY tasks_update ON tasks USING (app_can_record('update', type)) WITH CHECK (app_can_record('update', type));
ALTER POLICY tasks_delete ON tasks USING (app_can_record('delete', type));
ALTER POLICY incidents_update ON incidents
    USING (app_can_record('update', 'incident') AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id))
    WITH CHECK (app_can_record('update', 'incident'));

-- Comment events carry the record's type, so the hub can route them by records:read:<type>.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION comments_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('comment_changes', (SELECT json_build_object(
        'op', TG_OP, 'id', NEW.id, 'ticket_id', NEW.task_id, 'internal', NEW.internal,
        'requester_id', t.requester_id, 'type', t.type) FROM tasks t WHERE t.id = NEW.task_id)::text);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

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
           AND u.id IS DISTINCT FROM behalf
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

-- Still defined for anything outside this repository; nothing here calls them (TestNothingChecksRolesInSQL).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_is_staff() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT app_can('records:read:*') $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_is_staff() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT app_can('tickets:read_all') $$;
-- +goose StatementEnd
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
           AND u.id IS DISTINCT FROM behalf
           AND u.app_name IS NULL
           AND u.deactivated_at IS NULL
           AND (u.id = t.requester_id OR app_user_can(u.id, 'tickets:read_all'))
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION comments_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('comment_changes', json_build_object(
        'op', TG_OP, 'id', NEW.id, 'ticket_id', NEW.task_id, 'internal', NEW.internal,
        'requester_id', (SELECT requester_id FROM tasks WHERE id = NEW.task_id))::text);
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER POLICY incidents_update ON incidents
    USING (app_can('tickets:update') AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id))
    WITH CHECK (app_can('tickets:update'));
ALTER POLICY tasks_delete ON tasks USING (app_can('tickets:delete'));
ALTER POLICY tasks_update ON tasks USING (app_can('tickets:update')) WITH CHECK (app_can('tickets:update'));
ALTER POLICY tasks_insert ON tasks WITH CHECK (app_can('tickets:update') OR requester_id = app_current_user_id());
ALTER POLICY tasks_select ON tasks USING (app_can('tickets:read_all') OR requester_id = app_current_user_id());
DROP FUNCTION app_anyone_may_file(text);
DROP FUNCTION app_user_can_record_any(uuid, text);
DROP FUNCTION app_user_can_record(uuid, text, text);
DROP FUNCTION app_can_record(text, text);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
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
-- +goose StatementBegin
CREATE FUNCTION app_record_permission_unrename(p text) RETURNS text
    LANGUAGE sql IMMUTABLE
    AS $$
    SELECT CASE
        WHEN p LIKE 'records:read:%' THEN 'tickets:read_all'
        WHEN p LIKE 'records:update:%' THEN 'tickets:update'
        WHEN p LIKE 'records:delete:%' THEN 'tickets:delete'
        ELSE p END
$$;
-- +goose StatementEnd
UPDATE roles SET permissions = ARRAY(SELECT DISTINCT app_record_permission_unrename(p) FROM unnest(permissions) p
                                     WHERE p NOT LIKE 'records:create:%' ORDER BY 1)
WHERE app_name IS NOT NULL;
UPDATE apps SET manifest = jsonb_set(manifest, '{permissions}',
    (SELECT coalesce(jsonb_agg(DISTINCT app_record_permission_unrename(p)), '[]') FROM jsonb_array_elements_text(manifest -> 'permissions') p
     WHERE p NOT LIKE 'records:create:%'))
WHERE jsonb_typeof(manifest -> 'permissions') = 'array';
DROP FUNCTION app_record_permission_unrename(text);
DROP FUNCTION app_record_permission_rename(text);
ALTER TABLE task_types DROP COLUMN anyone_may_file;
