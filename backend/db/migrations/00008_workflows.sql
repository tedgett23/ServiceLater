-- +goose Up
-- Workflows (docs/apps-platform.md §7): runs, the steps they took, approval requests, and schedules.

-- An execution is a position in a workflow graph plus its variables. The engine moves it one step at a time, each
-- step in its own transaction together with whatever the step did to records, so a step's effect and the record that
-- it happened commit together or not at all. Waiting is a row with a wake-up time, not a goroutine.
CREATE TABLE workflow_executions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    app             text NOT NULL,
    workflow        text NOT NULL,
    app_version     text NOT NULL,
    -- The workflow as it was when this execution started. Running executions stay on the version they began on,
    -- whatever is installed later, so upgrading an app never changes the meaning of a run in flight.
    definition      jsonb NOT NULL,
    -- No foreign key: the record may be gone by the time anyone looks, and the history outlives it.
    record_id       uuid,
    -- The person it runs for, and the app principal it runs as: steps act as the app, on behalf of the person.
    started_by      uuid NOT NULL REFERENCES users (id),
    run_as          uuid REFERENCES users (id),
    -- Subflows and foreach: a child run knows its parent, the step that started it and its place in a foreach; the
    -- rows are the truth the parent counts on every visit. depth is how many subflows deep it is (the engine stops
    -- at 5).
    parent_id       uuid REFERENCES workflow_executions (id),
    parent_step     text,
    parent_index    integer,
    depth           integer NOT NULL DEFAULT 0,
    -- The chain of runs behind this one (loop protection): what its saves carry.
    cause           jsonb NOT NULL DEFAULT '{}',
    -- queued: a run job is on its way. running: a worker holds it. waiting: sleeping until wake_at.
    status          text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'waiting', 'done', 'failed', 'cancelled')),
    current_step    text NOT NULL,
    vars            jsonb NOT NULL DEFAULT '{}',
    steps_run       integer NOT NULL DEFAULT 0,
    -- Set while the current step is a wait that has begun. wait_until: the wait is on a condition, which is also
    -- re-checked whenever the record changes (tasks_wake_workflows), not only at wake_at.
    wait_started_at timestamptz,
    wait_deadline   timestamptz,
    wait_until      boolean NOT NULL DEFAULT false,
    wake_at         timestamptz,
    error           text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);
CREATE INDEX workflow_executions_created_idx ON workflow_executions (created_at DESC);
CREATE INDEX workflow_executions_record_idx ON workflow_executions (record_id, created_at DESC);
CREATE INDEX workflow_executions_run_as_idx ON workflow_executions (run_as) WHERE run_as IS NOT NULL;
CREATE INDEX workflow_executions_parent_idx ON workflow_executions (parent_id, parent_step) WHERE parent_id IS NOT NULL;
-- The timer's query: sleeping executions whose time has come.
CREATE INDEX workflow_executions_wake_idx ON workflow_executions (wake_at) WHERE status = 'waiting';
-- The record-change trigger's query.
CREATE INDEX workflow_executions_waiting_record_idx ON workflow_executions (record_id) WHERE status = 'waiting' AND wait_until;
CREATE INDEX workflow_executions_finished_idx ON workflow_executions (finished_at) WHERE finished_at IS NOT NULL;
-- The builder's live view of a workflow (§7.7): its runs, and those in flight per step.
CREATE INDEX workflow_executions_workflow_idx ON workflow_executions (app, workflow, created_at DESC);
CREATE INDEX workflow_executions_in_flight_idx ON workflow_executions (app, workflow, current_step)
    WHERE status IN ('queued', 'running', 'waiting');

