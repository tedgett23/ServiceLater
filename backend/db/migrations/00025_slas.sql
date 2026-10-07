-- +goose Up

-- Business calendars: the working hours an SLA counts, in a time zone, less holidays. Like freeze windows they are
-- reference data people edit at run time (holidays change every year), which an app cannot define a table for yet
-- (docs/apps-platform.md, open question 11), so the table is core. An SLA definition names a calendar; when the name
-- is missing the clock counts every hour and the problem is logged.
CREATE TABLE business_calendars (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- What an SLA definition writes after calendar: (lowercase letters, digits and hyphens).
    name       text NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9-]{1,62}$'),
    label      text NOT NULL CHECK (length(btrim(label)) BETWEEN 1 AND 200),
    -- An IANA zone (Europe/London); internal/api checks it, since Postgres has no immutable test for one.
    time_zone  text NOT NULL,
    -- The working week: [{"day": 1, "start": "09:00", "end": "17:00"}, ...], day 0 = Sunday.
    hours      jsonb NOT NULL CHECK (jsonb_typeof(hours) = 'array' AND jsonb_array_length(hours) BETWEEN 1 AND 70),
    holidays   date[] NOT NULL DEFAULT '{}' CHECK (cardinality(holidays) <= 1000),
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Everyone signed in reads them (a save as anyone, even an end user filing an incident, starts a clock that needs its
-- calendar); calendars:manage writes them.
ALTER TABLE business_calendars ENABLE ROW LEVEL SECURITY;
CREATE POLICY business_calendars_select ON business_calendars FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
CREATE POLICY business_calendars_insert ON business_calendars FOR INSERT TO servicelater_app WITH CHECK (app_can('calendars:manage'));
CREATE POLICY business_calendars_update ON business_calendars FOR UPDATE TO servicelater_app
    USING (app_can('calendars:manage')) WITH CHECK (app_can('calendars:manage'));
CREATE POLICY business_calendars_delete ON business_calendars FOR DELETE TO servicelater_app USING (app_can('calendars:manage'));

-- The calendar incident-management's SLAs name: Monday to Friday, 09:00 to 17:00 UTC. Operators change its hours,
-- zone and holidays to match their own; this never overwrites them. Tests call it again after wiping users, which
-- takes the calendars with them.
-- +goose StatementBegin
CREATE FUNCTION app_seed_business_calendars() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO business_calendars (name, label, time_zone, hours) VALUES
        ('business-hours', 'Business hours', 'UTC',
         '[{"day":1,"start":"09:00","end":"17:00"},{"day":2,"start":"09:00","end":"17:00"},{"day":3,"start":"09:00","end":"17:00"},
           {"day":4,"start":"09:00","end":"17:00"},{"day":5,"start":"09:00","end":"17:00"}]')
    ON CONFLICT (name) DO NOTHING;
$$;
-- +goose StatementEnd
SELECT app_seed_business_calendars();

-- SLA clocks: one per record per SLA definition that started on it (docs/apps-platform.md §6.6). Only the save path
-- (internal/records) and the SLA timer (internal/slatimer) write them.
CREATE TABLE task_slas (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id         uuid NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    app             text NOT NULL,
    sla             text NOT NULL,
    -- The definition the clock started with, so changing an SLA does not change the clocks already running under it
    -- (as a workflow execution keeps its definition). checksum names it, for the compiled-definition cache.
    definition      jsonb NOT NULL,
    checksum        text NOT NULL,
    -- running: counting. paused: its pause condition holds. stopped: its stop condition holds (met, or breached if
    -- breached_at is set); a stopped clock runs again if the record is reopened. cancelled: the app no longer defines it.
    stage           text NOT NULL CHECK (stage IN ('running', 'paused', 'stopped', 'cancelled')),
    target_ms       bigint NOT NULL CHECK (target_ms > 0),
    -- Working time used before running_since. While running, the time used is this plus the working time since then.
    elapsed_ms      bigint NOT NULL DEFAULT 0 CHECK (elapsed_ms >= 0),
    running_since   timestamptz,
    -- When the target is used up at the current rate: set while running, for the queue's sort and the ticket's display.
    due_at          timestamptz,
    breached_at     timestamptz,
    -- How many of the definition's warnings have been sent, lowest threshold first.
    warnings_sent   integer NOT NULL DEFAULT 0,
    -- When the timer next looks at this clock: the next warning or the breach. Null when there is nothing left to send.
    next_alert_at   timestamptz,
    -- Why the clock could not be worked out on the last save (an expression failed, the calendar is missing), or ''.
    problem         text NOT NULL DEFAULT '',
    started_at      timestamptz NOT NULL DEFAULT now(),
    stopped_at      timestamptz,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (task_id, app, sla),
    CONSTRAINT task_slas_running_check CHECK ((stage = 'running') = (running_since IS NOT NULL))
);
CREATE INDEX task_slas_alert_idx ON task_slas (next_alert_at) WHERE stage = 'running' AND next_alert_at IS NOT NULL;
-- The queue's SLA column: each task's soonest running clock.
CREATE INDEX task_slas_due_idx ON task_slas (task_id, due_at) WHERE stage = 'running';

