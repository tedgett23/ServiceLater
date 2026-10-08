-- +goose Up

-- The builder shows where a workflow's runs are, live (docs/apps-platform.md §7.7): it asks per workflow, and hears
-- when a run starts, moves to another step or ends. Events carry ids and names only, like every other.
CREATE INDEX workflow_executions_workflow_idx ON workflow_executions (app, workflow, created_at DESC);
CREATE INDEX workflow_executions_in_flight_idx ON workflow_executions (app, workflow, current_step)
    WHERE status IN ('queued', 'running', 'waiting');

-- +goose StatementBegin
CREATE FUNCTION workflow_executions_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM NEW.status OR OLD.current_step IS DISTINCT FROM NEW.current_step THEN
        PERFORM pg_notify('run_changes', json_build_object('id', NEW.id, 'app', NEW.app, 'workflow', NEW.workflow)::text);
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER workflow_executions_notify AFTER INSERT OR UPDATE ON workflow_executions
    FOR EACH ROW EXECUTE FUNCTION workflow_executions_notify();

-- +goose Down
DROP TRIGGER workflow_executions_notify ON workflow_executions;
DROP FUNCTION workflow_executions_notify();
DROP INDEX workflow_executions_in_flight_idx;
DROP INDEX workflow_executions_workflow_idx;
