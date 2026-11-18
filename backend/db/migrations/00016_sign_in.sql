-- +goose Up
-- Signing in other than with a password: API keys for service accounts, and single sign-on (docs/auth.md).

-- API keys (docs/auth.md): integrations call the API with `Authorization: Bearer slk_...`, as a service
-- account, never as a person. A service account is a users row (so roles, groups, RLS and history work unchanged)
-- that cannot sign in: it has no password, and a password cannot be set for it.
CREATE TABLE service_accounts (
    user_id     uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    description text NOT NULL DEFAULT '' CHECK (length(description) <= 500),
    created_by  uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE service_accounts ENABLE ROW LEVEL SECURITY;

-- Everyone signed in may know an account is a service account (the UI marks it), like the user directory.
CREATE POLICY service_accounts_select ON service_accounts FOR SELECT TO servicelater_app USING (true);
CREATE POLICY service_accounts_insert ON service_accounts FOR INSERT TO servicelater_app WITH CHECK (app_can('users:manage'));
CREATE POLICY service_accounts_update ON service_accounts FOR UPDATE TO servicelater_app
    USING (app_can('users:manage')) WITH CHECK (app_can('users:manage'));

-- A service account has no mailbox: its address is made up, so it is never emailed.
-- +goose StatementBegin
CREATE FUNCTION service_accounts_quiet() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE users SET email_notifications = false WHERE id = NEW.user_id;
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER service_accounts_quiet AFTER INSERT ON service_accounts FOR EACH ROW EXECUTE FUNCTION service_accounts_quiet();

-- The keys. Only a hash of each is kept (SHA-256: keys are 256 random bits, so a slow hash adds nothing), with its
-- first characters so people can tell keys apart. Owner-only, like sessions: the app role reaches it only through the
-- functions below.
CREATE TABLE api_keys (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    prefix       text NOT NULL,
    secret_hash  bytea NOT NULL UNIQUE,
    -- NULL: everything the account may do. Otherwise the permissions it is narrowed to (app_narrow_permissions).
    scopes       text[],
    created_by   uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    last_used_at timestamptz,
    revoked_at   timestamptz
);
CREATE INDEX api_keys_user_idx ON api_keys (user_id);
ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;

-- Those who manage people read everything about keys but the hash; all writes go through the functions below.
REVOKE ALL ON api_keys FROM servicelater_app;
GRANT SELECT (id, user_id, name, prefix, scopes, created_by, created_at, expires_at, last_used_at, revoked_at)
    ON api_keys TO servicelater_app;
CREATE POLICY api_keys_select ON api_keys FOR SELECT TO servicelater_app USING (app_can('users:manage'));

-- What permissions are left of perms once narrowed to scopes. A scope the holder has is kept; records:<verb>:<type>
-- is kept when they may do it to every type; records:<verb>:* keeps only the record permissions of that verb they
-- have. Never more than perms allow. authz.Narrow is the same in Go (TestNarrowingMatchesTheDatabase).
-- +goose StatementBegin
CREATE FUNCTION app_narrow_permissions(p_perms text[], p_scopes text[]) RETURNS text[]
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
    SELECT coalesce(array_agg(DISTINCT x ORDER BY x), '{}')
    FROM (
        SELECT s AS x FROM unnest(p_scopes) s
         WHERE s = ANY (p_perms)
            OR (s LIKE 'records:%:%' AND split_part(s, ':', 3) <> '*'
                AND 'records:' || split_part(s, ':', 2) || ':*' = ANY (p_perms))
        UNION
        SELECT p FROM unnest(p_scopes) s
          JOIN unnest(p_perms) p ON s LIKE 'records:%:*' AND split_part(s, ':', 3) = '*'
                                AND p LIKE 'records:' || split_part(s, ':', 2) || ':%'
    ) y
$$;
-- +goose StatementEnd

-- db.WithUser for a request made with a narrowed key: as app_begin, then the permissions narrowed. It can only take
-- permissions away, so anyone may call it.
-- +goose StatementBegin
CREATE FUNCTION app_begin_key(p_user uuid, p_scopes text[]) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    PERFORM app_begin(p_user);
    IF p_scopes IS NOT NULL THEN
        PERFORM set_config('app.permissions',
            ',' || array_to_string(app_narrow_permissions(app_permissions_of(p_user), p_scopes), ',') || ',', true);
    END IF;
END
$$;
-- +goose StatementEnd

-- Whether the caller (app.permissions) covers a permission: the same, or the wildcard over its record type.
-- +goose StatementBegin
CREATE FUNCTION app_can_cover(p_perm text) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT app_can(p_perm)
        OR (p_perm LIKE 'records:%:%' AND app_can('records:' || split_part(p_perm, ':', 2) || ':*'))
$$;
-- +goose StatementEnd

-- Making a key: by those who manage people, for an active service account, and never for more than the maker can do
-- themselves (or a group manager could mint a key for an account that holds roles:manage). Returns the key's id.
-- +goose StatementBegin
CREATE FUNCTION app_create_api_key(p_user uuid, p_name text, p_prefix text, p_hash bytea, p_scopes text[], p_expires timestamptz)
    RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    perm text;
    id uuid;
BEGIN
    IF NOT app_can('users:manage') THEN
        RAISE EXCEPTION 'making an API key needs the users:manage permission' USING ERRCODE = '42501';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM service_accounts s JOIN users u ON u.id = s.user_id
                   WHERE s.user_id = p_user AND u.deactivated_at IS NULL) THEN
        RAISE EXCEPTION 'API keys belong to active service accounts' USING ERRCODE = '22023';
    END IF;
    FOREACH perm IN ARRAY CASE WHEN p_scopes IS NULL THEN app_permissions_of(p_user)
                               ELSE app_narrow_permissions(app_permissions_of(p_user), p_scopes) END LOOP
        IF NOT app_can_cover(perm) THEN
            RAISE EXCEPTION 'the key could do something you cannot (%): narrow it, or ask someone who can', perm
                USING ERRCODE = '42501';
        END IF;
    END LOOP;
    INSERT INTO api_keys (user_id, name, prefix, secret_hash, scopes, created_by, expires_at)
    VALUES (p_user, p_name, p_prefix, p_hash, p_scopes, app_current_user_id(), p_expires)
    RETURNING api_keys.id INTO id;
    RETURN id;
END
$$;
-- +goose StatementEnd

-- Revoking is immediate: every request looks its key up. Returns false if there was no such key on that account.
-- +goose StatementBegin
CREATE FUNCTION app_revoke_api_key(p_user uuid, p_id uuid) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT app_can('users:manage') THEN
        RAISE EXCEPTION 'revoking an API key needs the users:manage permission' USING ERRCODE = '42501';
    END IF;
    UPDATE api_keys SET revoked_at = coalesce(revoked_at, now()) WHERE id = p_id AND user_id = p_user;
    RETURN FOUND;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_create_api_key(uuid, text, text, bytea, text[], timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_create_api_key(uuid, text, text, bytea, text[], timestamptz) TO servicelater_app;

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
DROP FUNCTION app_revoke_api_key(uuid, uuid);
DROP FUNCTION app_create_api_key(uuid, text, text, bytea, text[], timestamptz);
DROP FUNCTION app_can_cover(text);
DROP FUNCTION app_begin_key(uuid, text[]);
DROP FUNCTION app_narrow_permissions(text[], text[]);
DROP TABLE api_keys;
DROP TRIGGER service_accounts_quiet ON service_accounts;
DROP TABLE service_accounts;
DROP FUNCTION service_accounts_quiet();
