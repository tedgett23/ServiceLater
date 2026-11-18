-- +goose Up
-- Subflows and foreach (docs/apps-platform.md §7): a step starts child runs of another workflow of the same app and
-- waits for them. A child knows its parent run, the step that started it and its place in a foreach; the rows are
-- the truth the parent counts on every visit.
ALTER TABLE workflow_executions ADD COLUMN parent_id uuid REFERENCES workflow_executions (id);
ALTER TABLE workflow_executions ADD COLUMN parent_step text;
ALTER TABLE workflow_executions ADD COLUMN parent_index integer;
-- depth is how many subflows deep a run is: a child of a child is 2. The engine stops at 5.
ALTER TABLE workflow_executions ADD COLUMN depth integer NOT NULL DEFAULT 0;
CREATE INDEX workflow_executions_parent_idx ON workflow_executions (parent_id, parent_step) WHERE parent_id IS NOT NULL;

-- A child that finishes wakes its parent (the timer then runs it); a run that fails or is cancelled cancels its
-- children still going, and theirs.
-- +goose StatementBegin
CREATE FUNCTION workflow_executions_family() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NEW.status IN ('done', 'failed', 'cancelled') AND OLD.status NOT IN ('done', 'failed', 'cancelled') THEN
        IF NEW.parent_id IS NOT NULL THEN
            UPDATE workflow_executions SET wake_at = now() WHERE id = NEW.parent_id AND status = 'waiting';
        END IF;
        IF NEW.status IN ('failed', 'cancelled') THEN
            UPDATE workflow_executions SET status = 'cancelled', error = 'the run that started it ended', finished_at = now(),
                   updated_at = now()
             WHERE parent_id = NEW.id AND status IN ('queued', 'running', 'waiting');
        END IF;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER workflow_executions_family AFTER UPDATE OF status ON workflow_executions
    FOR EACH ROW EXECUTE FUNCTION workflow_executions_family();

-- Scheduled workflows: when each last fired, so every worker's timer can try and only one starts it.
CREATE TABLE workflow_schedules (
    app      text NOT NULL,
    workflow text NOT NULL,
    last_due timestamptz NOT NULL,
    PRIMARY KEY (app, workflow)
);
ALTER TABLE workflow_schedules ENABLE ROW LEVEL SECURITY;

-- Finding records for a query step or a schedule's for_each: the ids of one type's records matching every filter,
-- oldest first, at most 1000, as the caller (RLS applies). The fields are a lookup's (app_lookup_count) and a few
-- more; apps.QueryFields keeps the Go side in step (TestQueryFieldsMatchTheDatabase).
-- +goose StatementBegin
CREATE FUNCTION app_find_records(p_type text, p_filters jsonb, p_limit integer) RETURNS SETOF uuid
    LANGUAGE plpgsql STABLE SET search_path = public
    AS $$
DECLARE
    fields  jsonb;
    conds   text[] := ARRAY['true'];
    f       jsonb;
    i       int := 0;
    col     text;
    kind    text;
    cast_to text;
    op      text;
BEGIN
    IF p_type = 'task' OR NOT EXISTS (SELECT 1 FROM task_types WHERE type = p_type) THEN
        RAISE EXCEPTION '% is not a record type to find', p_type USING ERRCODE = '22023';
    END IF;
    fields := '{"id": ["x.id", "id"], "status": ["x.status", "text"], "state_category": ["x.state_category", "text"],
                "requester_id": ["x.requester_id", "id"], "assignee_id": ["x.assignee_id", "id"],
                "assignment_group_id": ["x.assignment_group_id", "id"], "parent_id": ["x.parent_id", "id"],
                "created_at": ["x.created_at", "time"], "updated_at": ["x.updated_at", "time"]}';
    IF p_type = 'change' THEN
        fields := fields || '{"change_type": ["c.change_type", "text"], "planned_start": ["c.planned_start", "time"],
                              "planned_end": ["c.planned_end", "time"]}';
    END IF;
    IF jsonb_typeof(p_filters) <> 'array' THEN
        RAISE EXCEPTION 'filters must be an array' USING ERRCODE = '22023';
    END IF;
    FOR f IN SELECT value FROM jsonb_array_elements(p_filters) LOOP
        IF NOT fields ? (f ->> 'field') THEN
            RAISE EXCEPTION '% cannot be filtered by %', p_type, f ->> 'field' USING ERRCODE = '22023';
        END IF;
        col := fields -> (f ->> 'field') ->> 0;
        kind := fields -> (f ->> 'field') ->> 1;
        cast_to := CASE kind WHEN 'time' THEN 'timestamptz' WHEN 'id' THEN 'uuid' ELSE 'text' END;
        op := CASE f ->> 'op' WHEN 'eq' THEN '=' WHEN 'ne' THEN 'IS DISTINCT FROM' WHEN 'lt' THEN '<' WHEN 'lte' THEN '<='
                              WHEN 'gt' THEN '>' WHEN 'gte' THEN '>=' WHEN 'in' THEN 'in' END;
        IF op IS NULL THEN
            RAISE EXCEPTION '% is not an operator', f ->> 'op' USING ERRCODE = '22023';
        ELSIF op = 'in' THEN
            conds := conds || format('%s = ANY (ARRAY(SELECT jsonb_array_elements_text($1 -> %s -> ''value''))::%s[])', col, i, cast_to);
        ELSE
            conds := conds || format('%s %s ($1 -> %s ->> ''value'')::%s', col, op, i, cast_to);
        END IF;
        i := i + 1;
    END LOOP;
    RETURN QUERY EXECUTE format('SELECT x.id FROM tasks x LEFT JOIN changes c ON c.task_id = x.id AND c.type = x.type
                                 WHERE x.type = %L AND %s ORDER BY x.created_at, x.id LIMIT %s',
                                p_type, array_to_string(conds, ' AND '), least(greatest(coalesce(p_limit, 100), 1), 1000))
        USING p_filters;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION app_find_records(text, jsonb, integer);
DROP TABLE workflow_schedules;
DROP TRIGGER workflow_executions_family ON workflow_executions;
DROP FUNCTION workflow_executions_family();
DROP INDEX workflow_executions_parent_idx;
ALTER TABLE workflow_executions DROP COLUMN depth;
ALTER TABLE workflow_executions DROP COLUMN parent_index;
ALTER TABLE workflow_executions DROP COLUMN parent_step;
ALTER TABLE workflow_executions DROP COLUMN parent_id;
