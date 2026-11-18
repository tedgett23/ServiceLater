-- +goose Up
-- Instance settings and encrypted secrets (docs/apps-platform.md §11.2).

-- Instance settings: configuration admins change at run time (the mail server first). The names and the type of each
-- value are fixed in Go (internal/settings); the table only stores them. settings:manage reads and writes them.
CREATE TABLE settings (
    name       text PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$'),
    value      jsonb NOT NULL,
    updated_by uuid REFERENCES users (id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE settings ENABLE ROW LEVEL SECURITY;

CREATE POLICY settings_select ON settings FOR SELECT TO servicelater_app USING (app_can('settings:manage'));

CREATE POLICY settings_insert ON settings FOR INSERT TO servicelater_app WITH CHECK (app_can('settings:manage'));

CREATE POLICY settings_update ON settings FOR UPDATE TO servicelater_app
    USING (app_can('settings:manage')) WITH CHECK (app_can('settings:manage'));

CREATE POLICY settings_delete ON settings FOR DELETE TO servicelater_app USING (app_can('settings:manage'));

-- Secrets, encrypted in Go (internal/secrets: AES-256-GCM, the key never in the database) before they get here. Like
-- the credential tables, the app role has no access at all: an admin sets or clears one through the functions below
-- and can never read it back; only the worker (db.WithSystem) reads the ciphertext, to decrypt it where it is used.
CREATE TABLE secrets (
    name       text PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$'),
    key_id     text NOT NULL CHECK (key_id ~ '^[0-9a-f]{16}$'),
    nonce      bytea NOT NULL,
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) <= 16384),
    updated_by uuid REFERENCES users (id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE secrets ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON secrets FROM servicelater_app;

-- +goose StatementBegin
CREATE FUNCTION app_set_secret(p_name text, p_key_id text, p_nonce bytea, p_ciphertext bytea) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT app_can('settings:manage') THEN
        RAISE EXCEPTION 'only those who manage settings may set a secret' USING ERRCODE = '42501';
    END IF;
    INSERT INTO secrets (name, key_id, nonce, ciphertext, updated_by)
    VALUES (p_name, p_key_id, p_nonce, p_ciphertext, app_current_user_id())
    ON CONFLICT (name) DO UPDATE SET key_id = EXCLUDED.key_id, nonce = EXCLUDED.nonce, ciphertext = EXCLUDED.ciphertext,
        updated_by = EXCLUDED.updated_by, updated_at = now();
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_clear_secret(p_name text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT app_can('settings:manage') THEN
        RAISE EXCEPTION 'only those who manage settings may clear a secret' USING ERRCODE = '42501';
    END IF;
    DELETE FROM secrets WHERE name = p_name;
END $$;
-- +goose StatementEnd

-- Whether each secret is set, by whom and when: never its value.
-- +goose StatementBegin
CREATE FUNCTION app_secret_status() RETURNS TABLE (name text, key_id text, updated_at timestamptz, updated_by_name text)
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT app_can('settings:manage') THEN
        RAISE EXCEPTION 'only those who manage settings may see secrets' USING ERRCODE = '42501';
    END IF;
    RETURN QUERY SELECT s.name, s.key_id, s.updated_at, coalesce(u.name, '') FROM secrets s LEFT JOIN users u ON u.id = s.updated_by ORDER BY s.name;
END $$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION app_set_secret(text, text, bytea, bytea), app_clear_secret(text), app_secret_status() FROM PUBLIC;

GRANT EXECUTE ON FUNCTION app_set_secret(text, text, bytea, bytea), app_clear_secret(text), app_secret_status() TO servicelater_app;

-- +goose Down
DROP FUNCTION app_secret_status();
DROP FUNCTION app_clear_secret(text);
DROP FUNCTION app_set_secret(text, text, bytea, bytea);
DROP TABLE secrets;
DROP TABLE settings;
