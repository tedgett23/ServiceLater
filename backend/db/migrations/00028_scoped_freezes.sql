-- +goose Up

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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_lookup_count(p_target text, p_filters jsonb) RETURNS bigint
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

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_lookup_count(p_target text, p_filters jsonb) RETURNS bigint
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
DROP TABLE freeze_window_cis;
