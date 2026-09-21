-- +goose Up

-- Record types share one base table, `tasks`, and each type that needs typed columns adds a small extension table
-- (class-table inheritance, docs/apps-platform.md §9.2). This migration turns the old `tickets` table into `tasks`,
-- makes every existing row an incident, and gives comments a real foreign key to tasks.
--
-- Nothing is released yet, so tables and columns are renamed directly instead of in two phases.

-- The registry of task types. A type is a row here; a numbering sequence is created with it. A type with no
-- extension table is a lightweight type: just a `tasks` row.
CREATE TABLE task_types (
    type   text PRIMARY KEY CHECK (type ~ '^[a-z][a-z0-9_]{1,30}$'),
    prefix text NOT NULL UNIQUE CHECK (prefix ~ '^[A-Z]{2,5}$'),
    label  text NOT NULL
);
ALTER TABLE task_types ENABLE ROW LEVEL SECURITY;
-- Everyone signed in may read the registry (the prefix is part of a ticket's reference); only the owner writes it.
CREATE POLICY task_types_select ON task_types FOR SELECT TO servicelater_app USING (true);

-- Each type numbers its records independently: INC1, INC2 ... and CHG1, CHG2 ...
-- +goose StatementBegin
CREATE FUNCTION task_types_create_sequence() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    EXECUTE format('CREATE SEQUENCE %I', 'task_number_' || NEW.type);
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER task_types_create_sequence AFTER INSERT ON task_types
    FOR EACH ROW EXECUTE FUNCTION task_types_create_sequence();

INSERT INTO task_types (type, prefix, label) VALUES
    ('incident', 'INC', 'Incident'),
    ('change',   'CHG', 'Change'),
    ('request',  'REQ', 'Request'),
    ('problem',  'PRB', 'Problem');

-- Existing numbers are kept: incidents carry on from where tickets left off.
SELECT setval('task_number_incident', COALESCE((SELECT max(number) FROM tickets), 0) + 1, false);

-- tickets becomes tasks
ALTER TABLE tickets RENAME TO tasks;
ALTER TABLE tasks ADD COLUMN type text NOT NULL DEFAULT 'incident' REFERENCES task_types (type);
ALTER TABLE tasks ALTER COLUMN type DROP DEFAULT;

ALTER TABLE tasks ALTER COLUMN number DROP IDENTITY;
ALTER TABLE tasks ALTER COLUMN number SET DEFAULT 0; -- 0 means "assign one": see tasks_assign_number
ALTER TABLE tasks ALTER COLUMN number SET NOT NULL; -- already true (it was an identity column); stated so tools see it
ALTER TABLE tasks DROP CONSTRAINT tickets_number_key;
ALTER TABLE tasks ADD CONSTRAINT tasks_type_number_key UNIQUE (type, number);
-- Lets an extension table prove its row belongs to a task of the right type (see incidents below).
ALTER TABLE tasks ADD CONSTRAINT tasks_id_type_key UNIQUE (id, type);

ALTER TABLE tasks RENAME CONSTRAINT tickets_pkey TO tasks_pkey;
ALTER TABLE tasks RENAME CONSTRAINT tickets_requester_id_fkey TO tasks_requester_id_fkey;
ALTER TABLE tasks RENAME CONSTRAINT tickets_assignee_id_fkey TO tasks_assignee_id_fkey;
ALTER TABLE tasks RENAME CONSTRAINT tickets_priority_check TO tasks_priority_check;
ALTER TABLE tasks RENAME CONSTRAINT tickets_state_category_check TO tasks_state_category_check;
ALTER INDEX tickets_requester_idx RENAME TO tasks_requester_idx;
ALTER INDEX tickets_assignee_idx RENAME TO tasks_assignee_idx;
ALTER INDEX tickets_status_idx RENAME TO tasks_status_idx;
ALTER INDEX tickets_state_category_idx RENAME TO tasks_state_category_idx;
ALTER POLICY tickets_select ON tasks RENAME TO tasks_select;
ALTER POLICY tickets_insert ON tasks RENAME TO tasks_insert;
ALTER POLICY tickets_update ON tasks RENAME TO tasks_update;
ALTER POLICY tickets_delete ON tasks RENAME TO tasks_delete;

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

DROP TRIGGER tickets_touch_and_notify ON tasks;
DROP FUNCTION tickets_touch_and_notify();

-- Realtime payloads still carry ids only (never content); `type` is added so listeners can route by it.
-- Trigger names run in alphabetical order, so the number is assigned before the notification is built.
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

-- Comments belong to any task, with a real foreign key.
ALTER TABLE ticket_comments RENAME TO task_comments;
ALTER TABLE task_comments RENAME COLUMN ticket_id TO task_id;
ALTER TABLE task_comments RENAME CONSTRAINT ticket_comments_pkey TO task_comments_pkey;
ALTER TABLE task_comments RENAME CONSTRAINT ticket_comments_ticket_id_fkey TO task_comments_task_id_fkey;
ALTER TABLE task_comments RENAME CONSTRAINT ticket_comments_author_id_fkey TO task_comments_author_id_fkey;
ALTER TABLE task_comments RENAME CONSTRAINT ticket_comments_body_check TO task_comments_body_check;
ALTER INDEX ticket_comments_ticket_idx RENAME TO task_comments_task_idx;

-- The function body names the table as text, so it does not follow the rename: replace it.
-- The payload key stays `ticket_id`: "ticket" is what people call these.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION comments_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('comment_changes', json_build_object(
        'op', TG_OP, 'id', NEW.id, 'ticket_id', NEW.task_id, 'internal', NEW.internal,
        'requester_id', (SELECT requester_id FROM tasks WHERE id = NEW.task_id))::text);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- Incident-specific columns. The composite foreign key ties each row to a task whose type really is 'incident', so
-- an incident row can never hang off a change.
CREATE TABLE incidents (
    task_id    uuid PRIMARY KEY,
    type       text NOT NULL DEFAULT 'incident' CHECK (type = 'incident'),
    resolution text NOT NULL DEFAULT '' CHECK (length(resolution) <= 10000),
    FOREIGN KEY (task_id, type) REFERENCES tasks (id, type) ON DELETE CASCADE
);
INSERT INTO incidents (task_id) SELECT id FROM tasks;

ALTER TABLE incidents ENABLE ROW LEVEL SECURITY;
-- An extension row is visible, and can be created, exactly when its task is (tasks RLS applies in the subquery);
-- only staff change it. Deleting is by cascade from the task.
CREATE POLICY incidents_select ON incidents FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));
CREATE POLICY incidents_insert ON incidents FOR INSERT TO servicelater_app
    WITH CHECK (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));
