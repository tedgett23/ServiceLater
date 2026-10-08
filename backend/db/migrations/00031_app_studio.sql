-- +goose Up

-- Each installed app's own files (app.yaml, README.md, default/ and local/), kept at every install, so the app can be
-- compiled again with a change made in ServiceLater, whether it was bundled or installed from a folder or archive.
-- Only the installer (the system) reads and writes them.
CREATE TABLE app_files (
    app_name text NOT NULL REFERENCES apps (name) ON DELETE CASCADE,
    path     text NOT NULL,
    content  bytea NOT NULL,
    PRIMARY KEY (app_name, path)
);
ALTER TABLE app_files ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON app_files FROM servicelater_app;

-- The local layer edited in ServiceLater itself (docs/apps-platform.md §4.5, the "studio" layer): override files keyed
-- like an app's local/ folder (workflows/cab.yaml). Applied over the shipped files and any on disk, at every install
-- and sync; upgrades never touch them. Kept even when the app is uninstalled, so reinstalling it keeps the edits.
CREATE TABLE app_overrides (
    app_name   text NOT NULL CHECK (app_name ~ '^[a-z][a-z0-9-]{1,62}$'),
    path       text NOT NULL CHECK (path ~ '^[a-z]+/[a-z0-9][a-z0-9_.-]*\.yaml$'),
    content    text NOT NULL CHECK (length(content) <= 262144),
    updated_by uuid REFERENCES users (id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app_name, path)
);
ALTER TABLE app_overrides ENABLE ROW LEVEL SECURITY;
CREATE POLICY app_overrides_select ON app_overrides FOR SELECT TO servicelater_app
    USING (app_can('workflows:edit') OR app_can('workflows:read_all'));
CREATE POLICY app_overrides_insert ON app_overrides FOR INSERT TO servicelater_app WITH CHECK (app_can('workflows:edit'));
CREATE POLICY app_overrides_update ON app_overrides FOR UPDATE TO servicelater_app
    USING (app_can('workflows:edit')) WITH CHECK (app_can('workflows:edit'));
CREATE POLICY app_overrides_delete ON app_overrides FOR DELETE TO servicelater_app USING (app_can('workflows:edit'));

-- Admins edit workflows in the builder.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,freezes:manage,groups:manage,knowledge:publish,knowledge:read_internal,knowledge:write,records:create:*,records:delete:*,records:read:*,records:update:*,roles:manage,settings:manage,users:manage,users:read,workflows:edit,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
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
         '{calendars:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,freezes:manage,groups:manage,knowledge:publish,knowledge:read_internal,knowledge:write,records:create:*,records:delete:*,records:read:*,records:update:*,roles:manage,settings:manage,users:manage,users:read,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();
DROP TABLE app_overrides;
DROP TABLE app_files;
