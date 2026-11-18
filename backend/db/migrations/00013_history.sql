-- +goose Up
-- What happened to a record or a configuration item, field by field (docs/apps-platform.md §9.4): who changed what,
-- from what to what, and when. It is written by triggers on the tables themselves, so every way of changing them is
-- recorded alike (a person, a rule, a workflow, the SLA timer, an import), and nothing can write or change it
-- otherwise: the app role may only read it, and only for what it may see. Values are kept as stored (ids, not names);
-- readers resolve them (internal/api/history.go).
CREATE TABLE record_history (
    id           bigserial PRIMARY KEY,
    -- setting: a change to the history settings themselves, so a gap can be told from a quiet record.
    subject      text NOT NULL CHECK (subject IN ('task', 'ci', 'setting')),
    subject_id   uuid NOT NULL,
    at           timestamptz NOT NULL DEFAULT now(),
    -- Who did it: the person, or an app's principal; NULL is the system (the SLA timer).
    actor_id     uuid,
    -- The person an app acted for, when it was an app.
    on_behalf_of uuid,
    action       text NOT NULL CHECK (action IN ('created', 'changed', 'linked', 'unlinked', 'deleted')),
    -- What changed: a column (status, priority...), an extra field (fields.category), or what was linked (ci, article,
    -- ticket, relationship, attachment).
    field        text NOT NULL DEFAULT '',
    old          jsonb,
    new          jsonb
);
CREATE INDEX record_history_subject_idx ON record_history (subject, subject_id, id);
ALTER TABLE record_history ENABLE ROW LEVEL SECURITY;
REVOKE INSERT, UPDATE, DELETE ON record_history FROM servicelater_app;
REVOKE USAGE ON SEQUENCE record_history_id_seq FROM servicelater_app;
CREATE POLICY record_history_select ON record_history FOR SELECT TO servicelater_app USING (
    (subject = 'task' AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = subject_id))
    OR (subject = 'ci' AND app_can('cmdb:read'))
    OR (subject = 'setting' AND app_can('settings:manage')));

-- Choosing what is recorded: the history.record_types and history.cmdb settings, and fields an app marks
-- history: false (kept here by the installer, from the app's forms and classes: kind is the record type or class,
-- field as record_history names it).
CREATE TABLE history_exclusions (
    app_name text NOT NULL REFERENCES apps (name) ON DELETE CASCADE,
    subject  text NOT NULL CHECK (subject IN ('task', 'ci')),
    kind     text NOT NULL,
    field    text NOT NULL,
    PRIMARY KEY (subject, kind, field, app_name)
);
ALTER TABLE history_exclusions ENABLE ROW LEVEL SECURITY;
CREATE POLICY history_exclusions_select ON history_exclusions FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
REVOKE INSERT, UPDATE, DELETE ON history_exclusions FROM servicelater_app;

