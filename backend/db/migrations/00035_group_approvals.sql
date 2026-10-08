-- +goose Up

-- An approval step can ask groups as well as people (docs/apps-platform.md §7.4): each group named in `groups` has
-- one vote, which any of its members may cast, so "the owners of every item this change touches must approve" asks
-- each owning team once. group_name is the group as it was asked (a name, not a reference: deleting or renaming the
-- group later must not change a decision); NULL is a person asked in their own right. Someone in two of the groups is
-- asked once per group.
ALTER TABLE workflow_approvals ADD COLUMN group_name text;
-- A request nobody needs to answer any more: another member of its group decided, or the step is over (an any-rule
-- approval that someone else settled, a timeout, a cancelled run). It leaves the inbox and can no longer be decided.
ALTER TABLE workflow_approvals ADD COLUMN closed_at timestamptz;
ALTER TABLE workflow_approvals DROP CONSTRAINT workflow_approvals_execution_id_step_id_approver_id_key;
CREATE UNIQUE INDEX workflow_approvals_request_key ON workflow_approvals (execution_id, step_id, approver_id, group_name) NULLS NOT DISTINCT;
DROP INDEX workflow_approvals_pending_idx;
CREATE INDEX workflow_approvals_pending_idx ON workflow_approvals (approver_id) WHERE decision IS NULL AND closed_at IS NULL;

-- Closes undecided requests and clears their notifications, telling each approver's bell.
-- +goose StatementBegin
CREATE FUNCTION app_close_approvals(p_execution uuid, p_step text, p_group text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    closed record;
BEGIN
    FOR closed IN
        UPDATE workflow_approvals SET closed_at = now()
         WHERE execution_id = p_execution AND (p_step IS NULL OR step_id = p_step)
           AND (p_group IS NULL OR group_name = p_group) AND decision IS NULL AND closed_at IS NULL
        RETURNING id, approver_id
    LOOP
        UPDATE notifications SET read_at = now() WHERE approval_id = closed.id AND read_at IS NULL;
        PERFORM pg_notify('notification_changes', json_build_object('user_id', closed.approver_id)::text);
    END LOOP;
END $$;

CREATE OR REPLACE FUNCTION workflow_wake_on_approval_decision() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NEW.decision IS NOT NULL AND OLD.decision IS NULL THEN
        UPDATE workflow_executions SET wake_at = now()
         WHERE id = NEW.execution_id AND status = 'waiting' AND wake_at > now();
        UPDATE notifications SET read_at = now() WHERE approval_id = NEW.id AND read_at IS NULL;
        PERFORM pg_notify('notification_changes', json_build_object('user_id', NEW.approver_id)::text);
        -- One member answered for the group.
        IF NEW.group_name IS NOT NULL THEN
            PERFORM app_close_approvals(NEW.execution_id, NEW.step_id, NEW.group_name);
        END IF;
    END IF;
    RETURN NULL;
END $$;

-- A step that is over leaves nobody waiting to be asked: when a run moves past an approval step, or ends.
CREATE FUNCTION workflow_close_left_approvals() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NEW.status IN ('done', 'failed', 'cancelled') AND OLD.status NOT IN ('done', 'failed', 'cancelled') THEN
        PERFORM app_close_approvals(NEW.id, NULL, NULL);
    ELSIF OLD.current_step IS DISTINCT FROM NEW.current_step THEN
        PERFORM app_close_approvals(NEW.id, OLD.current_step, NULL);
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_close_approvals(uuid, text, text) FROM PUBLIC;
CREATE TRIGGER workflow_close_left_approvals AFTER UPDATE ON workflow_executions
    FOR EACH ROW EXECUTE FUNCTION workflow_close_left_approvals();

-- Requests already left over by finished steps.
UPDATE workflow_approvals a SET closed_at = now()
  FROM workflow_executions e
 WHERE e.id = a.execution_id AND a.decision IS NULL
   AND (e.status IN ('done', 'failed', 'cancelled') OR e.current_step <> a.step_id);
UPDATE notifications n SET read_at = now()
  FROM workflow_approvals a WHERE a.id = n.approval_id AND a.closed_at IS NOT NULL AND n.read_at IS NULL;

-- +goose Down
DROP TRIGGER workflow_close_left_approvals ON workflow_executions;
DROP FUNCTION workflow_close_left_approvals();
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
DROP FUNCTION app_close_approvals(uuid, text, text);
DROP INDEX workflow_approvals_pending_idx;
CREATE INDEX workflow_approvals_pending_idx ON workflow_approvals (approver_id) WHERE decision IS NULL;
DROP INDEX workflow_approvals_request_key;
-- Group requests do not fit the old one-request-per-person rule; keep the first of each.
DELETE FROM workflow_approvals a USING workflow_approvals b
 WHERE a.execution_id = b.execution_id AND a.step_id = b.step_id AND a.approver_id = b.approver_id
   AND (a.created_at, a.id) > (b.created_at, b.id);
ALTER TABLE workflow_approvals ADD CONSTRAINT workflow_approvals_execution_id_step_id_approver_id_key UNIQUE (execution_id, step_id, approver_id);
ALTER TABLE workflow_approvals DROP COLUMN closed_at;
ALTER TABLE workflow_approvals DROP COLUMN group_name;
