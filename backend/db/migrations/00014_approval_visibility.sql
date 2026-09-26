-- +goose Up

-- The approvals inbox joins an execution to the ticket it is about, so an approver who is neither staff nor the
-- person who started the run still needs to see that one execution row: the one holding a request of theirs. A plain
-- EXISTS against workflow_approvals would recurse (that table's own policy looks back at workflow_executions), so
-- this checks it through a SECURITY DEFINER function that reads past RLS instead.
-- +goose StatementBegin
CREATE FUNCTION app_has_approval_on(exec_id uuid) RETURNS boolean
    LANGUAGE sql SECURITY DEFINER STABLE SET search_path = public
    AS $$
    SELECT EXISTS (SELECT 1 FROM workflow_approvals WHERE execution_id = exec_id AND approver_id = app_current_user_id());
$$;
-- +goose StatementEnd

DROP POLICY workflow_executions_select ON workflow_executions;
CREATE POLICY workflow_executions_select ON workflow_executions FOR SELECT TO servicelater_app
    USING (app_is_staff() OR started_by = app_current_user_id() OR app_has_approval_on(id));

-- The inbox also needs the ticket's reference and title, and an approver may otherwise have no right to the ticket
-- at all (that is the point of asking someone outside its normal audience). This hands back only those two columns,
-- for a task this person has a pending or decided approval on, never the ticket itself.
-- +goose StatementBegin
CREATE FUNCTION app_approval_task_summary(task_id uuid) RETURNS TABLE(reference text, title text)
    LANGUAGE sql SECURITY DEFINER STABLE SET search_path = public
    AS $$
    SELECT COALESCE(tt.prefix || t.number::text, ''), COALESCE(t.title, '')
    FROM tasks t LEFT JOIN task_types tt ON tt.type = t.type
    WHERE t.id = task_id
      AND EXISTS (SELECT 1 FROM workflow_approvals wa JOIN workflow_executions e ON e.id = wa.execution_id
                  WHERE e.record_id = task_id AND wa.approver_id = app_current_user_id());
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION app_approval_task_summary(uuid);
DROP POLICY workflow_executions_select ON workflow_executions;
CREATE POLICY workflow_executions_select ON workflow_executions FOR SELECT TO servicelater_app
    USING (app_is_staff() OR started_by = app_current_user_id());
DROP FUNCTION app_has_approval_on(uuid);
