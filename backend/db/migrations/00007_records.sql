-- +goose Up
-- Records (docs/apps-platform.md §9). Every record type shares one base table, `tasks` (people and the API say
-- "ticket"), and a type that needs typed columns adds a small extension table (class-table inheritance): incidents,
-- changes, requests. Types are rows in task_types, core or defined by apps, each numbering its records on its own
-- (INC42, CHG7). Statuses are not in the schema: they come from the type's lifecycle (an app definition), and core
-- logic works on state_category, stored on the row so it keeps working if that app is removed.

-- The registry of record types. A type with no extension table is just a `tasks` row.
CREATE TABLE task_types (
    type            text PRIMARY KEY CHECK (type ~ '^[a-z][a-z0-9_]{1,30}$'),
    prefix          text NOT NULL UNIQUE CHECK (prefix ~ '^[A-Z]{2,5}$'),
    label           text NOT NULL,
    plural          text NOT NULL DEFAULT '',
    description     text NOT NULL DEFAULT '',
    -- Whether anyone signed in may file one as themselves, without records:create:<type> (incidents, requests).
    anyone_may_file boolean NOT NULL DEFAULT false,
    -- Ordered from the service catalog (requests): anyone may, for the items open to them. Filing one any other way
    -- needs records:create:<type>; the Go save path says so, as RLS cannot see which item was ordered.
    from_catalog    boolean NOT NULL DEFAULT false,
    -- The app that defines it (record_type definitions, §9.5); NULL for the core types. No foreign key: the app's row
    -- goes on uninstall and the type must not. A type whose app is removed is retired: its records keep their
    -- references and can still be opened, but nothing lists or files them.
    owner_app       text,
    retired_at      timestamptz
);
-- Everyone signed in may read the registry (the prefix is part of a record's reference); only the owner writes it.
ALTER TABLE task_types ENABLE ROW LEVEL SECURITY;
CREATE POLICY task_types_select ON task_types FOR SELECT TO servicelater_app USING (true);

-- Each type numbers its records independently: INC1, INC2 ... and CHG1, CHG2 ... An app's type can be removed and
-- defined again (tests do), so a sequence left behind is reused, not an error.
-- +goose StatementBegin
CREATE FUNCTION task_types_create_sequence() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    EXECUTE format('CREATE SEQUENCE IF NOT EXISTS %I', 'task_number_' || NEW.type);
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER task_types_create_sequence AFTER INSERT ON task_types
    FOR EACH ROW EXECUTE FUNCTION task_types_create_sequence();

-- The core types (apps.CoreRecordTypes). Apps give them their lifecycles and forms.
INSERT INTO task_types (type, prefix, label, plural, anyone_may_file, from_catalog) VALUES
    ('incident',  'INC',  'Incident', 'Incidents', true,  false),
    ('change',    'CHG',  'Change',   'Changes',   false, false),
    ('request',   'REQ',  'Request',  'Requests',  true,  true),
    ('problem',   'PRB',  'Problem',  'Problems',  false, false),
    ('work_task', 'TASK', 'Task',     'Tasks',     false, false);

-- Nobody may file a record of a retired type, whatever it allowed before.
-- +goose StatementBegin
CREATE FUNCTION app_anyone_may_file(p_type text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$ SELECT coalesce((SELECT anyone_may_file AND retired_at IS NULL FROM task_types WHERE type = p_type), false) $$;
-- +goose StatementEnd

-- Records. Only the save path (internal/records) writes them.
CREATE TABLE tasks (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    type                text NOT NULL REFERENCES task_types (type),
    -- 0 means "assign one": see tasks_assign_number.
    number              bigint NOT NULL DEFAULT 0,
    title               text NOT NULL,
    description         text NOT NULL DEFAULT '',
    -- A status of the type's lifecycle, and its category, which core logic uses ("open" means open or waiting).
    status              text NOT NULL DEFAULT 'new',
    state_category      text NOT NULL DEFAULT 'open' CHECK (state_category IN ('open', 'waiting', 'done', 'closed')),
    priority            smallint NOT NULL DEFAULT 3 CHECK (priority BETWEEN 1 AND 4),
    requester_id        uuid NOT NULL REFERENCES users (id),
    assignee_id         uuid REFERENCES users (id),
    -- The team it is routed to. A group with records cannot be deleted (no cascade): they would change without
    -- passing through the save path, its rules and its audit.
    assignment_group_id uuid REFERENCES groups (id),
    -- A request's tasks (one level; deleting a parent with tasks is refused).
    parent_id           uuid REFERENCES tasks (id) CHECK (parent_id <> id),
    -- Extra fields, the ones the type's form declares (docs/apps-platform.md §8.1), so an app adds fields without a
    -- table change. Values are text, whole numbers or booleans (dates as YYYY-MM-DD, times as RFC 3339 text);
    -- internal/records checks every value against the form on each save, the database only keeps it a sane object.
    fields              jsonb NOT NULL DEFAULT '{}'::jsonb
                        CONSTRAINT tasks_fields_object CHECK (jsonb_typeof(fields) = 'object' AND pg_column_size(fields) <= 65536),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tasks_type_number_key UNIQUE (type, number),
    -- Lets an extension table prove its row belongs to a record of the right type.
    CONSTRAINT tasks_id_type_key UNIQUE (id, type)
);
CREATE INDEX tasks_requester_idx ON tasks (requester_id);
CREATE INDEX tasks_assignee_idx ON tasks (assignee_id) WHERE assignee_id IS NOT NULL;
CREATE INDEX tasks_assignment_group_idx ON tasks (assignment_group_id) WHERE assignment_group_id IS NOT NULL;
CREATE INDEX tasks_parent_idx ON tasks (parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX tasks_status_idx ON tasks (status);
CREATE INDEX tasks_state_category_idx ON tasks (state_category);

-- A record the caller is saving stays visible to them until the transaction ends, even when the save routes it to a
-- restricted group they are not in (handing a misfiled ticket to HR): without this, Postgres would refuse the save,
-- because the row it returns would no longer pass the read policy. Only the save path (internal/records) marks a
-- record, after reading it under RLS, or for a record it is about to create.
-- +goose StatementBegin
CREATE FUNCTION app_mark_saving(p_task uuid) RETURNS void
    LANGUAGE sql VOLATILE
    AS $$ SELECT set_config('app.saving', coalesce(current_setting('app.saving', true), ',') || p_task::text || ',', true) $$;
-- +goose StatementEnd

-- Who sees a record: its requester, and those who may read its type, unless it is routed to a restricted group they
-- are not in (the assignee and restricted:read holders still see it). This is the read policy, and the same check
-- SECURITY DEFINER code uses to judge a record for the caller; saving says whether a record being saved counts.
-- internal/realtime mirrors it (visibleTo, recordAudience).
-- +goose StatementBegin
CREATE FUNCTION app_sees_task(p_id uuid, p_type text, p_requester uuid, p_assignee uuid, p_group uuid, p_saving boolean)
    RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT p_requester = app_current_user_id()
        OR (p_saving AND position(',' || p_id::text || ',' IN coalesce(current_setting('app.saving', true), ',')) > 0)
        OR (app_can_record('read', p_type)
            AND (p_group IS NULL
                 OR p_assignee = app_current_user_id()
                 OR position(',' || p_group::text || ',' IN coalesce(current_setting('app.hidden_groups', true), ',')) = 0));
$$;
-- +goose StatementEnd

-- The same, for someone other than the caller (who gets a notification, say). Works it out from scratch.
-- +goose StatementBegin
CREATE FUNCTION app_user_sees_task(p_user uuid, p_type text, p_requester uuid, p_assignee uuid, p_group uuid)
    RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$
    SELECT p_user = p_requester
        OR (app_user_can_record(p_user, 'read', p_type)
            AND (p_group IS NULL
                 OR p_assignee = p_user
                 OR NOT EXISTS (SELECT 1 FROM groups g WHERE g.id = p_group AND g.restricted)
                 OR EXISTS (SELECT 1 FROM group_members m WHERE m.group_id = p_group AND m.user_id = p_user)
                 OR app_user_can(p_user, 'restricted:read')));
$$;
-- +goose StatementEnd

-- Filing needs records:create:<type> (or a type anyone may file), and filing for someone else records:update:<type>
-- too. Changing needs records:update:<type> and sight of the record; deleting records:delete:<type>.
ALTER TABLE tasks ENABLE ROW LEVEL SECURITY;
CREATE POLICY tasks_select ON tasks FOR SELECT TO servicelater_app
    USING (app_sees_task(id, type, requester_id, assignee_id, assignment_group_id, true));
CREATE POLICY tasks_insert ON tasks FOR INSERT TO servicelater_app
    WITH CHECK ((app_can_record('create', type) OR app_anyone_may_file(type))
                AND (requester_id = app_current_user_id() OR app_can_record('update', type)));
CREATE POLICY tasks_update ON tasks FOR UPDATE TO servicelater_app
    USING (app_can_record('update', type) AND app_sees_task(id, type, requester_id, assignee_id, assignment_group_id, true))
    WITH CHECK (app_can_record('update', type));
CREATE POLICY tasks_delete ON tasks FOR DELETE TO servicelater_app
    USING (app_can_record('delete', type) AND app_sees_task(id, type, requester_id, assignee_id, assignment_group_id, false));

-- SECURITY DEFINER so that numbering works for whoever inserts, without granting them the sequences.
-- +goose StatementBegin
CREATE FUNCTION tasks_assign_number() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    -- An unregistered type is left at 0 so the foreign key on type reports it, not a missing sequence.
    IF NEW.number = 0 AND EXISTS (SELECT 1 FROM task_types WHERE type = NEW.type) THEN
        NEW.number := nextval(('task_number_' || NEW.type)::regclass);
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- Realtime hints carry ids only, never content (clients refetch through the API, so RLS stays the one authority), plus
-- what routing needs: the type and who requested it and is assigned. Trigger names run in alphabetical order, so the
-- number is assigned before the notification is built.
-- +goose StatementBegin
CREATE FUNCTION tasks_touch_and_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        NEW.updated_at = now();
    END IF;
    PERFORM pg_notify('ticket_changes', json_build_object(
        'op', TG_OP, 'id', NEW.id, 'type', NEW.type, 'number', NEW.number, 'status', NEW.status,
        'requester_id', NEW.requester_id, 'assignee_id', NEW.assignee_id)::text);
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER tasks_assign_number BEFORE INSERT ON tasks
    FOR EACH ROW EXECUTE FUNCTION tasks_assign_number();
CREATE TRIGGER tasks_touch_and_notify BEFORE INSERT OR UPDATE ON tasks
    FOR EACH ROW EXECUTE FUNCTION tasks_touch_and_notify();

-- The same hint for a record something else changed: an item, article or file linked to it, one of a request's
-- tasks, an incident or change linked to a problem.
-- +goose StatementBegin
CREATE FUNCTION task_notify(p_task uuid) RETURNS void
    LANGUAGE sql SECURITY DEFINER SET search_path = public
    AS $$
    SELECT pg_notify('ticket_changes', json_build_object(
        'op', 'UPDATE', 'id', t.id, 'type', t.type, 'number', t.number, 'status', t.status,
        'requester_id', t.requester_id, 'assignee_id', t.assignee_id)::text)
    FROM tasks t WHERE t.id = p_task;
$$;
-- +goose StatementEnd
-- Only triggers (as the owner) call it: nobody may announce changes to records directly.
REVOKE ALL ON FUNCTION task_notify(uuid) FROM PUBLIC;

-- As a trigger on a table linking things to records (task_id): a link added or removed changes what the record shows.
-- +goose StatementBegin
CREATE FUNCTION task_link_notify() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM task_notify(coalesce(NEW.task_id, OLD.task_id));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- Whoever has a request open sees its tasks come and go: a task's change is also its parent's.
-- +goose StatementBegin
CREATE FUNCTION tasks_notify_parent() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM task_notify(NEW.parent_id);
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER tasks_notify_parent AFTER INSERT OR UPDATE ON tasks
    FOR EACH ROW WHEN (NEW.parent_id IS NOT NULL) EXECUTE FUNCTION tasks_notify_parent();

-- Changing an extension row is a change to the record: bump updated_at and notify, through the task's own trigger.
-- +goose StatementBegin
CREATE FUNCTION extension_touch_task() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE tasks SET updated_at = now() WHERE id = NEW.task_id;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- Incident columns. The composite foreign key ties each row to a task whose type really is 'incident', so an incident
-- row can never hang off a change; every extension table does the same. An extension row is visible, and can be
-- created, exactly when its task is (tasks RLS applies in the subquery); deleting is by cascade from the task.
CREATE TABLE incidents (
    task_id      uuid PRIMARY KEY,
    type         text NOT NULL DEFAULT 'incident' CHECK (type = 'incident'),
    resolution   text NOT NULL DEFAULT '' CHECK (length(resolution) <= 10000),
    -- The problem behind it (docs/apps-platform.md §9.7), one per incident. The generated column lets the foreign key
    -- prove the other end is a problem; deleting a problem that incidents point at is refused (no ON DELETE action),
    -- so a link never vanishes outside the save path.
    problem_id   uuid,
    problem_type text GENERATED ALWAYS AS (CASE WHEN problem_id IS NULL THEN NULL ELSE 'problem' END) STORED,
    FOREIGN KEY (task_id, type) REFERENCES tasks (id, type) ON DELETE CASCADE,
    CONSTRAINT incidents_problem_fk FOREIGN KEY (problem_id, problem_type) REFERENCES tasks (id, type)
);
CREATE INDEX incidents_problem_idx ON incidents (problem_id) WHERE problem_id IS NOT NULL;
ALTER TABLE incidents ENABLE ROW LEVEL SECURITY;
CREATE POLICY incidents_select ON incidents FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));
CREATE POLICY incidents_insert ON incidents FOR INSERT TO servicelater_app
    WITH CHECK (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));
