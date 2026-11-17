-- +goose Up
-- Problem management (docs/apps-platform.md §9.7): an incident may be linked to the problem behind it, one problem
-- per incident, as in ServiceNow. The link lives in the incident's own row. A generated column makes the foreign key
-- prove the other end is a problem, the way the extension tables prove their own type; deleting a problem that
-- incidents point at is refused (no ON DELETE action), so a link never vanishes outside the save path.
ALTER TABLE incidents
    ADD COLUMN problem_id uuid,
    ADD COLUMN problem_type text GENERATED ALWAYS AS (CASE WHEN problem_id IS NULL THEN NULL ELSE 'problem' END) STORED,
    ADD CONSTRAINT incidents_problem_fk FOREIGN KEY (problem_id, problem_type) REFERENCES tasks (id, type);
CREATE INDEX incidents_problem_idx ON incidents (problem_id) WHERE problem_id IS NOT NULL;

-- The link is part of the incident's history, like its resolution.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION incidents_history() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM app_history_change('task', NEW.task_id, 'resolution', to_jsonb(OLD.resolution), to_jsonb(NEW.resolution));
    PERFORM app_history_change('task', NEW.task_id, 'problem_id', to_jsonb(OLD.problem_id), to_jsonb(NEW.problem_id));
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- Whoever has a problem open sees its incidents come, go and move: linking or unlinking one, or a linked incident
-- changing status, is a change to the problem too (ids only, as ever).
-- +goose StatementBegin
CREATE FUNCTION incidents_notify_problem() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF OLD.problem_id IS NOT NULL THEN
        PERFORM task_cis_notify_for(OLD.problem_id);
    END IF;
    IF NEW.problem_id IS NOT NULL AND NEW.problem_id IS DISTINCT FROM OLD.problem_id THEN
        PERFORM task_cis_notify_for(NEW.problem_id);
    END IF;
    RETURN NULL;
END $$;

CREATE FUNCTION tasks_notify_problem() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM task_cis_notify_for(i.problem_id) FROM incidents i WHERE i.task_id = NEW.id AND i.problem_id IS NOT NULL;
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER incidents_notify_problem AFTER UPDATE OF problem_id ON incidents
    FOR EACH ROW WHEN (OLD.problem_id IS DISTINCT FROM NEW.problem_id) EXECUTE FUNCTION incidents_notify_problem();
CREATE TRIGGER tasks_notify_problem AFTER UPDATE OF status ON tasks
    FOR EACH ROW WHEN (NEW.type = 'incident' AND OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION tasks_notify_problem();

-- +goose Down
DROP TRIGGER tasks_notify_problem ON tasks;
DROP TRIGGER incidents_notify_problem ON incidents;
DROP FUNCTION tasks_notify_problem();
DROP FUNCTION incidents_notify_problem();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION incidents_history() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM app_history_change('task', NEW.task_id, 'resolution', to_jsonb(OLD.resolution), to_jsonb(NEW.resolution));
    RETURN NULL;
END $$;
-- +goose StatementEnd
DROP INDEX incidents_problem_idx;
ALTER TABLE incidents DROP CONSTRAINT incidents_problem_fk, DROP COLUMN problem_type, DROP COLUMN problem_id;
