-- +goose Up

-- Credentials move out of users: the directory policy lets any authenticated user read users rows,
-- which would expose password hashes. user_credentials has RLS enabled with no policies and no grants
-- for servicelater_app, so only the owner role (db.WithSystem) can touch it.
-- Dropping users.password_hash here is deliberate and predates any release (no deployed instances).
CREATE TABLE user_credentials (
    user_id       uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    password_hash text NOT NULL,
    updated_at    timestamptz NOT NULL DEFAULT now()
);
INSERT INTO user_credentials (user_id, password_hash) SELECT id, password_hash FROM users WHERE password_hash IS NOT NULL;
ALTER TABLE users DROP COLUMN password_hash;
ALTER TABLE user_credentials ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON user_credentials FROM servicelater_app;

-- Refresh-token sessions (§5.2). Unlogged: lost on a crash, users simply log in again.
-- One row per issued refresh token; rotation inserts a new row in the same family.
CREATE UNLOGGED TABLE sessions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id  uuid NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    rotated_at timestamptz
);
CREATE INDEX sessions_family_idx ON sessions (family_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);
ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON sessions FROM servicelater_app;

CREATE UNLOGGED TABLE rate_limits (
    key          text PRIMARY KEY,
    count        integer NOT NULL,
    window_start timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE rate_limits ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON rate_limits FROM servicelater_app;

-- +goose Down
DROP TABLE rate_limits;
DROP TABLE sessions;
ALTER TABLE users ADD COLUMN password_hash text;
UPDATE users u SET password_hash = c.password_hash FROM user_credentials c WHERE c.user_id = u.id;
DROP TABLE user_credentials;