CREATE POLICY incidents_update ON incidents FOR UPDATE TO servicelater_app
    USING (app_can_record('update', 'incident') AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id))
    WITH CHECK (app_can_record('update', 'incident'));
CREATE TRIGGER incidents_touch_task AFTER UPDATE ON incidents
    FOR EACH ROW EXECUTE FUNCTION extension_touch_task();

-- Change columns. A change has a kind (standard changes are pre-approved, normal ones go to the change advisory board,
-- emergency ones to a smaller board, fast), an assessed risk and impact, a planned window, and the plans that say how
-- it is done, tested and undone. Empty text means not filled in yet; a guard in the change lifecycle decides when it
-- must be. Allowed values are mirrored in apps.ChangeTypes and apps.Levels.
CREATE TABLE changes (
    task_id             uuid PRIMARY KEY,
    type                text NOT NULL DEFAULT 'change' CHECK (type = 'change'),
    change_type         text NOT NULL DEFAULT 'normal' CHECK (change_type IN ('standard', 'normal', 'emergency')),
    risk                text NOT NULL DEFAULT '' CHECK (risk IN ('', 'low', 'medium', 'high')),
    impact              text NOT NULL DEFAULT '' CHECK (impact IN ('', 'low', 'medium', 'high')),
    planned_start       timestamptz,
    planned_end         timestamptz,
    implementation_plan text NOT NULL DEFAULT '' CHECK (length(implementation_plan) <= 10000),
    backout_plan        text NOT NULL DEFAULT '' CHECK (length(backout_plan) <= 10000),
    test_plan           text NOT NULL DEFAULT '' CHECK (length(test_plan) <= 10000),
    -- The problem it fixes. Several changes may fix one problem (a first attempt, a retry); same shape as incidents'.
    problem_id          uuid,
    problem_type        text GENERATED ALWAYS AS (CASE WHEN problem_id IS NULL THEN NULL ELSE 'problem' END) STORED,
    CONSTRAINT changes_window_check CHECK (planned_end > planned_start),
    FOREIGN KEY (task_id, type) REFERENCES tasks (id, type) ON DELETE CASCADE,
    CONSTRAINT changes_problem_fk FOREIGN KEY (problem_id, problem_type) REFERENCES tasks (id, type)
);
-- The change calendar asks which windows overlap a range.
CREATE INDEX changes_window_idx ON changes (planned_start, planned_end) WHERE planned_start IS NOT NULL;
CREATE INDEX changes_problem_idx ON changes (problem_id) WHERE problem_id IS NOT NULL;
ALTER TABLE changes ENABLE ROW LEVEL SECURITY;
CREATE POLICY changes_select ON changes FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));
CREATE POLICY changes_insert ON changes FOR INSERT TO servicelater_app
    WITH CHECK (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));
