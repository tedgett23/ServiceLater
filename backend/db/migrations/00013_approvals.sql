-- +goose Up

-- An `approval` workflow step's requests, one row per approver it asked (docs/apps-platform.md §7.2). Whether the step
-- is done, and which way, is computed from these rows and the step's rule (any, all, majority); there is no separate
-- "decision" column on the execution because a workflow may ask more than once (a different approval step later on).
CREATE TABLE workflow_approvals (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    execution_id uuid NOT NULL REFERENCES workflow_executions(id) ON DELETE CASCADE,
    step_id      text NOT NULL,
    approver_id  uuid NOT NULL REFERENCES users(id),
    decision     text CHECK (decision IN ('approved', 'rejected')),
    comment      text NOT NULL DEFAULT '',
    decided_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (execution_id, step_id, approver_id)
);
-- The approvals inbox: one person's undecided requests.
CREATE INDEX workflow_approvals_pending_idx ON workflow_approvals (approver_id) WHERE decision IS NULL;
-- The engine reading a step's requests back, and the wake trigger below finding the execution to nudge.
CREATE INDEX workflow_approvals_step_idx ON workflow_approvals (execution_id, step_id);

-- Requests are visible to the approver they were sent to, to staff, and to whoever started the run (the run viewer).
-- Only the approver may decide, and only once: an already-decided row no longer matches the update policy.
ALTER TABLE workflow_approvals ENABLE ROW LEVEL SECURITY;
CREATE POLICY workflow_approvals_select ON workflow_approvals FOR SELECT TO servicelater_app
    USING (approver_id = app_current_user_id() OR app_is_staff()
        OR EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id AND e.started_by = app_current_user_id()));
CREATE POLICY workflow_approvals_insert ON workflow_approvals FOR INSERT TO servicelater_app
    WITH CHECK (EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id AND e.started_by = app_current_user_id()));
CREATE POLICY workflow_approvals_update ON workflow_approvals FOR UPDATE TO servicelater_app
    USING (approver_id = app_current_user_id() AND decision IS NULL)
    WITH CHECK (approver_id = app_current_user_id());

-- A waiting execution is nudged awake as soon as one of its pending approvals is decided (the timer, every second in
-- the worker role, then picks it up), the same way a `wait until` is brought forward when its ticket changes.
-- +goose StatementBegin
CREATE FUNCTION workflow_wake_on_approval_decision() RETURNS trigger
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
CREATE TRIGGER workflow_approvals_wake AFTER UPDATE ON workflow_approvals
    FOR EACH ROW EXECUTE FUNCTION workflow_wake_on_approval_decision();

-- +goose Down
DROP TRIGGER workflow_approvals_wake ON workflow_approvals;
DROP FUNCTION workflow_wake_on_approval_decision();
DROP TABLE workflow_approvals;