CREATE POLICY incidents_update ON incidents FOR UPDATE TO servicelater_app
    USING (app_is_staff() AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id))
    WITH CHECK (app_is_staff());

-- Changing an extension row is a change to the ticket: bump updated_at and notify, through the task's own trigger.
-- +goose StatementBegin
CREATE FUNCTION incidents_touch_task() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE tasks SET updated_at = now() WHERE id = NEW.task_id;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER incidents_touch_task AFTER UPDATE ON incidents
    FOR EACH ROW EXECUTE FUNCTION incidents_touch_task();

-- +goose Down
-- Rolling back is only possible while every task is an incident, because the old table numbers all tickets in one
-- sequence and has no type. Anything in an incident-only column (resolution) is lost.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM tasks WHERE type <> 'incident') THEN
        RAISE EXCEPTION 'cannot roll back: tasks of other types exist';
    END IF;
END
$$;
-- +goose StatementEnd

DROP TRIGGER incidents_touch_task ON incidents;
DROP FUNCTION incidents_touch_task();
DROP TABLE incidents;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION comments_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('comment_changes', json_build_object(
        'op', TG_OP, 'id', NEW.id, 'ticket_id', NEW.ticket_id, 'internal', NEW.internal,
        'requester_id', (SELECT requester_id FROM tickets WHERE id = NEW.ticket_id))::text);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

