-- +goose Up
-- Comments that tell people (docs/apps-platform.md §6.5): a comment is an event rules can act on (on: [comment]),
-- a comment can @mention people, and a notification can be about one comment.

-- Who a comment mentions. Written with the comment, by its author, in the same save; the save path has already
-- checked that each person may read the comment (the record, and internal notes only to comments:internal). Seen
-- when the comment is.
CREATE TABLE task_comment_mentions (
    comment_id uuid NOT NULL REFERENCES task_comments (id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (comment_id, user_id)
);
CREATE INDEX task_comment_mentions_user_idx ON task_comment_mentions (user_id);
ALTER TABLE task_comment_mentions ENABLE ROW LEVEL SECURITY;
CREATE POLICY comment_mentions_select ON task_comment_mentions FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM task_comments c WHERE c.id = comment_id));
CREATE POLICY comment_mentions_insert ON task_comment_mentions FOR INSERT TO servicelater_app
    WITH CHECK (EXISTS (SELECT 1 FROM task_comments c WHERE c.id = comment_id AND c.author_id = app_current_user_id()));

-- The comment a notification is about, when it is about one: its text is filled in when the recipient reads it, and
-- the notification drops out if they cannot read the comment.
ALTER TABLE notifications ADD COLUMN comment_id uuid REFERENCES task_comments (id) ON DELETE CASCADE;

-- app_notify_comment is app_notify for a notification about a comment: the same rules for who is told, and the
-- comment must be on the record and readable by each recipient, so an internal note reaches only comments:internal.
-- +goose StatementBegin
CREATE FUNCTION app_notify_comment(p_users uuid[], p_task uuid, p_comment uuid, p_app text, p_template text) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    actor uuid := app_current_user_id();
    behalf uuid := app_on_behalf_of();
    is_internal boolean;
    recipient uuid;
    sent integer := 0;
BEGIN
    IF actor IS NULL OR NOT EXISTS (
        SELECT 1 FROM tasks t WHERE t.id = p_task
           AND app_sees_task(t.id, t.type, t.requester_id, t.assignee_id, t.assignment_group_id, true)) THEN
        RAISE EXCEPTION 'not allowed to notify about this record' USING ERRCODE = '42501';
    END IF;
    SELECT c.internal INTO is_internal FROM task_comments c WHERE c.id = p_comment AND c.task_id = p_task;
    IF NOT FOUND OR (is_internal AND NOT app_can('comments:internal')) THEN
        RAISE EXCEPTION 'not allowed to notify about this comment' USING ERRCODE = '42501';
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
           AND (NOT is_internal OR app_user_can(u.id, 'comments:internal'))
    LOOP
        INSERT INTO notifications (user_id, kind, task_id, comment_id, app, template, actor_id)
        VALUES (recipient, 'template', p_task, p_comment, p_app, p_template, coalesce(behalf, actor));
        PERFORM pg_notify('notification_changes', json_build_object('user_id', recipient)::text);
        sent := sent + 1;
    END LOOP;
    RETURN sent;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_notify_comment(uuid[], uuid, uuid, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_notify_comment(uuid[], uuid, uuid, text, text) TO servicelater_app;

-- +goose Down
DROP FUNCTION app_notify_comment(uuid[], uuid, uuid, text, text);
ALTER TABLE notifications DROP COLUMN comment_id;
DROP TABLE task_comment_mentions;
