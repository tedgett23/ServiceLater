-- +goose Up

-- App service principals (docs/apps-platform.md §11, D9). Each installed app acts as an identity of its own: a users
-- row with app_name set, which cannot sign in (no credentials, and login refuses it), never shows in the directory or
-- pickers, and holds exactly the permissions its manifest declares, approved at install. Async rules and workflows
-- run as it, on behalf of the person who caused them; so do after rules that say run_as: app.
ALTER TABLE users ADD COLUMN app_name text UNIQUE;
ALTER TABLE users ADD CONSTRAINT users_app_principal_check CHECK (app_name IS NULL OR role = 'end_user');

-- The person a principal is acting for in this transaction, or NULL when someone is acting for themselves.
-- +goose StatementBegin
CREATE FUNCTION app_on_behalf_of() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$ SELECT nullif(current_setting('app.on_behalf_of', true), '')::uuid $$;
-- +goose StatementEnd

-- An app's principal, created the first time it is needed (an install, or a test that wiped users). It is not
-- reactivated here: uninstalling an app deactivates its principal, and only installing it again brings it back.
-- +goose StatementBegin
CREATE FUNCTION app_principal(p_app text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    id uuid;
BEGIN
    SELECT u.id INTO id FROM users u WHERE u.app_name = p_app;
    IF id IS NULL THEN
        INSERT INTO users (email, name, role, app_name)
        VALUES (p_app || '@apps.servicelater.invalid', p_app, 'end_user', p_app)
        ON CONFLICT (app_name) DO NOTHING
        RETURNING users.id INTO id;
        IF id IS NULL THEN
            SELECT u.id INTO id FROM users u WHERE u.app_name = p_app;
        END IF;
    END IF;
    RETURN id;
END
$$;
-- +goose StatementEnd

-- Permissions: a person's come from roles; an app principal's from its manifest (catalog names only; the notify: and
-- http: capabilities are declared there too, and checked by the steps that use them).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_permissions_of(p_user uuid) RETURNS text[]
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

-- How a principal starts a transaction: as itself, with its permissions, on behalf of someone.
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
END
$$;
-- +goose StatementEnd

-- And how a person starts one, which also clears any on_behalf_of left by a switch back from a principal.
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

-- An app's comments say whom it acted for.
ALTER TABLE task_comments ADD COLUMN on_behalf_of uuid REFERENCES users (id) DEFAULT app_on_behalf_of();

-- A workflow runs as its app's principal, for the person who started it. Executions remember which principal, so
-- the policies can let it do the engine's bookkeeping.
ALTER TABLE workflow_executions ADD COLUMN run_as uuid REFERENCES users (id);
CREATE INDEX workflow_executions_run_as_idx ON workflow_executions (run_as) WHERE run_as IS NOT NULL;
UPDATE workflow_executions SET run_as = app_principal(app);

ALTER POLICY workflow_executions_insert ON workflow_executions
    WITH CHECK (started_by = app_current_user_id()
                OR (run_as = app_current_user_id() AND started_by = app_on_behalf_of()));
ALTER POLICY workflow_executions_select ON workflow_executions
    USING (app_can('workflows:read_all') OR started_by = app_current_user_id() OR run_as = app_current_user_id()
           OR app_has_approval_on(id));
ALTER POLICY workflow_executions_update ON workflow_executions
    USING (started_by = app_current_user_id() OR run_as = app_current_user_id() OR app_can('workflows:run'))
    WITH CHECK (started_by = app_current_user_id() OR run_as = app_current_user_id() OR app_can('workflows:run'));
ALTER POLICY workflow_steps_insert ON workflow_steps
    WITH CHECK (EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id
                        AND (e.started_by = app_current_user_id() OR e.run_as = app_current_user_id())));
ALTER POLICY workflow_approvals_insert ON workflow_approvals
    WITH CHECK (EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id
                        AND (e.started_by = app_current_user_id() OR e.run_as = app_current_user_id())));
ALTER POLICY workflow_approvals_select ON workflow_approvals
    USING (approver_id = app_current_user_id() OR app_can('workflows:read_all')
        OR EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id
                   AND (e.started_by = app_current_user_id() OR e.run_as = app_current_user_id())));

-- Notifications: never to whoever made the change, which for an app is the person it acted for as well.
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
           AND u.deactivated_at IS NULL
           AND u.app_name IS NULL
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

-- +goose Down
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
ALTER POLICY workflow_approvals_select ON workflow_approvals
    USING (approver_id = app_current_user_id() OR app_can('workflows:read_all')
        OR EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id AND e.started_by = app_current_user_id()));
ALTER POLICY workflow_approvals_insert ON workflow_approvals
    WITH CHECK (EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id AND e.started_by = app_current_user_id()));
ALTER POLICY workflow_steps_insert ON workflow_steps
    WITH CHECK (EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id AND e.started_by = app_current_user_id()));
ALTER POLICY workflow_executions_update ON workflow_executions
    USING (started_by = app_current_user_id() OR app_can('workflows:run'))
    WITH CHECK (started_by = app_current_user_id() OR app_can('workflows:run'));
ALTER POLICY workflow_executions_select ON workflow_executions
    USING (app_can('workflows:read_all') OR started_by = app_current_user_id() OR app_has_approval_on(id));
ALTER POLICY workflow_executions_insert ON workflow_executions WITH CHECK (started_by = app_current_user_id());
DROP INDEX workflow_executions_run_as_idx;
ALTER TABLE workflow_executions DROP COLUMN run_as;
ALTER TABLE task_comments DROP COLUMN on_behalf_of;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_begin(p_user uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM set_config('app.current_user_id', p_user::text, true);
    PERFORM set_config('app.permissions', ',' || array_to_string(app_permissions_of(p_user), ',') || ',', true);
END
$$;
-- +goose StatementEnd
DROP FUNCTION app_begin_as(uuid, uuid);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_permissions_of(p_user uuid) RETURNS text[]
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
DROP FUNCTION app_principal(text);
DROP FUNCTION app_on_behalf_of();
DELETE FROM users WHERE app_name IS NOT NULL;
ALTER TABLE users DROP CONSTRAINT users_app_principal_check;
ALTER TABLE users DROP COLUMN app_name;