-- Whether a change is recorded: always for the basics (a record's creation, status, assignee, assignment group and
-- files; an item's creation, deletion and status) and for settings; otherwise as the settings say, and not for a field an
-- app leaves out. kind is the record's type or the item's class; NULL (the row is gone) records.
-- +goose StatementBegin
CREATE FUNCTION app_history_on(p_subject text, p_kind text, p_field text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$
    SELECT CASE
        WHEN p_subject = 'setting' THEN true
        WHEN p_subject = 'task' AND p_field IN ('', 'status', 'assignee_id', 'assignment_group_id', 'attachment') THEN true
        WHEN p_subject = 'ci' AND p_field IN ('', 'status') THEN true
        WHEN p_kind IS NULL THEN true
        WHEN p_subject = 'ci' AND (SELECT value FROM settings WHERE name = 'history.cmdb') = 'false'::jsonb THEN false
        WHEN p_subject = 'task' AND NOT coalesce(
            (SELECT value ? '*' OR value ? p_kind FROM settings WHERE name = 'history.record_types'), true) THEN false
        ELSE NOT EXISTS (SELECT 1 FROM history_exclusions e WHERE e.subject = p_subject AND e.kind = p_kind AND e.field = p_field)
    END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION app_history_on(text, text, text) FROM PUBLIC;

-- Every entry goes through here, and is kept only when app_history_on says so.
-- +goose StatementBegin
CREATE FUNCTION app_history(p_subject text, p_id uuid, p_action text, p_field text, p_old jsonb, p_new jsonb) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    kind text;
BEGIN
    IF p_subject = 'task' THEN
        SELECT t.type INTO kind FROM tasks t WHERE t.id = p_id;
    ELSIF p_subject = 'ci' THEN
        SELECT c.class INTO kind FROM cis c WHERE c.id = p_id;
    END IF;
    IF app_history_on(p_subject, kind, p_field) THEN
        INSERT INTO record_history (subject, subject_id, actor_id, on_behalf_of, action, field, old, new)
        VALUES (p_subject, p_id, app_current_user_id(), app_on_behalf_of(), p_action, p_field, p_old, p_new);
    END IF;
END $$;
-- +goose StatementEnd

-- A change, when the value really changed.
-- +goose StatementBegin
CREATE FUNCTION app_history_change(p_subject text, p_id uuid, p_field text, p_old jsonb, p_new jsonb) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF p_old IS DISTINCT FROM p_new THEN
        PERFORM app_history(p_subject, p_id, 'changed', p_field, p_old, p_new);
    END IF;
END $$;
-- +goose StatementEnd

-- Each key of an extra-fields object that changed, as fields.<key>.
-- +goose StatementBegin
CREATE FUNCTION app_history_fields(p_subject text, p_id uuid, p_old jsonb, p_new jsonb) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    k text;
BEGIN
    FOR k IN SELECT jsonb_object_keys(coalesce(p_old, '{}') || coalesce(p_new, '{}')) ORDER BY 1 LOOP
        PERFORM app_history_change(p_subject, p_id, 'fields.' || k, p_old -> k, p_new -> k);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION tasks_history() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM app_history('task', NEW.id, 'created', '', NULL,
            jsonb_build_object('title', NEW.title, 'status', NEW.status, 'priority', NEW.priority, 'requester_id', NEW.requester_id));
        RETURN NULL;
    END IF;
    PERFORM app_history_change('task', NEW.id, 'title', to_jsonb(OLD.title), to_jsonb(NEW.title));
    PERFORM app_history_change('task', NEW.id, 'description', to_jsonb(OLD.description), to_jsonb(NEW.description));
    PERFORM app_history_change('task', NEW.id, 'status', to_jsonb(OLD.status), to_jsonb(NEW.status));
    PERFORM app_history_change('task', NEW.id, 'priority', to_jsonb(OLD.priority), to_jsonb(NEW.priority));
    PERFORM app_history_change('task', NEW.id, 'requester_id', to_jsonb(OLD.requester_id), to_jsonb(NEW.requester_id));
    PERFORM app_history_change('task', NEW.id, 'assignee_id', to_jsonb(OLD.assignee_id), to_jsonb(NEW.assignee_id));
    PERFORM app_history_change('task', NEW.id, 'assignment_group_id', to_jsonb(OLD.assignment_group_id), to_jsonb(NEW.assignment_group_id));
    PERFORM app_history_fields('task', NEW.id, OLD.fields, NEW.fields);
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION incidents_history() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM app_history_change('task', NEW.task_id, 'resolution', to_jsonb(OLD.resolution), to_jsonb(NEW.resolution));
    PERFORM app_history_change('task', NEW.task_id, 'problem_id', to_jsonb(OLD.problem_id), to_jsonb(NEW.problem_id));
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION changes_history() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM app_history_change('task', NEW.task_id, 'change_type', to_jsonb(OLD.change_type), to_jsonb(NEW.change_type));
    PERFORM app_history_change('task', NEW.task_id, 'risk', to_jsonb(OLD.risk), to_jsonb(NEW.risk));
    PERFORM app_history_change('task', NEW.task_id, 'impact', to_jsonb(OLD.impact), to_jsonb(NEW.impact));
    PERFORM app_history_change('task', NEW.task_id, 'planned_start', to_jsonb(OLD.planned_start), to_jsonb(NEW.planned_start));
    PERFORM app_history_change('task', NEW.task_id, 'planned_end', to_jsonb(OLD.planned_end), to_jsonb(NEW.planned_end));
    PERFORM app_history_change('task', NEW.task_id, 'implementation_plan', to_jsonb(OLD.implementation_plan), to_jsonb(NEW.implementation_plan));
    PERFORM app_history_change('task', NEW.task_id, 'backout_plan', to_jsonb(OLD.backout_plan), to_jsonb(NEW.backout_plan));
    PERFORM app_history_change('task', NEW.task_id, 'test_plan', to_jsonb(OLD.test_plan), to_jsonb(NEW.test_plan));
    PERFORM app_history_change('task', NEW.task_id, 'problem_id', to_jsonb(OLD.problem_id), to_jsonb(NEW.problem_id));
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- An item linked to a record is in both histories.
-- +goose StatementBegin
CREATE FUNCTION task_cis_history() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM app_history('task', NEW.task_id, 'linked', 'ci', NULL, jsonb_build_object('id', NEW.ci_id));
        PERFORM app_history('ci', NEW.ci_id, 'linked', 'ticket', NULL, jsonb_build_object('id', NEW.task_id));
    ELSE
        PERFORM app_history('task', OLD.task_id, 'unlinked', 'ci', jsonb_build_object('id', OLD.ci_id), NULL);
        PERFORM app_history('ci', OLD.ci_id, 'unlinked', 'ticket', jsonb_build_object('id', OLD.task_id), NULL);
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION task_articles_history() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM app_history('task', NEW.task_id, 'linked', 'article', NULL, jsonb_build_object('id', NEW.article_id));
    ELSE
        PERFORM app_history('task', OLD.task_id, 'unlinked', 'article', jsonb_build_object('id', OLD.article_id), NULL);
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cis_history() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM app_history('ci', NEW.id, 'created', '', NULL, jsonb_build_object('name', NEW.name, 'class', NEW.class, 'status', NEW.status));
        RETURN NULL;
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM app_history('ci', OLD.id, 'deleted', '', jsonb_build_object('name', OLD.name, 'class', OLD.class), NULL);
        RETURN NULL;
    END IF;
    PERFORM app_history_change('ci', NEW.id, 'class', to_jsonb(OLD.class), to_jsonb(NEW.class));
    PERFORM app_history_change('ci', NEW.id, 'name', to_jsonb(OLD.name), to_jsonb(NEW.name));
    PERFORM app_history_change('ci', NEW.id, 'status', to_jsonb(OLD.status), to_jsonb(NEW.status));
    PERFORM app_history_change('ci', NEW.id, 'description', to_jsonb(OLD.description), to_jsonb(NEW.description));
    PERFORM app_history_change('ci', NEW.id, 'owner_group_id', to_jsonb(OLD.owner_group_id), to_jsonb(NEW.owner_group_id));
    PERFORM app_history_change('ci', NEW.id, 'support_group_id', to_jsonb(OLD.support_group_id), to_jsonb(NEW.support_group_id));
    PERFORM app_history_fields('ci', NEW.id, OLD.fields, NEW.fields);
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- A relationship is in the history of both items, read from each: {type, to} from the first, {type, from} from the
-- second.
-- +goose StatementBegin
CREATE FUNCTION ci_relationships_history() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    r ci_relationships;
    act text;
BEGIN
    IF TG_OP = 'INSERT' THEN r := NEW; act := 'linked'; ELSE r := OLD; act := 'unlinked'; END IF;
    PERFORM app_history('ci', r.from_ci, act, 'relationship',
        CASE WHEN act = 'unlinked' THEN jsonb_build_object('type', r.type, 'to', r.to_ci) END,
        CASE WHEN act = 'linked' THEN jsonb_build_object('type', r.type, 'to', r.to_ci) END);
    PERFORM app_history('ci', r.to_ci, act, 'relationship',
        CASE WHEN act = 'unlinked' THEN jsonb_build_object('type', r.type, 'from', r.from_ci) END,
        CASE WHEN act = 'linked' THEN jsonb_build_object('type', r.type, 'from', r.from_ci) END);
    RETURN NULL;
END $$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION app_history(text, uuid, text, text, jsonb, jsonb), app_history_change(text, uuid, text, jsonb, jsonb),
    app_history_fields(text, uuid, jsonb, jsonb) FROM PUBLIC;

CREATE TRIGGER tasks_history AFTER INSERT OR UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION tasks_history();
CREATE TRIGGER incidents_history AFTER UPDATE ON incidents FOR EACH ROW EXECUTE FUNCTION incidents_history();
CREATE TRIGGER changes_history AFTER UPDATE ON changes FOR EACH ROW EXECUTE FUNCTION changes_history();
CREATE TRIGGER task_cis_history AFTER INSERT OR DELETE ON task_cis FOR EACH ROW EXECUTE FUNCTION task_cis_history();
CREATE TRIGGER task_articles_history AFTER INSERT OR DELETE ON task_articles FOR EACH ROW EXECUTE FUNCTION task_articles_history();
CREATE TRIGGER cis_history AFTER INSERT OR UPDATE OR DELETE ON cis FOR EACH ROW EXECUTE FUNCTION cis_history();
CREATE TRIGGER ci_relationships_history AFTER INSERT OR DELETE ON ci_relationships FOR EACH ROW EXECUTE FUNCTION ci_relationships_history();

-- Changes to the history settings.
-- +goose StatementBegin
CREATE FUNCTION settings_history() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF coalesce(NEW.name, OLD.name) LIKE 'history.%' THEN
        PERFORM app_history_change('setting', '00000000-0000-0000-0000-000000000000', coalesce(NEW.name, OLD.name),
            CASE WHEN TG_OP = 'INSERT' THEN NULL ELSE OLD.value END, CASE WHEN TG_OP = 'DELETE' THEN NULL ELSE NEW.value END);
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER settings_history AFTER INSERT OR UPDATE OR DELETE ON settings FOR EACH ROW EXECUTE FUNCTION settings_history();

-- A clock that switches calendar (a P1, a hand-off to a team with other hours) says so in its record's history, with
-- the SLA's and the calendars' names as they were then. SECURITY DEFINER like every history writer.
-- +goose StatementBegin
CREATE FUNCTION task_slas_calendar_history() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    sla text := coalesce(NEW.definition ->> 'label', NEW.sla);
BEGIN
    PERFORM app_history('task', NEW.task_id, 'changed', 'sla_calendar',
        jsonb_build_object('sla', sla, 'calendar', OLD.calendar,
            'label', (SELECT label FROM business_calendars WHERE name = OLD.calendar)),
        jsonb_build_object('sla', sla, 'calendar', NEW.calendar,
            'label', (SELECT label FROM business_calendars WHERE name = NEW.calendar)));
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER task_slas_calendar_history AFTER UPDATE OF calendar ON task_slas
    FOR EACH ROW WHEN (OLD.calendar IS DISTINCT FROM NEW.calendar) EXECUTE FUNCTION task_slas_calendar_history();

-- +goose Down
DROP TRIGGER task_slas_calendar_history ON task_slas;
DROP FUNCTION task_slas_calendar_history();
DROP TRIGGER settings_history ON settings;
DROP FUNCTION settings_history();
DROP TRIGGER ci_relationships_history ON ci_relationships;
DROP TRIGGER cis_history ON cis;
DROP TRIGGER task_articles_history ON task_articles;
DROP TRIGGER task_cis_history ON task_cis;
DROP TRIGGER changes_history ON changes;
DROP TRIGGER incidents_history ON incidents;
DROP TRIGGER tasks_history ON tasks;
DROP FUNCTION ci_relationships_history();
DROP FUNCTION cis_history();
DROP FUNCTION task_articles_history();
DROP FUNCTION task_cis_history();
DROP FUNCTION changes_history();
DROP FUNCTION incidents_history();
DROP FUNCTION tasks_history();
DROP FUNCTION app_history_fields(text, uuid, jsonb, jsonb);
DROP FUNCTION app_history_change(text, uuid, text, jsonb, jsonb);
DROP FUNCTION app_history(text, uuid, text, text, jsonb, jsonb);
DROP FUNCTION app_history_on(text, text, text);
DROP TABLE history_exclusions;
DROP TABLE record_history;
