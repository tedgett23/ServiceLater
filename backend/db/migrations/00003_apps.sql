-- +goose Up
-- Installed apps (docs/apps-platform.md): their manifests and compiled definitions, their own files, the layer edited
-- in ServiceLater itself, and the principal each app acts as.

-- Apps and their definitions are configuration, not user data: only the owner role (db.WithSystem) touches them, so
-- RLS is on with no policies and no grants for the app role.
CREATE TABLE apps (
    name         text PRIMARY KEY,
    version      text NOT NULL,
    manifest     jsonb NOT NULL,
    installed_by text NOT NULL DEFAULT '',
    installed_at timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Definitions are immutable: an update inserts a new version and moves the `current` flag. Deleting an app removes
-- everything it owns, and nothing else.
CREATE TABLE app_definitions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    app_name   text NOT NULL REFERENCES apps (name) ON DELETE CASCADE,
    kind       text NOT NULL,
    name       text NOT NULL,
    version    integer NOT NULL,
    checksum   text NOT NULL,
    body       jsonb NOT NULL,
    current    boolean NOT NULL DEFAULT true,
    -- Which layers produced it: default, default+local, default+local+studio... Informational (the body is already the
    -- merged result), but it answers "why does my screen say this?".
    source     text NOT NULL DEFAULT 'default',
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (app_name, kind, name, version)
);
CREATE UNIQUE INDEX app_definitions_current_idx ON app_definitions (app_name, kind, name) WHERE current;

ALTER TABLE apps ENABLE ROW LEVEL SECURITY;
ALTER TABLE app_definitions ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON apps, app_definitions FROM servicelater_app;

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
-- like an app's local/ folder (workflows/cab.yaml, scripts/triage.star). Applied over the shipped files and any on
-- disk, at every install and sync; upgrades never touch them. Kept even when the app is uninstalled, so reinstalling
-- it keeps the edits.
CREATE TABLE app_overrides (
    app_name   text NOT NULL CHECK (app_name ~ '^[a-z][a-z0-9-]{1,62}$'),
    path       text NOT NULL CHECK (path ~ '^[a-z]+/[a-z0-9][a-z0-9_.-]*\.(yaml|star)$'),
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

-- An app's principal (a users row with app_name), created the first time it is needed (an install, or a test that
-- wiped users). It is not reactivated here: uninstalling an app deactivates its principal, and only installing it
-- again brings it back.
-- +goose StatementBegin
CREATE FUNCTION app_principal(p_app text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    id uuid;
BEGIN
    SELECT u.id INTO id FROM users u WHERE u.app_name = p_app;
    IF id IS NULL THEN
        INSERT INTO users (email, name, role, app_name)
        VALUES (p_app || '@apps.servicelater.invalid', p_app, 'end_user', p_app)
        ON CONFLICT (app_name) DO NOTHING
        RETURNING users.id INTO id;
        IF id IS NULL THEN
            SELECT u.id INTO id FROM users u WHERE u.app_name = p_app;
        END IF;
    END IF;
    RETURN id;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION app_principal(text);
DROP TABLE app_overrides;
DROP TABLE app_files;
DROP TABLE app_definitions;
DROP TABLE apps;
