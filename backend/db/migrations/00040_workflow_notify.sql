-- +goose Up

-- Nobody is told about their own change, nor is the person an app acts for (an async rule set off by their change).
-- A workflow step is different: it happens later, because someone decided or a task was finished, so the person it
-- runs for (the requester whose order started it) is told what it did. The engine marks its transactions.
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

-- +goose Down
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
