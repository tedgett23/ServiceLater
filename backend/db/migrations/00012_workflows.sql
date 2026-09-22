-- +goose Up

-- Workflow executions (docs/apps-platform.md §7). An execution is a position in a workflow graph plus its variables.
-- The engine moves it one step at a time, each step in its own transaction together with whatever the step did to
-- tickets, so a step's effect and the record that it happened commit together or not at all. Waiting is a row with a
-- wake-up time, not a goroutine.
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
    -- Steps run as this person, under their permissions and RLS, until service principals exist.
    started_by      uuid NOT NULL REFERENCES users(id),
    -- The chain of runs behind this one (loop protection): what its saves carry.
    cause           jsonb NOT NULL DEFAULT '{}',
    -- queued: a run job is on its way. running: a worker holds it. waiting: sleeping until wake_at.
    status          text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'waiting', 'done', 'failed', 'cancelled')),
    current_step    text NOT NULL,
    vars            jsonb NOT NULL DEFAULT '{}',
    steps_run       integer NOT NULL DEFAULT 0,
    -- Set while the current step is a wait that has begun. wait_until: the wait is on a condition, which is also
    -- re-checked whenever the record changes (the trigger below), not only at wake_at.
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
-- The timer's query: sleeping executions whose time has come.
CREATE INDEX workflow_executions_wake_idx ON workflow_executions (wake_at) WHERE status = 'waiting';
-- The record-change trigger's query.
CREATE INDEX workflow_executions_waiting_record_idx ON workflow_executions (record_id) WHERE status = 'waiting' AND wait_until;
CREATE INDEX workflow_executions_finished_idx ON workflow_executions (finished_at) WHERE finished_at IS NOT NULL;

-- One row for each time the engine visited a step: what it did and how it went.
CREATE TABLE workflow_steps (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    execution_id uuid NOT NULL REFERENCES workflow_executions(id) ON DELETE CASCADE,
    seq          integer NOT NULL,
    step_id      text NOT NULL,
    type         text NOT NULL,
    -- waiting: went to sleep here. timeout: a wait ran out. failed: the execution stopped here.
    outcome      text NOT NULL CHECK (outcome IN ('ok', 'waiting', 'timeout', 'failed')),
    detail       jsonb NOT NULL DEFAULT '{}',
    error        text NOT NULL DEFAULT '',
    duration_ms  integer NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (execution_id, seq)
);

-- The person a run belongs to sees it, staff see every run, and only the owner or staff can move it along.
ALTER TABLE workflow_executions ENABLE ROW LEVEL SECURITY;
CREATE POLICY workflow_executions_select ON workflow_executions FOR SELECT TO servicelater_app
    USING (app_is_staff() OR started_by = app_current_user_id());
CREATE POLICY workflow_executions_insert ON workflow_executions FOR INSERT TO servicelater_app
    WITH CHECK (started_by = app_current_user_id());
CREATE POLICY workflow_executions_update ON workflow_executions FOR UPDATE TO servicelater_app
    USING (started_by = app_current_user_id() OR app_is_staff())
    WITH CHECK (started_by = app_current_user_id() OR app_is_staff());

ALTER TABLE workflow_steps ENABLE ROW LEVEL SECURITY;
CREATE POLICY workflow_steps_select ON workflow_steps FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id));
CREATE POLICY workflow_steps_insert ON workflow_steps FOR INSERT TO servicelater_app
    WITH CHECK (EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = execution_id AND e.started_by = app_current_user_id()));

-- A condition wait is re-checked when the record changes, not only when its timer fires. Whoever changed the record
-- may not be allowed to see the execution, so this runs as its owner: it only brings the wake-up time forward.
-- +goose StatementBegin
CREATE FUNCTION workflow_wake_on_task_change() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE workflow_executions SET wake_at = now()
     WHERE record_id = NEW.id AND status = 'waiting' AND wait_until AND wake_at > now();
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER tasks_wake_workflows AFTER UPDATE ON tasks
    FOR EACH ROW EXECUTE FUNCTION workflow_wake_on_task_change();

-- +goose Down
DROP TRIGGER tasks_wake_workflows ON tasks;
DROP FUNCTION workflow_wake_on_task_change();
DROP TABLE workflow_steps;
DROP TABLE workflow_executions;
