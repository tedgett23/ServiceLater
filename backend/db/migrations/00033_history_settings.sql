-- +goose Up

-- Choosing what is recorded field by field (docs/apps-platform.md §9.4): the history.record_types and history.cmdb
-- settings, and fields an app marks history: false (kept here by the installer, from the app's forms and classes:
-- kind is the record type or class, field as record_history names it). The basics are always recorded.
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

-- Changes to these settings are history too, so a gap can be told from a quiet record.
ALTER TABLE record_history DROP CONSTRAINT record_history_subject_check;
ALTER TABLE record_history ADD CONSTRAINT record_history_subject_check CHECK (subject IN ('task', 'ci', 'setting'));
ALTER POLICY record_history_select ON record_history USING (
    (subject = 'task' AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = subject_id))
    OR (subject = 'ci' AND app_can('cmdb:read'))
    OR (subject = 'setting' AND app_can('settings:manage')));

-- Whether a change is recorded: always for the basics (a record's creation, status, assignee and assignment group;
-- an item's creation, deletion and status) and for settings; otherwise as the settings say, and not for a field an
-- app leaves out. kind is the record's type or the item's class; NULL (the row is gone) records.
-- +goose StatementBegin
CREATE FUNCTION app_history_on(p_subject text, p_kind text, p_field text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$
    SELECT CASE
        WHEN p_subject = 'setting' THEN true
        WHEN p_subject = 'task' AND p_field IN ('', 'status', 'assignee_id', 'assignment_group_id') THEN true
        WHEN p_subject = 'ci' AND p_field IN ('', 'status') THEN true
        WHEN p_kind IS NULL THEN true
        WHEN p_subject = 'ci' AND (SELECT value FROM settings WHERE name = 'history.cmdb') = 'false'::jsonb THEN false
        WHEN p_subject = 'task' AND NOT coalesce(
            (SELECT value ? '*' OR value ? p_kind FROM settings WHERE name = 'history.record_types'), true) THEN false
        ELSE NOT EXISTS (SELECT 1 FROM history_exclusions e WHERE e.subject = p_subject AND e.kind = p_kind AND e.field = p_field)
    END
$$;

-- Every entry goes through here: the triggers are unchanged, this decides.
CREATE OR REPLACE FUNCTION app_history(p_subject text, p_id uuid, p_action text, p_field text, p_old jsonb, p_new jsonb) RETURNS void
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
REVOKE ALL ON FUNCTION app_history_on(text, text, text) FROM PUBLIC;
CREATE TRIGGER settings_history AFTER INSERT OR UPDATE OR DELETE ON settings FOR EACH ROW EXECUTE FUNCTION settings_history();

-- +goose Down
DROP TRIGGER settings_history ON settings;
DROP FUNCTION settings_history();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_history(p_subject text, p_id uuid, p_action text, p_field text, p_old jsonb, p_new jsonb) RETURNS void
    LANGUAGE sql SECURITY DEFINER SET search_path = public
    AS $$
    INSERT INTO record_history (subject, subject_id, actor_id, on_behalf_of, action, field, old, new)
    VALUES (p_subject, p_id, app_current_user_id(), app_on_behalf_of(), p_action, p_field, p_old, p_new);
$$;
-- +goose StatementEnd
DROP FUNCTION app_history_on(text, text, text);
DELETE FROM record_history WHERE subject = 'setting';
ALTER POLICY record_history_select ON record_history USING (
    (subject = 'task' AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = subject_id))
    OR (subject = 'ci' AND app_can('cmdb:read')));
ALTER TABLE record_history DROP CONSTRAINT record_history_subject_check;
ALTER TABLE record_history ADD CONSTRAINT record_history_subject_check CHECK (subject IN ('task', 'ci'));
DROP TABLE history_exclusions;
