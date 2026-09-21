-- +goose Up

-- Installed apps and their definitions (docs/apps-platform.md, §9). These are configuration, not user data:
-- only the owner role (db.WithSystem) touches them, so RLS is on with no policies and no grants for the app role.
CREATE TABLE apps (
    name         text PRIMARY KEY,
    version      text NOT NULL,
    manifest     jsonb NOT NULL,
    installed_by text NOT NULL DEFAULT '',
    installed_at timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Definitions are immutable: an update inserts a new version and moves the `current` flag.
-- Deleting an app removes everything it owns, and nothing else.
CREATE TABLE app_definitions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    app_name   text NOT NULL REFERENCES apps (name) ON DELETE CASCADE,
    kind       text NOT NULL,
    name       text NOT NULL,
    version    integer NOT NULL,
    checksum   text NOT NULL,
    body       jsonb NOT NULL,
    current    boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (app_name, kind, name, version)
);
CREATE UNIQUE INDEX app_definitions_current_idx ON app_definitions (app_name, kind, name) WHERE current;

ALTER TABLE apps ENABLE ROW LEVEL SECURITY;
ALTER TABLE app_definitions ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON apps, app_definitions FROM servicelater_app;

-- Statuses now come from a lifecycle definition instead of a fixed list in the schema.
-- Core logic works on the category of the status, stored on the row so it keeps working even if the app that
-- defined the lifecycle is later removed.
ALTER TABLE tickets DROP CONSTRAINT tickets_status_check;
ALTER TABLE tickets ADD COLUMN state_category text NOT NULL DEFAULT 'open'
    CHECK (state_category IN ('open', 'waiting', 'done', 'closed'));

-- Backfill without firing the touch/notify trigger: this is not a change anyone should be told about.
ALTER TABLE tickets DISABLE TRIGGER tickets_touch_and_notify;
UPDATE tickets SET state_category = CASE status
    WHEN 'on_hold'  THEN 'waiting'
    WHEN 'resolved' THEN 'done'
    WHEN 'closed'   THEN 'closed'
    ELSE 'open' END;
ALTER TABLE tickets ENABLE TRIGGER tickets_touch_and_notify;

CREATE INDEX tickets_state_category_idx ON tickets (state_category);

-- +goose Down
DROP INDEX tickets_state_category_idx;
ALTER TABLE tickets DROP COLUMN state_category;
ALTER TABLE tickets ADD CONSTRAINT tickets_status_check
    CHECK (status IN ('new', 'in_progress', 'on_hold', 'resolved', 'closed'));
DROP TABLE app_definitions;
DROP TABLE apps;
