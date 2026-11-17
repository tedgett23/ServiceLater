-- +goose Up
-- A change can record the problem it fixes (docs/apps-platform.md §9.7), as an incident records the problem behind
-- it: several changes may fix one problem (a first attempt, a retry). Same shape as incidents.problem_id (00045).
ALTER TABLE changes
    ADD COLUMN problem_id uuid,
    ADD COLUMN problem_type text GENERATED ALWAYS AS (CASE WHEN problem_id IS NULL THEN NULL ELSE 'problem' END) STORED,
    ADD CONSTRAINT changes_problem_fk FOREIGN KEY (problem_id, problem_type) REFERENCES tasks (id, type);
CREATE INDEX changes_problem_idx ON changes (problem_id) WHERE problem_id IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION changes_problem_history() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM app_history_change('task', NEW.task_id, 'problem_id', to_jsonb(OLD.problem_id), to_jsonb(NEW.problem_id));
    RETURN NULL;
END $$;

-- A linked change moving is news to its problem's watchers too.
CREATE OR REPLACE FUNCTION tasks_notify_problem() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM task_cis_notify_for(i.problem_id) FROM incidents i WHERE i.task_id = NEW.id AND i.problem_id IS NOT NULL;
    PERFORM task_cis_notify_for(c.problem_id) FROM changes c WHERE c.task_id = NEW.id AND c.problem_id IS NOT NULL;
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER changes_problem_history AFTER UPDATE OF problem_id ON changes
    FOR EACH ROW WHEN (OLD.problem_id IS DISTINCT FROM NEW.problem_id) EXECUTE FUNCTION changes_problem_history();
-- incidents_notify_problem only reads problem_id, so it serves changes as well.
CREATE TRIGGER changes_notify_problem AFTER UPDATE OF problem_id ON changes
    FOR EACH ROW WHEN (OLD.problem_id IS DISTINCT FROM NEW.problem_id) EXECUTE FUNCTION incidents_notify_problem();
DROP TRIGGER tasks_notify_problem ON tasks;
CREATE TRIGGER tasks_notify_problem AFTER UPDATE OF status ON tasks
    FOR EACH ROW WHEN (NEW.type IN ('incident', 'change') AND OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION tasks_notify_problem();

-- +goose Down
DROP TRIGGER tasks_notify_problem ON tasks;
CREATE TRIGGER tasks_notify_problem AFTER UPDATE OF status ON tasks
    FOR EACH ROW WHEN (NEW.type = 'incident' AND OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION tasks_notify_problem();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tasks_notify_problem() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM task_cis_notify_for(i.problem_id) FROM incidents i WHERE i.task_id = NEW.id AND i.problem_id IS NOT NULL;
    RETURN NULL;
END $$;
-- +goose StatementEnd
DROP TRIGGER changes_notify_problem ON changes;
DROP TRIGGER changes_problem_history ON changes;
DROP FUNCTION changes_problem_history();
DROP INDEX changes_problem_idx;
ALTER TABLE changes DROP CONSTRAINT changes_problem_fk, DROP COLUMN problem_type, DROP COLUMN problem_id;
