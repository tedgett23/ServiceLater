-- +goose Up

-- A notification is a hint that something needs a person's attention. Like a realtime event it names what happened
-- and lets the person go fetch it through the normal RLS-protected endpoint, rather than duplicating ticket or
-- approval content here (Realtime rules in CLAUDE.md). kind is open-ended so a later notification source (an
-- assignment, an @mention) can reuse this table without a migration.
CREATE TABLE notifications (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id),
    kind        text NOT NULL,
    approval_id uuid REFERENCES workflow_approvals(id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    read_at     timestamptz
);
CREATE INDEX notifications_user_idx ON notifications (user_id, created_at DESC);

-- Only the recipient may see or mark their own notifications read. Nothing but the trigger below (SECURITY DEFINER,
-- so it runs past RLS) ever inserts one, so there is no insert policy for servicelater_app.
ALTER TABLE notifications ENABLE ROW LEVEL SECURITY;
CREATE POLICY notifications_select ON notifications FOR SELECT TO servicelater_app
    USING (user_id = app_current_user_id());
CREATE POLICY notifications_update ON notifications FOR UPDATE TO servicelater_app
    USING (user_id = app_current_user_id()) WITH CHECK (user_id = app_current_user_id());

-- A pending approval is also a notification to its approver.
-- +goose StatementBegin
CREATE FUNCTION notify_on_approval_request() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    INSERT INTO notifications (user_id, kind, approval_id) VALUES (NEW.approver_id, 'approval_pending', NEW.id);
    PERFORM pg_notify('notification_changes', json_build_object('user_id', NEW.approver_id)::text);
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER workflow_approvals_notify AFTER INSERT ON workflow_approvals
    FOR EACH ROW EXECUTE FUNCTION notify_on_approval_request();

-- Deciding it clears the notification too, the same way it already wakes the waiting execution.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION workflow_wake_on_approval_decision() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NEW.decision IS NOT NULL AND OLD.decision IS NULL THEN
        UPDATE workflow_executions SET wake_at = now()
         WHERE id = NEW.execution_id AND status = 'waiting' AND wake_at > now();
        UPDATE notifications SET read_at = now() WHERE approval_id = NEW.id AND read_at IS NULL;
        PERFORM pg_notify('notification_changes', json_build_object('user_id', NEW.approver_id)::text);
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION workflow_wake_on_approval_decision() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NEW.decision IS NOT NULL AND OLD.decision IS NULL THEN
        UPDATE workflow_executions SET wake_at = now()
         WHERE id = NEW.execution_id AND status = 'waiting' AND wake_at > now();
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
DROP TRIGGER workflow_approvals_notify ON workflow_approvals;
DROP FUNCTION notify_on_approval_request();
DROP TABLE notifications;