ALTER INDEX task_comments_task_idx RENAME TO ticket_comments_ticket_idx;
ALTER TABLE task_comments RENAME CONSTRAINT task_comments_body_check TO ticket_comments_body_check;
ALTER TABLE task_comments RENAME CONSTRAINT task_comments_author_id_fkey TO ticket_comments_author_id_fkey;
ALTER TABLE task_comments RENAME CONSTRAINT task_comments_task_id_fkey TO ticket_comments_ticket_id_fkey;
ALTER TABLE task_comments RENAME CONSTRAINT task_comments_pkey TO ticket_comments_pkey;
ALTER TABLE task_comments RENAME COLUMN task_id TO ticket_id;
ALTER TABLE task_comments RENAME TO ticket_comments;

DROP TRIGGER tasks_touch_and_notify ON tasks;
DROP TRIGGER tasks_assign_number ON tasks;
DROP FUNCTION tasks_touch_and_notify();
DROP FUNCTION tasks_assign_number();

-- +goose StatementBegin
CREATE FUNCTION tickets_touch_and_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        NEW.updated_at = now();
    END IF;
    PERFORM pg_notify('ticket_changes', json_build_object(
        'op', TG_OP, 'id', NEW.id, 'number', NEW.number, 'status', NEW.status,
        'requester_id', NEW.requester_id, 'assignee_id', NEW.assignee_id)::text);
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER tickets_touch_and_notify BEFORE INSERT OR UPDATE ON tasks
    FOR EACH ROW EXECUTE FUNCTION tickets_touch_and_notify();

ALTER POLICY tasks_delete ON tasks RENAME TO tickets_delete;
ALTER POLICY tasks_update ON tasks RENAME TO tickets_update;
ALTER POLICY tasks_insert ON tasks RENAME TO tickets_insert;
ALTER POLICY tasks_select ON tasks RENAME TO tickets_select;
ALTER INDEX tasks_state_category_idx RENAME TO tickets_state_category_idx;
ALTER INDEX tasks_status_idx RENAME TO tickets_status_idx;
ALTER INDEX tasks_assignee_idx RENAME TO tickets_assignee_idx;
ALTER INDEX tasks_requester_idx RENAME TO tickets_requester_idx;
ALTER TABLE tasks RENAME CONSTRAINT tasks_state_category_check TO tickets_state_category_check;
ALTER TABLE tasks RENAME CONSTRAINT tasks_priority_check TO tickets_priority_check;
ALTER TABLE tasks RENAME CONSTRAINT tasks_assignee_id_fkey TO tickets_assignee_id_fkey;
ALTER TABLE tasks RENAME CONSTRAINT tasks_requester_id_fkey TO tickets_requester_id_fkey;
ALTER TABLE tasks RENAME CONSTRAINT tasks_pkey TO tickets_pkey;

ALTER TABLE tasks DROP CONSTRAINT tasks_id_type_key;
ALTER TABLE tasks DROP CONSTRAINT tasks_type_number_key;
ALTER TABLE tasks DROP COLUMN type;
ALTER TABLE tasks ALTER COLUMN number DROP DEFAULT;
ALTER TABLE tasks ALTER COLUMN number ADD GENERATED ALWAYS AS IDENTITY;
SELECT setval(pg_get_serial_sequence('tasks', 'number'), COALESCE((SELECT max(number) FROM tasks), 0) + 1, false);
ALTER TABLE tasks ADD CONSTRAINT tickets_number_key UNIQUE (number);
ALTER TABLE tasks RENAME TO tickets;

DROP TRIGGER task_types_create_sequence ON task_types;
DROP FUNCTION task_types_create_sequence();
-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOR t IN SELECT type FROM task_types LOOP
        EXECUTE format('DROP SEQUENCE IF EXISTS %I', 'task_number_' || t);
    END LOOP;
END
$$;
-- +goose StatementEnd
DROP TABLE task_types;
