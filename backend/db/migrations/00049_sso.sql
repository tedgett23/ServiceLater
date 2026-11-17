-- +goose Up
-- Single sign-on with OpenID Connect (docs/auth.md): Okta, Microsoft Entra ID, Google Workspace, Keycloak and the
-- like. Each provider is a row; its client secret is a secret (sso.<id>.client_secret), never a column. Those who
-- change settings manage them; the sign-in flow reads them as the system, before anyone is known.
CREATE TABLE sso_providers (
    id              text PRIMARY KEY CHECK (id ~ '^[a-z][a-z0-9-]{1,30}$'),
    -- label is the button: "Sign in with <label>".
    label           text NOT NULL CHECK (length(label) BETWEEN 1 AND 60),
    issuer          text NOT NULL CHECK (issuer ~ '^https?://'),
    client_id       text NOT NULL CHECK (length(client_id) BETWEEN 1 AND 500),
    -- Who may sign in by their email's domain (example.com); empty: anyone the provider vouches for.
    allowed_domains text[] NOT NULL DEFAULT '{}',
    -- Whether someone signing in for the first time gets an account (as an end user), or must exist already.
    create_users    boolean NOT NULL DEFAULT true,
    -- The ID token claim listing the person's groups (often "groups"); empty: groups are not synced.
    groups_claim    text NOT NULL DEFAULT '' CHECK (groups_claim ~ '^[A-Za-z0-9_.:/-]{0,100}$'),
    enabled         boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE sso_providers ENABLE ROW LEVEL SECURITY;
CREATE POLICY sso_providers_all ON sso_providers FOR ALL TO servicelater_app
    USING (app_can('settings:manage')) WITH CHECK (app_can('settings:manage'));

-- Group sync: a value of the provider's groups claim, and the ServiceLater group it puts people in. At each sign-in a
-- person is added to the mapped groups whose values they have and taken out of the mapped groups whose values they
-- lack; groups no mapping names are left alone.
CREATE TABLE sso_group_maps (
    provider_id text NOT NULL REFERENCES sso_providers (id) ON DELETE CASCADE,
    claim_value text NOT NULL CHECK (length(claim_value) BETWEEN 1 AND 300),
    group_id    uuid NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    PRIMARY KEY (provider_id, claim_value, group_id)
);
ALTER TABLE sso_group_maps ENABLE ROW LEVEL SECURITY;
CREATE POLICY sso_group_maps_all ON sso_group_maps FOR ALL TO servicelater_app
    USING (app_can('settings:manage')) WITH CHECK (app_can('settings:manage'));

-- Who signed in through which provider, by the provider's stable subject (an email can change; the subject cannot).
-- Written only by the sign-in flow, as the system.
CREATE TABLE user_identities (
    provider_id     text NOT NULL REFERENCES sso_providers (id) ON DELETE CASCADE,
    subject         text NOT NULL,
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    email           text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_sign_in_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_id, subject)
);
CREATE INDEX user_identities_user_idx ON user_identities (user_id);
ALTER TABLE user_identities ENABLE ROW LEVEL SECURITY;
CREATE POLICY user_identities_select ON user_identities FOR SELECT TO servicelater_app
    USING (app_can('users:manage') OR app_can('settings:manage') OR user_id = app_current_user_id());

-- +goose Down
DROP TABLE user_identities;
DROP TABLE sso_group_maps;
DROP TABLE sso_providers;
DELETE FROM secrets WHERE name LIKE 'sso.%';
DELETE FROM settings WHERE name LIKE 'sign_in.%';