CREATE POLICY changes_update ON changes FOR UPDATE TO servicelater_app
    USING (app_can_record('update', 'change') AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id))
    WITH CHECK (app_can_record('update', 'change'));
CREATE TRIGGER changes_touch_task AFTER UPDATE ON changes
    FOR EACH ROW EXECUTE FUNCTION extension_touch_task();

-- A request's own row: the catalog item it was ordered from (empty when staff filed it by hand). It never changes.
CREATE TABLE requests (
    task_id      uuid PRIMARY KEY,
    type         text NOT NULL DEFAULT 'request' CHECK (type = 'request'),
    catalog_item text NOT NULL DEFAULT '' CHECK (catalog_item = '' OR catalog_item ~ '^[a-z][a-z0-9-]{1,62}$'),
    FOREIGN KEY (task_id, type) REFERENCES tasks (id, type) ON DELETE CASCADE
);
CREATE INDEX requests_item_idx ON requests (catalog_item) WHERE catalog_item <> '';
ALTER TABLE requests ENABLE ROW LEVEL SECURITY;
CREATE POLICY requests_select ON requests FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));
CREATE POLICY requests_insert ON requests FOR INSERT TO servicelater_app
    WITH CHECK (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));

-- Whoever has a problem open sees its incidents and changes come, go and move: linking or unlinking one, or a linked
-- one changing status, is a change to the problem too (ids only, as ever).
-- +goose StatementBegin
CREATE FUNCTION problem_link_notify() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF OLD.problem_id IS NOT NULL THEN
        PERFORM task_notify(OLD.problem_id);
    END IF;
    IF NEW.problem_id IS NOT NULL AND NEW.problem_id IS DISTINCT FROM OLD.problem_id THEN
        PERFORM task_notify(NEW.problem_id);
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER incidents_notify_problem AFTER UPDATE OF problem_id ON incidents
    FOR EACH ROW WHEN (OLD.problem_id IS DISTINCT FROM NEW.problem_id) EXECUTE FUNCTION problem_link_notify();
