-- +goose Up
-- Change freezes (scoped to items or not), the lookups rules and guards count them with, and SLA clocks.

-- Change freeze windows: periods when changes may not be scheduled (year end, a launch). They are reference data
-- people edit at run time, which an app cannot define a table for yet (docs/apps-platform.md, open question 11), so
-- the table is core. What a freeze means is app content: change-management's rules and guards count them through a
-- lookup (app_lookup_count below).
CREATE TABLE freeze_windows (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    reason     text NOT NULL DEFAULT '' CHECK (length(reason) <= 2000),
    starts_at  timestamptz NOT NULL,
    ends_at    timestamptz NOT NULL,
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT freeze_windows_window_check CHECK (ends_at > starts_at)
);
CREATE INDEX freeze_windows_window_idx ON freeze_windows (starts_at, ends_at);

-- Everyone signed in reads them (a requester filing a change needs to know); freezes:manage writes them.
ALTER TABLE freeze_windows ENABLE ROW LEVEL SECURITY;
CREATE POLICY freeze_windows_select ON freeze_windows FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
CREATE POLICY freeze_windows_insert ON freeze_windows FOR INSERT TO servicelater_app WITH CHECK (app_can('freezes:manage'));
CREATE POLICY freeze_windows_update ON freeze_windows FOR UPDATE TO servicelater_app
    USING (app_can('freezes:manage')) WITH CHECK (app_can('freezes:manage'));
CREATE POLICY freeze_windows_delete ON freeze_windows FOR DELETE TO servicelater_app USING (app_can('freezes:manage'));

-- Open calendars refetch when a freeze changes. Everyone may see freezes, so the event goes to everyone.
-- +goose StatementBegin
CREATE FUNCTION freeze_windows_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('freeze_changes', json_build_object('id', coalesce(NEW.id, OLD.id))::text);
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER freeze_windows_notify AFTER INSERT OR UPDATE OR DELETE ON freeze_windows
    FOR EACH ROW EXECUTE FUNCTION freeze_windows_notify();

-- Change freezes scoped to configuration items (docs/apps-platform.md §8.2): a freeze that lists items applies only to
-- records linked to one of them; one that lists none applies to everything, as before. Lookups filter freezes by the
-- items a record is linked to (applies_to); what a freeze stops is still app content.
CREATE TABLE freeze_window_cis (
    freeze_id uuid NOT NULL REFERENCES freeze_windows (id) ON DELETE CASCADE,
    ci_id     uuid NOT NULL REFERENCES cis (id) ON DELETE CASCADE,
    PRIMARY KEY (freeze_id, ci_id)
);
CREATE INDEX freeze_window_cis_ci_idx ON freeze_window_cis (ci_id);

-- Everyone signed in sees which item ids a freeze covers, as they see freezes (a rule counting freezes runs as whoever
-- saves); the items' names stay behind cmdb:read. freezes:manage changes them.
ALTER TABLE freeze_window_cis ENABLE ROW LEVEL SECURITY;
CREATE POLICY freeze_window_cis_select ON freeze_window_cis FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
CREATE POLICY freeze_window_cis_insert ON freeze_window_cis FOR INSERT TO servicelater_app WITH CHECK (app_can('freezes:manage'));
CREATE POLICY freeze_window_cis_delete ON freeze_window_cis FOR DELETE TO servicelater_app USING (app_can('freezes:manage'));

-- A rule's or a guard's lookup (docs/apps-platform.md §5): how many rows of one kind match every filter, counted up to
-- 1000. It runs as the caller (no SECURITY DEFINER), so RLS decides what is counted. filters is a JSON array of
-- {field, op, value}; the fields and operators are a fixed menu (apps.LookupTargets mirrors it), every value is a
-- bound parameter, and anything else is an error, never a guess.
-- +goose StatementBegin
CREATE FUNCTION app_lookup_count(p_target text, p_filters jsonb) RETURNS bigint
    LANGUAGE plpgsql STABLE SET search_path = public
    AS $$
DECLARE
    src    text;
    fields jsonb;
    conds  text[] := ARRAY['true'];
    f      jsonb;
    i      int := 0;
    col    text;
    kind   text;
    cast_to text;
    op     text;
    n      bigint;
