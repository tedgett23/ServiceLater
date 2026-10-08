-- +goose Up

-- The CMDB (docs/apps-platform.md §8.2): configuration items, the relationships between them, and which records
-- they are linked to. The tables are core, like freezes, because people edit items at run time; what an item can be
-- (its class and that class's fields) and how items relate are app definitions (classes/, relationships/), so neither
-- has a CHECK here. An item's own attributes are in `fields`, as a ticket's extra fields are.
CREATE TABLE cis (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    class            text NOT NULL CHECK (class ~ '^[a-z][a-z0-9_]{1,39}$'),
    name             text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    status           text NOT NULL DEFAULT 'active' CHECK (status IN ('planned', 'active', 'maintenance', 'retired')),
    description      text NOT NULL DEFAULT '' CHECK (length(description) <= 10000),
    owner_group_id   uuid REFERENCES groups (id),
    support_group_id uuid REFERENCES groups (id),
    fields           jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT cis_fields_object CHECK (jsonb_typeof(fields) = 'object' AND pg_column_size(fields) <= 65536),
    created_by       uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
-- Two items of a class cannot share a name (case aside): people find items by name, and an import matches on it.
CREATE UNIQUE INDEX cis_class_name_key ON cis (class, lower(name));
CREATE INDEX cis_name_idx ON cis (lower(name) text_pattern_ops);
CREATE INDEX cis_support_group_idx ON cis (support_group_id) WHERE support_group_id IS NOT NULL;
CREATE INDEX cis_owner_group_idx ON cis (owner_group_id) WHERE owner_group_id IS NOT NULL;

-- from_ci <type> to_ci, read forwards: "payroll depends on postgres-01". Whether trouble with to_ci affects from_ci
-- (impact) is the relationship type's definition.
CREATE TABLE ci_relationships (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    from_ci    uuid NOT NULL REFERENCES cis (id) ON DELETE CASCADE,
    to_ci      uuid NOT NULL REFERENCES cis (id) ON DELETE CASCADE,
    type       text NOT NULL CHECK (type ~ '^[a-z][a-z0-9_]{1,39}$'),
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ci_relationships_not_self CHECK (from_ci <> to_ci),
    CONSTRAINT ci_relationships_key UNIQUE (from_ci, to_ci, type)
);
CREATE INDEX ci_relationships_to_idx ON ci_relationships (to_ci, type);

-- Records linked to items: the items an incident affects, the items a change touches. An item linked to any record
-- cannot be deleted (no ON DELETE action: retire it instead), so a record never loses what it was about.
CREATE TABLE task_cis (
    task_id    uuid NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    ci_id      uuid NOT NULL REFERENCES cis (id),
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, ci_id)
);
CREATE INDEX task_cis_ci_idx ON task_cis (ci_id);

-- cmdb:read sees items and how they relate, cmdb:manage changes them. A link is seen by whoever sees both the record
-- and items, and made by whoever may also update the record.
ALTER TABLE cis ENABLE ROW LEVEL SECURITY;
CREATE POLICY cis_select ON cis FOR SELECT TO servicelater_app USING (app_can('cmdb:read'));
CREATE POLICY cis_insert ON cis FOR INSERT TO servicelater_app WITH CHECK (app_can('cmdb:manage'));
CREATE POLICY cis_update ON cis FOR UPDATE TO servicelater_app USING (app_can('cmdb:manage')) WITH CHECK (app_can('cmdb:manage'));
CREATE POLICY cis_delete ON cis FOR DELETE TO servicelater_app USING (app_can('cmdb:manage'));

ALTER TABLE ci_relationships ENABLE ROW LEVEL SECURITY;
CREATE POLICY ci_relationships_select ON ci_relationships FOR SELECT TO servicelater_app USING (app_can('cmdb:read'));
CREATE POLICY ci_relationships_insert ON ci_relationships FOR INSERT TO servicelater_app WITH CHECK (app_can('cmdb:manage'));
CREATE POLICY ci_relationships_delete ON ci_relationships FOR DELETE TO servicelater_app USING (app_can('cmdb:manage'));

ALTER TABLE task_cis ENABLE ROW LEVEL SECURITY;
CREATE POLICY task_cis_select ON task_cis FOR SELECT TO servicelater_app
    USING (app_can('cmdb:read') AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));
CREATE POLICY task_cis_insert ON task_cis FOR INSERT TO servicelater_app
    WITH CHECK (app_can('cmdb:read') AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id AND app_can_record('update', t.type)));
CREATE POLICY task_cis_delete ON task_cis FOR DELETE TO servicelater_app
    USING (app_can('cmdb:read') AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id AND app_can_record('update', t.type)));

-- Linking or unlinking an item changes what a record shows: tell its watchers through the ticket channel, with the
-- routing fields the task trigger sends.
-- +goose StatementBegin
CREATE FUNCTION task_cis_notify() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM pg_notify('ticket_changes', json_build_object(
        'op', 'UPDATE', 'id', t.id, 'type', t.type, 'number', t.number, 'status', t.status,
        'requester_id', t.requester_id, 'assignee_id', t.assignee_id)::text)
    FROM tasks t WHERE t.id = coalesce(NEW.task_id, OLD.task_id);
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER task_cis_notify AFTER INSERT OR DELETE ON task_cis
    FOR EACH ROW EXECUTE FUNCTION task_cis_notify();

-- Open item pages refetch when an item or its relationships change. Only those who read the CMDB are told (routing
-- in internal/realtime); the event carries the item's id only.
-- +goose StatementBegin
CREATE FUNCTION cis_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_TABLE_NAME = 'ci_relationships' THEN
        PERFORM pg_notify('ci_changes', json_build_object('id', coalesce(NEW.from_ci, OLD.from_ci))::text);
        PERFORM pg_notify('ci_changes', json_build_object('id', coalesce(NEW.to_ci, OLD.to_ci))::text);
    ELSE
        PERFORM pg_notify('ci_changes', json_build_object('id', coalesce(NEW.id, OLD.id))::text);
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER cis_notify AFTER INSERT OR UPDATE OR DELETE ON cis FOR EACH ROW EXECUTE FUNCTION cis_notify();
CREATE TRIGGER ci_relationships_notify AFTER INSERT OR DELETE ON ci_relationships FOR EACH ROW EXECUTE FUNCTION cis_notify();

-- Agents read the CMDB; admins manage it (and the cmdb app's cmdb-manager role grants that too).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,freezes:manage,groups:manage,records:create:*,records:delete:*,records:read:*,records:update:*,roles:manage,users:manage,users:read,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
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
         '{calendars:manage,comments:delete,comments:internal,freezes:manage,groups:manage,records:create:*,records:delete:*,records:read:*,records:update:*,roles:manage,users:manage,users:read,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{comments:internal,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();
DROP TRIGGER ci_relationships_notify ON ci_relationships;
DROP TRIGGER cis_notify ON cis;
DROP FUNCTION cis_notify();
DROP TRIGGER task_cis_notify ON task_cis;
DROP FUNCTION task_cis_notify();
DROP TABLE task_cis;
DROP TABLE ci_relationships;
DROP TABLE cis;