CREATE TRIGGER changes_notify_problem AFTER UPDATE OF problem_id ON changes
    FOR EACH ROW WHEN (OLD.problem_id IS DISTINCT FROM NEW.problem_id) EXECUTE FUNCTION problem_link_notify();

-- +goose StatementBegin
CREATE FUNCTION tasks_notify_problem() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM task_notify(i.problem_id) FROM incidents i WHERE i.task_id = NEW.id AND i.problem_id IS NOT NULL;
    PERFORM task_notify(c.problem_id) FROM changes c WHERE c.task_id = NEW.id AND c.problem_id IS NOT NULL;
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER tasks_notify_problem AFTER UPDATE OF status ON tasks
    FOR EACH ROW WHEN (NEW.type IN ('incident', 'change') AND OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION tasks_notify_problem();

-- Comments on a record, visible when the record is; internal notes only to comments:internal. Append-only (no update
-- policy). An app's comments say whom it acted for.
CREATE TABLE task_comments (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id      uuid NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    author_id    uuid NOT NULL REFERENCES users (id),
    on_behalf_of uuid REFERENCES users (id) DEFAULT app_on_behalf_of(),
    body         text NOT NULL CHECK (length(body) BETWEEN 1 AND 10000),
    internal     boolean NOT NULL DEFAULT false, -- work notes
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX task_comments_task_idx ON task_comments (task_id, created_at);
ALTER TABLE task_comments ENABLE ROW LEVEL SECURITY;
CREATE POLICY comments_select ON task_comments FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id) AND (NOT internal OR app_can('comments:internal')));
CREATE POLICY comments_insert ON task_comments FOR INSERT TO servicelater_app
    WITH CHECK (author_id = app_current_user_id()
                AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id)
                AND (NOT internal OR app_can('comments:internal')));