-- One row for each time the engine visited a step: what it did and how it went.
CREATE TABLE workflow_steps (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    execution_id uuid NOT NULL REFERENCES workflow_executions (id) ON DELETE CASCADE,
    seq          integer NOT NULL,
    step_id      text NOT NULL,
    type         text NOT NULL,
    -- waiting: went to sleep here. timeout: a wait ran out. failed: the step failed (an on_error exit may carry on).
    outcome      text NOT NULL CHECK (outcome IN ('ok', 'waiting', 'timeout', 'failed')),
    detail       jsonb NOT NULL DEFAULT '{}',
    error        text NOT NULL DEFAULT '',
    duration_ms  integer NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (execution_id, seq)
);

-- An approval step's requests, one row per approver it asked (§7.2), or per group: each group named in `groups` has one
-- vote, which any of its members may cast, so "the owners of every item this change touches must approve" asks each
-- owning team once. Whether the step is done, and which way, is computed from these rows and the step's rule (any,
-- all, majority); a workflow may ask more than once.
CREATE TABLE workflow_approvals (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    execution_id uuid NOT NULL REFERENCES workflow_executions (id) ON DELETE CASCADE,
    step_id      text NOT NULL,
    approver_id  uuid NOT NULL REFERENCES users (id),
    -- The group as it was asked (a name, not a reference: deleting or renaming the group later must not change a
    -- decision); NULL is a person asked in their own right. Someone in two of the groups is asked once per group.
    group_name   text,
    decision     text CHECK (decision IN ('approved', 'rejected')),
    comment      text NOT NULL DEFAULT '',
    decided_at   timestamptz,
    -- A request nobody needs to answer any more: another member of its group decided, or the step is over (an
    -- any-rule approval someone else settled, a timeout, a cancelled run). It leaves the inbox and cannot be decided.
    closed_at    timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX workflow_approvals_request_key ON workflow_approvals (execution_id, step_id, approver_id, group_name) NULLS NOT DISTINCT;
-- The approvals inbox: one person's open requests.
CREATE INDEX workflow_approvals_pending_idx ON workflow_approvals (approver_id) WHERE decision IS NULL AND closed_at IS NULL;
-- The engine reading a step's requests back, and the wake trigger finding the execution to nudge.
CREATE INDEX workflow_approvals_step_idx ON workflow_approvals (execution_id, step_id);

-- A run is seen by the person it runs for, the principal it runs as, workflows:read_all holders, and approvers with a
-- request on it (the inbox shows the record it is about). A plain EXISTS against workflow_approvals would recurse (that
-- table's policy looks back here), so app_has_approval_on reads past RLS instead.
-- +goose StatementBegin
CREATE FUNCTION app_has_approval_on(exec_id uuid) RETURNS boolean
    LANGUAGE sql SECURITY DEFINER STABLE SET search_path = public
    AS $$
    SELECT EXISTS (SELECT 1 FROM workflow_approvals WHERE execution_id = exec_id AND approver_id = app_current_user_id());
$$;
-- +goose StatementEnd


ALTER TABLE workflow_executions ENABLE ROW LEVEL SECURITY;
CREATE POLICY workflow_executions_select ON workflow_executions FOR SELECT TO servicelater_app
    USING (app_can('workflows:read_all') OR started_by = app_current_user_id() OR run_as = app_current_user_id()
           OR app_has_approval_on(id));
CREATE POLICY workflow_executions_insert ON workflow_executions FOR INSERT TO servicelater_app
    WITH CHECK (started_by = app_current_user_id()
                OR (run_as = app_current_user_id() AND started_by = app_on_behalf_of()));
CREATE POLICY workflow_executions_update ON workflow_executions FOR UPDATE TO servicelater_app
    USING (started_by = app_current_user_id() OR run_as = app_current_user_id() OR app_can('workflows:run'))
    WITH CHECK (started_by = app_current_user_id() OR run_as = app_current_user_id() OR app_can('workflows:run'));

ALTER TABLE workflow_steps ENABLE ROW LEVEL SECURITY;
CREATE POLICY workflow_steps_select ON workflow_steps FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id));
CREATE POLICY workflow_steps_insert ON workflow_steps FOR INSERT TO servicelater_app
    WITH CHECK (EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id
                        AND (e.started_by = app_current_user_id() OR e.run_as = app_current_user_id())));

