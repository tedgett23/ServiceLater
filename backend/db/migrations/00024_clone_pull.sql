-- +goose Up
-- Scheduled pull (docs/backup-and-clone.md §2, "Getting the package to the target"): a UAT or development instance
-- fetches a fresh package from production over its API, on demand or on a schedule.

-- On the source: the keys that may pull a profile. A pull key belongs to one profile and does nothing else (it is not
-- an API key: it signs nobody in), so the key a target keeps can never be more than a way to get that profile's
-- package. Only a hash is kept, as for API keys.
CREATE TABLE clone_pull_keys (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    profile         text NOT NULL REFERENCES clone_profiles (name) ON DELETE CASCADE ON UPDATE CASCADE,
    name            text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    prefix          text NOT NULL,
    secret_hash     bytea NOT NULL UNIQUE,
    created_by_name text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_used_at    timestamptz,
    revoked_at      timestamptz
);
CREATE INDEX clone_pull_keys_profile_idx ON clone_pull_keys (profile);
ALTER TABLE clone_pull_keys ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON clone_pull_keys FROM servicelater_app;
GRANT SELECT (id, profile, name, prefix, created_by_name, created_at, last_used_at, revoked_at) ON clone_pull_keys TO servicelater_app;
GRANT INSERT ON clone_pull_keys TO servicelater_app;
GRANT UPDATE (revoked_at) ON clone_pull_keys TO servicelater_app;
CREATE POLICY clone_pull_keys_all ON clone_pull_keys FOR ALL TO servicelater_app
    USING (app_can('instance:clone')) WITH CHECK (app_can('instance:clone'));

-- On the source: every pull, whether it was given or refused after the key was known. Written as the system; like
-- clone_packages it is in no snapshot or clone and no restore empties it.
CREATE TABLE clone_pulls (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    profile     text NOT NULL,
    key_id      uuid,
    key_name    text NOT NULL DEFAULT '',
    -- running, done or failed (error says why).
    status      text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'done', 'failed')),
    error       text NOT NULL DEFAULT '',
    size        bigint NOT NULL DEFAULT 0,
    rows        bigint NOT NULL DEFAULT 0,
    remote_addr text NOT NULL DEFAULT '',
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    heartbeat_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX clone_pulls_started_idx ON clone_pulls (started_at DESC);
ALTER TABLE clone_pulls ENABLE ROW LEVEL SECURITY;
REVOKE INSERT, UPDATE, DELETE ON clone_pulls FROM servicelater_app;
CREATE POLICY clone_pulls_select ON clone_pulls FOR SELECT TO servicelater_app USING (app_can('instance:clone'));

-- On the target: a load may be pulled from the source (origin pull) instead of uploaded, waiting in status pulling
-- until the worker has fetched it. A pulled package is encrypted on disk with a passphrase derived from the secrets
-- key pull_key_id names, so nobody types one to preview or apply it. A scheduled pull remembers the time it was due.
ALTER TABLE clone_loads DROP CONSTRAINT clone_loads_status_check;
ALTER TABLE clone_loads ADD CONSTRAINT clone_loads_status_check
    CHECK (status IN ('pulling', 'uploaded', 'pending', 'running', 'done', 'failed'));
ALTER TABLE clone_loads ADD COLUMN origin text NOT NULL DEFAULT 'upload' CHECK (origin IN ('upload', 'pull'));
ALTER TABLE clone_loads ADD COLUMN pull_key_id text NOT NULL DEFAULT '';
ALTER TABLE clone_loads ADD COLUMN scheduled_for timestamptz;
-- A pull fetched on the schedule is applied as soon as it is fetched.
ALTER TABLE clone_loads ADD COLUMN apply_when_pulled boolean NOT NULL DEFAULT false;
GRANT SELECT (origin, pull_key_id, scheduled_for, apply_when_pulled) ON clone_loads TO servicelater_app;
-- One pull at a time.
CREATE UNIQUE INDEX clone_loads_one_pull_idx ON clone_loads ((true)) WHERE status = 'pulling';

-- +goose Down
DROP INDEX clone_loads_one_pull_idx;
DELETE FROM clone_loads WHERE status = 'pulling';
ALTER TABLE clone_loads DROP COLUMN apply_when_pulled;
ALTER TABLE clone_loads DROP COLUMN scheduled_for;
ALTER TABLE clone_loads DROP COLUMN pull_key_id;
ALTER TABLE clone_loads DROP COLUMN origin;
ALTER TABLE clone_loads DROP CONSTRAINT clone_loads_status_check;
ALTER TABLE clone_loads ADD CONSTRAINT clone_loads_status_check
    CHECK (status IN ('uploaded', 'pending', 'running', 'done', 'failed'));
DROP TABLE clone_pulls;
DROP TABLE clone_pull_keys;