CREATE POLICY comments_delete ON task_comments FOR DELETE TO servicelater_app USING (app_can('comments:delete'));

-- Ids only, never the body; the record's type and requester for routing, and whether it is internal (those go only
-- to comments:internal). The payload key stays ticket_id: "ticket" is what people call these.
-- +goose StatementBegin
CREATE FUNCTION comments_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('comment_changes', (SELECT json_build_object(
        'op', TG_OP, 'id', NEW.id, 'ticket_id', NEW.task_id, 'internal', NEW.internal,
        'requester_id', t.requester_id, 'type', t.type) FROM tasks t WHERE t.id = NEW.task_id)::text);
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER comments_notify AFTER INSERT ON task_comments
    FOR EACH ROW EXECUTE FUNCTION comments_notify();

-- +goose Down
DROP TABLE task_comments;
DROP FUNCTION comments_notify();
DROP TABLE requests;
DROP TABLE changes;
DROP TABLE incidents;
DROP TABLE tasks;
DROP FUNCTION tasks_notify_problem();
DROP FUNCTION problem_link_notify();
DROP FUNCTION extension_touch_task();
DROP FUNCTION tasks_notify_parent();
DROP FUNCTION task_link_notify();
DROP FUNCTION task_notify(uuid);
DROP FUNCTION tasks_touch_and_notify();
DROP FUNCTION tasks_assign_number();
DROP FUNCTION app_user_sees_task(uuid, text, uuid, uuid, uuid);
DROP FUNCTION app_sees_task(uuid, text, uuid, uuid, uuid, boolean);
DROP FUNCTION app_mark_saving(uuid);
DROP FUNCTION app_anyone_may_file(text);
DROP TABLE task_types;
DROP FUNCTION task_types_create_sequence();
-- The numbering sequences task_types_create_sequence made.
-- +goose StatementBegin
DO $$
DECLARE s record;
BEGIN
    FOR s IN SELECT sequencename FROM pg_sequences WHERE schemaname = 'public' AND sequencename LIKE 'task\_number\_%' LOOP
        EXECUTE format('DROP SEQUENCE %I', s.sequencename);
    END LOOP;
END
$$;
-- +goose StatementEnd