-- Requests are visible to the approver they were sent to, to workflows:read_all, and to the run's person and principal
-- (the run viewer). Only the approver may decide, and only once: a decided row no longer matches the update policy.
ALTER TABLE workflow_approvals ENABLE ROW LEVEL SECURITY;
CREATE POLICY workflow_approvals_select ON workflow_approvals FOR SELECT TO servicelater_app
    USING (approver_id = app_current_user_id() OR app_can('workflows:read_all')
        OR EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id
                   AND (e.started_by = app_current_user_id() OR e.run_as = app_current_user_id())));
CREATE POLICY workflow_approvals_insert ON workflow_approvals FOR INSERT TO servicelater_app
    WITH CHECK (EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id
                        AND (e.started_by = app_current_user_id() OR e.run_as = app_current_user_id())));
CREATE POLICY workflow_approvals_update ON workflow_approvals FOR UPDATE TO servicelater_app
    USING (approver_id = app_current_user_id() AND decision IS NULL)
    WITH CHECK (approver_id = app_current_user_id());

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

-- A condition wait is re-checked when its record changes, not only when its timer fires, and a run waiting on a
-- request (until its tasks are done) when one of its tasks changes or is added. Whoever changed the record may not see
-- the execution, so this runs as the owner: it only brings the wake-up time forward.
-- +goose StatementBegin
CREATE FUNCTION workflow_wake_on_task_change() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE workflow_executions SET wake_at = now()
     WHERE record_id IN (NEW.id, NEW.parent_id) AND status = 'waiting' AND wait_until AND wake_at > now();
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER tasks_wake_workflows AFTER UPDATE ON tasks
    FOR EACH ROW EXECUTE FUNCTION workflow_wake_on_task_change();
CREATE TRIGGER tasks_wake_parent_workflows AFTER INSERT ON tasks
    FOR EACH ROW WHEN (NEW.parent_id IS NOT NULL) EXECUTE FUNCTION workflow_wake_on_task_change();

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
-- +goose StatementEnd

REVOKE ALL ON FUNCTION app_close_approvals(uuid, text, text) FROM PUBLIC;

-- Deciding a request wakes its waiting run at once (the timer, every second in the worker role, picks it up), clears
-- the approver's notification, and for a group closes the other members' requests.
-- +goose StatementBegin
CREATE FUNCTION workflow_wake_on_approval_decision() RETURNS trigger
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
-- +goose StatementEnd

CREATE TRIGGER workflow_approvals_wake AFTER UPDATE ON workflow_approvals
    FOR EACH ROW EXECUTE FUNCTION workflow_wake_on_approval_decision();

-- A step that is over leaves nobody waiting to be asked: when a run moves past an approval step, or ends.
-- +goose StatementBegin
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

CREATE TRIGGER workflow_close_left_approvals AFTER UPDATE ON workflow_executions
    FOR EACH ROW EXECUTE FUNCTION workflow_close_left_approvals();

-- The builder hears when a run starts, moves to another step or ends (ids and names only, like every other event).
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

-- Scheduled workflows: when each last fired, so every worker's timer can try and only one starts it. System only.
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
DROP TRIGGER tasks_wake_parent_workflows ON tasks;
DROP TRIGGER tasks_wake_workflows ON tasks;
DROP FUNCTION workflow_wake_on_task_change();
DROP FUNCTION app_approval_task_summary(uuid);
DROP TABLE workflow_approvals;
DROP TABLE workflow_steps;
DROP TABLE workflow_executions;
DROP FUNCTION app_has_approval_on(uuid);
DROP FUNCTION workflow_executions_family();
DROP FUNCTION workflow_executions_notify();
DROP FUNCTION workflow_close_left_approvals();
DROP FUNCTION workflow_wake_on_approval_decision();
DROP FUNCTION app_close_approvals(uuid, text, text);
