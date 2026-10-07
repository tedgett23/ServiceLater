-- +goose Up

-- Notifications an app sends with a template (notifications/*.yaml) through a rule's notify action. Like the
-- approval kind, the row says what and about which record, never the text: the template is filled in when the
-- recipient reads it, from the record as they are allowed to see it.
ALTER TABLE notifications
    ADD COLUMN task_id  uuid REFERENCES tasks (id) ON DELETE CASCADE,
    ADD COLUMN app      text,
    ADD COLUMN template text,
    ADD COLUMN actor_id uuid REFERENCES users (id) ON DELETE SET NULL;
ALTER TABLE notifications ADD CONSTRAINT notifications_template_check
    CHECK (kind <> 'template' OR (task_id IS NOT NULL AND app IS NOT NULL AND template IS NOT NULL));
CREATE INDEX notifications_task_idx ON notifications (task_id, user_id) WHERE task_id IS NOT NULL;

-- app_notify delivers one template notification about a ticket. Rules run as the person making the change, who may
-- not write other people's notifications, so this runs past RLS and decides who gets one itself. Whatever the rule
-- asked for, nobody is told:
--   - about their own change;
--   - while deactivated;
--   - about a ticket they cannot see (the tasks_select rule: staff, or its requester), so an end user in a group
--     never hears of someone else's ticket.
-- The person acting must be able to see the ticket, and the template must be installed. It returns how many were
-- sent. A rule that names a thousand people is a mistake, not a notification.
-- +goose StatementBegin
CREATE FUNCTION app_notify(p_users uuid[], p_task uuid, p_app text, p_template text) RETURNS integer
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

-- +goose Down
DROP FUNCTION app_notify(uuid[], uuid, text, text);
DELETE FROM notifications WHERE kind = 'template';
DROP INDEX notifications_task_idx;
ALTER TABLE notifications DROP CONSTRAINT notifications_template_check;
ALTER TABLE notifications DROP COLUMN actor_id, DROP COLUMN template, DROP COLUMN app, DROP COLUMN task_id;
