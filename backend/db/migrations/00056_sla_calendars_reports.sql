-- +goose Up
-- SLA calendars by rule, and SLA reports (docs/apps-platform.md §6.6).
--
-- A clock now records the calendar it counts by: an SLA may choose its calendar by the record (priority, the
-- assignment group, any condition) and the choice is made again on every save, so a hand-off to a team with other
-- hours counts from then on in theirs. Existing clocks count by the calendar their definition names.
ALTER TABLE task_slas ADD COLUMN calendar text NOT NULL DEFAULT '';
UPDATE task_slas SET calendar = coalesce(definition ->> 'calendar', '');

-- A group's own working hours: what an SLA's "the assignment group's calendar" counts. A calendar a group uses
-- cannot be deleted.
ALTER TABLE groups ADD COLUMN calendar_id uuid REFERENCES business_calendars (id);

-- Calendars find their clocks by the clock's own calendar now.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION business_calendars_recheck() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE task_slas SET next_alert_at = now()
    WHERE stage = 'running' AND calendar IN (OLD.name, coalesce(NEW.name, OLD.name));
    RETURN NULL;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION business_calendars_recheck_new() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE task_slas SET next_alert_at = now() WHERE stage = 'running' AND calendar = NEW.name;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- Extended hours (12x5) ships alongside business hours (8x5); every hour (24x7) is no calendar at all.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_business_calendars() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO business_calendars (name, label, time_zone, hours) VALUES
        ('business-hours', 'Business hours', 'UTC',
         '[{"day":1,"start":"09:00","end":"17:00"},{"day":2,"start":"09:00","end":"17:00"},{"day":3,"start":"09:00","end":"17:00"},
           {"day":4,"start":"09:00","end":"17:00"},{"day":5,"start":"09:00","end":"17:00"}]'),
        ('extended-hours', 'Extended hours', 'UTC',
         '[{"day":1,"start":"07:00","end":"19:00"},{"day":2,"start":"07:00","end":"19:00"},{"day":3,"start":"07:00","end":"19:00"},
           {"day":4,"start":"07:00","end":"19:00"},{"day":5,"start":"07:00","end":"19:00"}]')
    ON CONFLICT (name) DO NOTHING;
$$;
-- +goose StatementEnd
SELECT app_seed_business_calendars();

-- Reports read clocks by when they ended, breached and fell due.
CREATE INDEX task_slas_stopped_idx ON task_slas (stopped_at) WHERE stopped_at IS NOT NULL;
CREATE INDEX task_slas_breached_idx ON task_slas (breached_at) WHERE breached_at IS NOT NULL;
CREATE INDEX task_slas_running_due_idx ON task_slas (due_at) WHERE stage = 'running';

-- reports:read: the SLA report (agents and admins); slas:manage: which hours each SLA counts (admins).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,catalog:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,forms:manage,freezes:manage,groups:manage,knowledge:publish,knowledge:read_internal,knowledge:write,records:create:*,records:delete:*,records:read:*,records:update:*,reports:read,restricted:read,roles:manage,scripts:edit,slas:manage,settings:manage,users:manage,users:read,workflows:edit,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,reports:read,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();

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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,catalog:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,forms:manage,freezes:manage,groups:manage,knowledge:publish,knowledge:read_internal,knowledge:write,records:create:*,records:delete:*,records:read:*,records:update:*,restricted:read,roles:manage,scripts:edit,settings:manage,users:manage,users:read,workflows:edit,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();
DROP INDEX task_slas_running_due_idx;
DROP INDEX task_slas_breached_idx;
DROP INDEX task_slas_stopped_idx;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_business_calendars() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO business_calendars (name, label, time_zone, hours) VALUES
        ('business-hours', 'Business hours', 'UTC',
         '[{"day":1,"start":"09:00","end":"17:00"},{"day":2,"start":"09:00","end":"17:00"},{"day":3,"start":"09:00","end":"17:00"},
           {"day":4,"start":"09:00","end":"17:00"},{"day":5,"start":"09:00","end":"17:00"}]')
    ON CONFLICT (name) DO NOTHING;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION business_calendars_recheck() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE task_slas SET next_alert_at = now()
    WHERE stage = 'running' AND definition ->> 'calendar' IN (OLD.name, coalesce(NEW.name, OLD.name));
    RETURN NULL;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION business_calendars_recheck_new() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE task_slas SET next_alert_at = now() WHERE stage = 'running' AND definition ->> 'calendar' = NEW.name;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
DROP TRIGGER task_slas_calendar_history ON task_slas;
DROP FUNCTION task_slas_calendar_history();
ALTER TABLE groups DROP COLUMN calendar_id;
ALTER TABLE task_slas DROP COLUMN calendar;
