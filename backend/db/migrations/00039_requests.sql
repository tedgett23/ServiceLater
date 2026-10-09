-- +goose Up

-- Requests and the work under them (docs/apps-platform.md §9.2). A request (REQ) is fulfilled by tasks (TASK), each
-- worked by a team from its own queue: a record may have a parent. Tasks are a type of their own so any record type
-- can have them later (change tasks); the service-catalog app gives requests and tasks their lifecycles.
ALTER TABLE tasks ADD COLUMN parent_id uuid REFERENCES tasks (id) CHECK (parent_id <> id);
CREATE INDEX tasks_parent_idx ON tasks (parent_id) WHERE parent_id IS NOT NULL;

INSERT INTO task_types (type, prefix, label) VALUES ('work_task', 'TASK', 'Task');

-- A request is ordered from the service catalog: anyone may, for the items open to them. Filing one any other way
-- (no item) needs records:create:request; the Go save path says so, as RLS cannot see which item was ordered.
ALTER TABLE task_types ADD COLUMN from_catalog boolean NOT NULL DEFAULT false;
UPDATE task_types SET anyone_may_file = true, from_catalog = true WHERE type = 'request';

-- A request's own row: the item it was ordered from (empty when staff filed it by hand). It never changes.
CREATE TABLE requests (
    task_id      uuid PRIMARY KEY,
    type         text NOT NULL DEFAULT 'request' CHECK (type = 'request'),
    catalog_item text NOT NULL DEFAULT '' CHECK (catalog_item = '' OR catalog_item ~ '^[a-z][a-z0-9-]{1,62}$'),
    FOREIGN KEY (task_id, type) REFERENCES tasks (id, type) ON DELETE CASCADE
);
CREATE INDEX requests_item_idx ON requests (catalog_item) WHERE catalog_item <> '';
INSERT INTO requests (task_id) SELECT id FROM tasks WHERE type = 'request';
ALTER TABLE requests ENABLE ROW LEVEL SECURITY;
CREATE POLICY requests_select ON requests FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));
CREATE POLICY requests_insert ON requests FOR INSERT TO servicelater_app
    WITH CHECK (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));

-- A workflow waiting on a request (until its tasks are done) is woken when one of its tasks changes, as it is when
-- the request itself does.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION workflow_wake_on_task_change() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE workflow_executions SET wake_at = now()
     WHERE record_id IN (NEW.id, NEW.parent_id) AND status = 'waiting' AND wait_until AND wake_at > now();
    RETURN NULL;
END
$$;

-- Whoever has a request open sees its tasks come and go: a task's change is also its parent's.
CREATE FUNCTION tasks_notify_parent() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM task_cis_notify_for(NEW.parent_id);
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER tasks_wake_parent_workflows AFTER INSERT ON tasks
    FOR EACH ROW WHEN (NEW.parent_id IS NOT NULL) EXECUTE FUNCTION workflow_wake_on_task_change();
CREATE TRIGGER tasks_notify_parent AFTER INSERT OR UPDATE ON tasks
    FOR EACH ROW WHEN (NEW.parent_id IS NOT NULL) EXECUTE FUNCTION tasks_notify_parent();

-- Each person may have a manager, who approves what they request. Like a role, only those who manage people set it:
-- otherwise anyone could name their own approver.
ALTER TABLE users ADD COLUMN manager_id uuid REFERENCES users (id) ON DELETE SET NULL CHECK (manager_id <> id);
CREATE INDEX users_manager_idx ON users (manager_id) WHERE manager_id IS NOT NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION users_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF current_user = 'servicelater_app' THEN
        IF NOT app_can('users:manage')
           AND (NEW.role IS DISTINCT FROM OLD.role OR NEW.deactivated_at IS DISTINCT FROM OLD.deactivated_at) THEN
            RAISE EXCEPTION 'only an admin may change a role or deactivate someone' USING ERRCODE = '42501';
        END IF;
        IF NOT app_can('users:manage') AND NEW.manager_id IS DISTINCT FROM OLD.manager_id THEN
            RAISE EXCEPTION 'only those who manage people may set someone''s manager' USING ERRCODE = '42501';
        END IF;
        IF NEW.role IS DISTINCT FROM OLD.role AND 'admin' IN (NEW.role, OLD.role) AND NOT app_can('roles:manage') THEN
            RAISE EXCEPTION 'making someone an admin, or taking it away, needs roles:manage' USING ERRCODE = '42501';
        END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION users_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF current_user = 'servicelater_app' THEN
        IF NOT app_can('users:manage')
           AND (NEW.role IS DISTINCT FROM OLD.role OR NEW.deactivated_at IS DISTINCT FROM OLD.deactivated_at) THEN
            RAISE EXCEPTION 'only an admin may change a role or deactivate someone' USING ERRCODE = '42501';
        END IF;
        IF NEW.role IS DISTINCT FROM OLD.role AND 'admin' IN (NEW.role, OLD.role) AND NOT app_can('roles:manage') THEN
            RAISE EXCEPTION 'making someone an admin, or taking it away, needs roles:manage' USING ERRCODE = '42501';
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION workflow_wake_on_task_change() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE workflow_executions SET wake_at = now()
     WHERE record_id = NEW.id AND status = 'waiting' AND wait_until AND wake_at > now();
    RETURN NULL;
END
$$;
-- +goose StatementEnd
DROP TABLE requests;
UPDATE task_types SET anyone_may_file = false WHERE type = 'request';
ALTER TABLE task_types DROP COLUMN from_catalog;
DROP INDEX users_manager_idx;
ALTER TABLE users DROP COLUMN manager_id;
DROP TRIGGER tasks_notify_parent ON tasks;
DROP TRIGGER tasks_wake_parent_workflows ON tasks;
DROP FUNCTION tasks_notify_parent();
DELETE FROM tasks WHERE type = 'work_task';
DELETE FROM task_types WHERE type = 'work_task';
DROP SEQUENCE IF EXISTS task_number_work_task;
DROP INDEX tasks_parent_idx;
ALTER TABLE tasks DROP COLUMN parent_id;