BEGIN
    IF p_target = 'freeze_window' THEN
        src := 'freeze_windows x';
        fields := '{"starts_at": ["x.starts_at", "time"], "ends_at": ["x.ends_at", "time"], "applies_to": ["x.id", "scope"]}';
    ELSIF p_target <> 'task' AND EXISTS (SELECT 1 FROM task_types WHERE type = p_target) THEN
        src := format('tasks x LEFT JOIN changes c ON c.task_id = x.id AND c.type = x.type WHERE x.type = %L', p_target);
        fields := '{"id": ["x.id", "id"], "status": ["x.status", "text"], "state_category": ["x.state_category", "text"],
                    "requester_id": ["x.requester_id", "id"], "assignee_id": ["x.assignee_id", "id"],
                    "assignment_group_id": ["x.assignment_group_id", "id"]}';
        IF p_target = 'change' THEN
            fields := fields || '{"change_type": ["c.change_type", "text"], "planned_start": ["c.planned_start", "time"],
                                  "planned_end": ["c.planned_end", "time"]}';
        END IF;
    ELSE
        RAISE EXCEPTION 'a lookup cannot count %', p_target USING ERRCODE = '22023';
    END IF;
    IF jsonb_typeof(p_filters) <> 'array' THEN
        RAISE EXCEPTION 'lookup filters must be an array' USING ERRCODE = '22023';
    END IF;
    FOR f IN SELECT value FROM jsonb_array_elements(p_filters) LOOP
        IF NOT fields ? (f ->> 'field') THEN
            RAISE EXCEPTION '% cannot be filtered by %', p_target, f ->> 'field' USING ERRCODE = '22023';
        END IF;
        col := fields -> (f ->> 'field') ->> 0;
        kind := fields -> (f ->> 'field') ->> 1;
        -- A freeze applies to an item when it lists no items (it applies to everything) or lists that one.
        IF kind = 'scope' THEN
            IF f ->> 'op' <> 'in' THEN
                RAISE EXCEPTION '% is filtered with in, a list of item ids', f ->> 'field' USING ERRCODE = '22023';
            END IF;
            conds := conds || format('(NOT EXISTS (SELECT 1 FROM freeze_window_cis s WHERE s.freeze_id = %s) OR EXISTS (SELECT 1 FROM freeze_window_cis s WHERE s.freeze_id = %s AND s.ci_id = ANY (ARRAY(SELECT jsonb_array_elements_text($1 -> %s -> ''value''))::uuid[])))', col, col, i);
            i := i + 1;
            CONTINUE;
        END IF;
        cast_to := CASE kind WHEN 'time' THEN 'timestamptz' WHEN 'id' THEN 'uuid' ELSE 'text' END;
        op := CASE f ->> 'op' WHEN 'eq' THEN '=' WHEN 'ne' THEN 'IS DISTINCT FROM' WHEN 'lt' THEN '<' WHEN 'lte' THEN '<='
                              WHEN 'gt' THEN '>' WHEN 'gte' THEN '>=' WHEN 'in' THEN 'in' END;
        IF op IS NULL THEN
            RAISE EXCEPTION '% is not a lookup operator', f ->> 'op' USING ERRCODE = '22023';
        ELSIF op = 'in' THEN
            conds := conds || format('%s = ANY (ARRAY(SELECT jsonb_array_elements_text($1 -> %s -> ''value''))::%s[])', col, i, cast_to);
        ELSE
            conds := conds || format('%s %s ($1 -> %s ->> ''value'')::%s', col, op, i, cast_to);
        END IF;
        i := i + 1;
    END LOOP;
    EXECUTE format('SELECT count(*) FROM (SELECT 1 FROM %s %s %s LIMIT 1000) found',
                   src, CASE WHEN p_target = 'freeze_window' THEN 'WHERE' ELSE 'AND' END, array_to_string(conds, ' AND '))
        INTO n USING p_filters;
    RETURN n;
END
$$;
-- +goose StatementEnd

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
    -- The business calendar it counts by now ('' counts every hour). An SLA may choose its calendar by the record
    -- (priority, the assignment group's own hours, any condition), and the choice is made again on every save, so a
    -- hand-off to a team with other hours counts from then on in theirs (the time used so far is kept).
    calendar        text NOT NULL DEFAULT '',
    started_at      timestamptz NOT NULL DEFAULT now(),
    stopped_at      timestamptz,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (task_id, app, sla),
    CONSTRAINT task_slas_running_check CHECK ((stage = 'running') = (running_since IS NOT NULL))
);
CREATE INDEX task_slas_alert_idx ON task_slas (next_alert_at) WHERE stage = 'running' AND next_alert_at IS NOT NULL;
-- The queue's SLA column (each record's soonest running clock), and the report's: ended, breached, falling due.
CREATE INDEX task_slas_due_idx ON task_slas (task_id, due_at) WHERE stage = 'running';
CREATE INDEX task_slas_running_due_idx ON task_slas (due_at) WHERE stage = 'running';
CREATE INDEX task_slas_stopped_idx ON task_slas (stopped_at) WHERE stopped_at IS NOT NULL;
CREATE INDEX task_slas_breached_idx ON task_slas (breached_at) WHERE breached_at IS NOT NULL;

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

-- A breach found by the timer is a change to the record people should see at once: tell its watchers. Changes made by
-- a save need no event of their own, since the save's change to the record sends one.
-- +goose StatementBegin
CREATE FUNCTION task_slas_notify() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM task_notify(NEW.task_id);
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
    WHERE stage = 'running' AND calendar IN (OLD.name, coalesce(NEW.name, OLD.name));
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
    UPDATE task_slas SET next_alert_at = now() WHERE stage = 'running' AND calendar = NEW.name;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER business_calendars_recheck_new AFTER INSERT ON business_calendars
    FOR EACH ROW EXECUTE FUNCTION business_calendars_recheck_new();

-- +goose Down
DROP TRIGGER business_calendars_recheck_new ON business_calendars;
DROP FUNCTION business_calendars_recheck_new();
DROP TRIGGER business_calendars_recheck ON business_calendars;
DROP FUNCTION business_calendars_recheck();
DROP TABLE task_slas;
DROP FUNCTION task_slas_notify();
DROP FUNCTION app_lookup_count(text, jsonb);
DROP TABLE freeze_window_cis;
DROP TABLE freeze_windows;
DROP FUNCTION freeze_windows_notify();