-- Visible, and written by a save, exactly when its record is visible to whoever is saving: the clock is part of the
-- record. Nobody deletes one; the record's deletion takes it.
ALTER TABLE task_slas ENABLE ROW LEVEL SECURITY;
CREATE POLICY task_slas_select ON task_slas FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));
CREATE POLICY task_slas_insert ON task_slas FOR INSERT TO servicelater_app
    WITH CHECK (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));
CREATE POLICY task_slas_update ON task_slas FOR UPDATE TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id))
    WITH CHECK (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));

-- A breach found by the timer is a change to the ticket people should see at once: tell its watchers through the
-- ticket channel, with the same routing fields the task trigger sends. Changes made by a save need no event of their
-- own, since the save's change to the task sends one.
-- +goose StatementBegin
CREATE FUNCTION task_slas_notify() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM pg_notify('ticket_changes', json_build_object(
        'op', 'UPDATE', 'id', t.id, 'type', t.type, 'number', t.number, 'status', t.status,
        'requester_id', t.requester_id, 'assignee_id', t.assignee_id)::text)
    FROM tasks t WHERE t.id = NEW.task_id;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER task_slas_notify AFTER UPDATE ON task_slas
    FOR EACH ROW WHEN (OLD.breached_at IS DISTINCT FROM NEW.breached_at) EXECUTE FUNCTION task_slas_notify();

-- Changing or deleting a calendar moves the due times of the clocks counting by it. Their due times are worked out in
-- Go, so the timer is asked to look at each of them now; it works them out again with the calendar as it is.
-- SECURITY DEFINER: whoever manages calendars may not see every record.
-- +goose StatementBegin
CREATE FUNCTION business_calendars_recheck() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE task_slas SET next_alert_at = now()
    WHERE stage = 'running' AND definition ->> 'calendar' IN (OLD.name, coalesce(NEW.name, OLD.name));
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER business_calendars_recheck AFTER UPDATE OR DELETE ON business_calendars
    FOR EACH ROW EXECUTE FUNCTION business_calendars_recheck();
-- A new calendar may be one that running clocks name and found missing.
-- +goose StatementBegin
CREATE FUNCTION business_calendars_recheck_new() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE task_slas SET next_alert_at = now() WHERE stage = 'running' AND definition ->> 'calendar' = NEW.name;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER business_calendars_recheck_new AFTER INSERT ON business_calendars
    FOR EACH ROW EXECUTE FUNCTION business_calendars_recheck_new();

-- What an SLA's warning and breach actions did is audited with the rules'.
ALTER TABLE action_runs DROP CONSTRAINT action_runs_timing_check;
ALTER TABLE action_runs ADD CONSTRAINT action_runs_timing_check CHECK (timing IN ('before', 'after', 'async', 'sla'));

-- Admins manage calendars.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,comments:delete,comments:internal,freezes:manage,groups:manage,records:create:*,records:delete:*,records:read:*,records:update:*,roles:manage,users:manage,users:read,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{comments:internal,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{comments:delete,comments:internal,freezes:manage,groups:manage,records:create:*,records:delete:*,records:read:*,records:update:*,roles:manage,users:manage,users:read,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{comments:internal,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();
DELETE FROM action_runs WHERE timing = 'sla';
ALTER TABLE action_runs DROP CONSTRAINT action_runs_timing_check;
ALTER TABLE action_runs ADD CONSTRAINT action_runs_timing_check CHECK (timing IN ('before', 'after', 'async'));
DROP TRIGGER business_calendars_recheck_new ON business_calendars;
DROP FUNCTION business_calendars_recheck_new();
DROP TRIGGER business_calendars_recheck ON business_calendars;
DROP FUNCTION business_calendars_recheck();
DROP TRIGGER task_slas_notify ON task_slas;
DROP FUNCTION task_slas_notify();
DROP TABLE task_slas;
DROP FUNCTION app_seed_business_calendars();
DROP TABLE business_calendars;
