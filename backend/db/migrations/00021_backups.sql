-- +goose Up
-- Snapshots and restores (docs/backup-and-clone.md §1, internal/backup). These two tables are the record of what was
-- taken and restored, so they are never in a snapshot and a restore never empties them: restoring last week's
-- snapshot must not forget the snapshots taken since, or that it ran. For the same reason they name people by id and
-- name with no foreign key (a restore replaces the users table under them).

CREATE TABLE snapshots (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- scheduled, manual (Take a snapshot now, or the CLI) or uploaded (from another instance, to restore).
    kind                text NOT NULL CHECK (kind IN ('scheduled', 'manual', 'uploaded')),
    status              text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'ready', 'failed')),
    error               text NOT NULL DEFAULT '',
    -- Where the file is: a folder (SL_SNAPSHOTS_DIR) or the S3 bucket of Admin → Storage; key is its name there.
    destination         text NOT NULL CHECK (destination IN ('folder', 's3')),
    key                 text NOT NULL DEFAULT '' CHECK (key ~ '^[A-Za-z0-9/._-]*$'),
    size                bigint NOT NULL DEFAULT 0,
    include_attachments boolean NOT NULL DEFAULT false,
    -- From the snapshot's header once it is written (or read, for an upload).
    taken_at            timestamptz,
    schema_version      bigint,
    key_id              text NOT NULL DEFAULT '',
    instance            text NOT NULL DEFAULT '',
    rows                bigint NOT NULL DEFAULT 0,
    attachment_files    int NOT NULL DEFAULT 0,
    requested_by        uuid,
    requested_by_name   text NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL DEFAULT now(),
    finished_at         timestamptz,
    -- While running, the worker taking it says it is alive; one silent for a minute is taken to have died.
    heartbeat_at        timestamptz
);
CREATE INDEX snapshots_created_idx ON snapshots (created_at DESC);
CREATE INDEX snapshots_pending_idx ON snapshots (created_at) WHERE status = 'pending';
ALTER TABLE snapshots ENABLE ROW LEVEL SECURITY;
REVOKE INSERT, UPDATE, DELETE ON snapshots FROM servicelater_app;
CREATE POLICY snapshots_select ON snapshots FOR SELECT TO servicelater_app USING (app_can('instance:backup'));

CREATE TABLE snapshot_restores (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    snapshot_id       uuid NOT NULL REFERENCES snapshots (id) ON DELETE CASCADE,
    status            text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'done', 'failed')),
    error             text NOT NULL DEFAULT '',
    -- A snapshot from an older schema: the database is migrated to it, loaded and migrated up again.
    change_schema     boolean NOT NULL DEFAULT false,
    requested_by      uuid,
    requested_by_name text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    started_at        timestamptz,
    finished_at       timestamptz,
    heartbeat_at      timestamptz
);
CREATE INDEX snapshot_restores_created_idx ON snapshot_restores (created_at DESC);
-- One restore at a time: a second is refused while one is pending or running.
CREATE UNIQUE INDEX snapshot_restores_one_idx ON snapshot_restores ((true)) WHERE status IN ('pending', 'running');
ALTER TABLE snapshot_restores ENABLE ROW LEVEL SECURITY;
REVOKE INSERT, UPDATE, DELETE ON snapshot_restores FROM servicelater_app;
CREATE POLICY snapshot_restores_select ON snapshot_restores FOR SELECT TO servicelater_app USING (app_can('instance:backup'));

-- Admins take and restore snapshots (instance:backup).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,catalog:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,forms:manage,freezes:manage,groups:manage,instance:backup,knowledge:publish,knowledge:read_internal,knowledge:write,query:run,records:create:*,records:delete:*,records:read:*,records:update:*,reports:read,restricted:read,roles:manage,scripts:edit,slas:manage,settings:manage,users:manage,users:read,workflows:edit,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,reports:read,users:read,workflows:read_all,workflows:run}'),
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
         '{calendars:manage,catalog:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,forms:manage,freezes:manage,groups:manage,knowledge:publish,knowledge:read_internal,knowledge:write,query:run,records:create:*,records:delete:*,records:read:*,records:update:*,reports:read,restricted:read,roles:manage,scripts:edit,slas:manage,settings:manage,users:manage,users:read,workflows:edit,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,reports:read,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();
DROP TABLE snapshot_restores;
DROP TABLE snapshots;
