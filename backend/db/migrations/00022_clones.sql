-- +goose Up
-- Clone profiles and packages (docs/backup-and-clone.md §2, internal/clone). A profile says what a clone of this
-- instance takes; a package is one made from it, encrypted with a passphrase the admin chose, kept in a folder or S3
-- until someone downloads it. Packages are a record like snapshots: never in a snapshot or a clone, never emptied by a
-- restore, and they name people by id and name with no foreign key.

CREATE TABLE clone_profiles (
    -- A short name (uat, dev-small): a scheduled pull asks for a profile by it.
    name        text PRIMARY KEY CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,39}$'),
    description text NOT NULL DEFAULT '',
    -- What it takes (clone.Profile): record types and filters, what comes with them, people, masking.
    config      jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE clone_profiles ENABLE ROW LEVEL SECURITY;
CREATE POLICY clone_profiles_all ON clone_profiles FOR ALL TO servicelater_app
    USING (app_can('instance:clone')) WITH CHECK (app_can('instance:clone'));

CREATE TABLE clone_packages (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The profile it was made from, and that profile as it was then.
    profile           text NOT NULL,
    config            jsonb NOT NULL,
    status            text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'ready', 'failed')),
    error             text NOT NULL DEFAULT '',
    destination       text NOT NULL CHECK (destination IN ('folder', 's3')),
    key               text NOT NULL DEFAULT '' CHECK (key ~ '^[A-Za-z0-9/._-]*$'),
    size              bigint NOT NULL DEFAULT 0,
    -- The passphrase it is encrypted with, sealed under the secrets key until the worker has used it, then cleared.
    passphrase_key_id text NOT NULL DEFAULT '',
    passphrase_nonce  bytea,
    passphrase        bytea,
    taken_at          timestamptz,
    schema_version    bigint,
    rows              bigint NOT NULL DEFAULT 0,
    -- What it holds and what it left out (clone.Report).
    report            jsonb,
    requested_by      uuid,
    requested_by_name text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    finished_at       timestamptz,
    heartbeat_at      timestamptz
);
CREATE INDEX clone_packages_created_idx ON clone_packages (created_at DESC);
CREATE INDEX clone_packages_pending_idx ON clone_packages (created_at) WHERE status = 'pending';
ALTER TABLE clone_packages ENABLE ROW LEVEL SECURITY;
REVOKE INSERT, UPDATE, DELETE ON clone_packages FROM servicelater_app;
-- The sealed passphrase is never readable by people, even admins.
REVOKE SELECT ON clone_packages FROM servicelater_app;
GRANT SELECT (id, profile, config, status, error, destination, key, size, taken_at, schema_version, rows, report,
    requested_by, requested_by_name, created_at, finished_at, heartbeat_at) ON clone_packages TO servicelater_app;
CREATE POLICY clone_packages_select ON clone_packages FOR SELECT TO servicelater_app USING (app_can('instance:clone'));

-- A scheduled snapshot remembers the time it was due, so the schedule compares due times with due times: comparing
-- them with created_at (the database's clock) queued one twice whenever the database's clock was behind the worker's.
ALTER TABLE snapshots ADD COLUMN scheduled_for timestamptz;

-- Admins make clone profiles and packages (instance:clone).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,catalog:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,forms:manage,freezes:manage,groups:manage,instance:backup,instance:clone,knowledge:publish,knowledge:read_internal,knowledge:write,query:run,records:create:*,records:delete:*,records:read:*,records:update:*,reports:read,restricted:read,roles:manage,scripts:edit,slas:manage,settings:manage,users:manage,users:read,workflows:edit,workflows:read_all,workflows:run}'),
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
         '{calendars:manage,catalog:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,forms:manage,freezes:manage,groups:manage,instance:backup,knowledge:publish,knowledge:read_internal,knowledge:write,query:run,records:create:*,records:delete:*,records:read:*,records:update:*,reports:read,restricted:read,roles:manage,scripts:edit,slas:manage,settings:manage,users:manage,users:read,workflows:edit,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,reports:read,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();
DROP TABLE clone_packages;
DROP TABLE clone_profiles;
ALTER TABLE snapshots DROP COLUMN scheduled_for;
