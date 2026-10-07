-- +goose Up

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

-- Admins manage freezes; change-management's change-manager role grants it too.
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
        fields := '{"starts_at": ["x.starts_at", "time"], "ends_at": ["x.ends_at", "time"]}';
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

-- +goose Down
DROP FUNCTION app_lookup_count(text, jsonb);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{comments:delete,comments:internal,groups:manage,records:create:*,records:delete:*,records:read:*,records:update:*,roles:manage,users:manage,users:read,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{comments:internal,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();
DROP TRIGGER freeze_windows_notify ON freeze_windows;
DROP FUNCTION freeze_windows_notify();
DROP TABLE freeze_windows;
