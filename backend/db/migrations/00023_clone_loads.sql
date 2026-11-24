-- +goose Up
-- Loading a clone package into this instance (docs/backup-and-clone.md §2, "Applying a clone"; internal/clone).
-- clone_loads is the record of packages uploaded here and applied: like snapshots it is in no snapshot or clone and
-- no restore or clone empties it, and it names people by id and name with no foreign key.

CREATE TABLE clone_loads (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- uploaded (waiting to be applied), pending (asked for), running, done or failed.
    status            text NOT NULL DEFAULT 'uploaded' CHECK (status IN ('uploaded', 'pending', 'running', 'done', 'failed')),
    error             text NOT NULL DEFAULT '',
    destination       text NOT NULL CHECK (destination IN ('folder', 's3')),
    key               text NOT NULL DEFAULT '' CHECK (key ~ '^[A-Za-z0-9/._-]*$'),
    size              bigint NOT NULL DEFAULT 0,
    -- The package's plaintext header: where it came from, which profile, when, at which schema version.
    source            jsonb NOT NULL,
    -- What the target keeps of its own besides what it always keeps.
    keep_studio       boolean NOT NULL DEFAULT true,
    keep_people       boolean NOT NULL DEFAULT true,
    -- The passphrase, sealed under the secrets key from asking until the worker has used it.
    passphrase_key_id text NOT NULL DEFAULT '',
    passphrase_nonce  bytea,
    passphrase        bytea,
    -- What it brought and what the target kept (clone.LoadReport).
    report            jsonb,
    requested_by      uuid,
    requested_by_name text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    started_at        timestamptz,
    finished_at       timestamptz,
    heartbeat_at      timestamptz
);
CREATE INDEX clone_loads_created_idx ON clone_loads (created_at DESC);
-- One at a time.
CREATE UNIQUE INDEX clone_loads_one_idx ON clone_loads ((true)) WHERE status IN ('pending', 'running');
ALTER TABLE clone_loads ENABLE ROW LEVEL SECURITY;
REVOKE INSERT, UPDATE, DELETE ON clone_loads FROM servicelater_app;
REVOKE SELECT ON clone_loads FROM servicelater_app;
GRANT SELECT (id, status, error, destination, key, size, source, keep_studio, keep_people, report, requested_by,
    requested_by_name, created_at, started_at, finished_at, heartbeat_at) ON clone_loads TO servicelater_app;
CREATE POLICY clone_loads_select ON clone_loads FOR SELECT TO servicelater_app USING (app_can('instance:clone'));

-- People this instance keeps when a clone replaces its people (test agents, integration accounts): their passwords,
-- roles, groups, phone numbers and API keys survive, moved to production's id when production has the same person.
CREATE TABLE clone_keep_people (
    user_id  uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    added_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE clone_keep_people ENABLE ROW LEVEL SECURITY;
CREATE POLICY clone_keep_people_all ON clone_keep_people FOR ALL TO servicelater_app
    USING (app_can('instance:clone')) WITH CHECK (app_can('instance:clone'));

-- +goose Down
DROP TABLE clone_keep_people;
DROP TABLE clone_loads;
