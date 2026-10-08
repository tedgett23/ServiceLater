-- +goose Up

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

CREATE FUNCTION app_clear_secret(p_name text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT app_can('settings:manage') THEN
        RAISE EXCEPTION 'only those who manage settings may clear a secret' USING ERRCODE = '42501';
    END IF;
    DELETE FROM secrets WHERE name = p_name;
END $$;

-- Whether each secret is set, by whom and when: never its value.
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

-- Each person decides whether notifications also reach them by email (their own row; users_update allows it).
ALTER TABLE users ADD COLUMN email_notifications boolean NOT NULL DEFAULT true;

-- One row per email: queued when a notification is, then sent, failed or skipped by the worker (notify.email). It
-- holds who and what kind, never the text, which the worker fills in as the recipient would see it.
CREATE TABLE email_deliveries (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    notification_id uuid REFERENCES notifications (id) ON DELETE SET NULL,
    user_id         uuid REFERENCES users (id) ON DELETE CASCADE,
    to_address      text NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('notification', 'test')),
    -- what is the notification's template (or its kind, approval_pending), kept here because only a notification's
    -- recipient may read the notification itself.
    what            text NOT NULL DEFAULT '',
    status          text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sent', 'failed', 'skipped')),
    detail          text NOT NULL DEFAULT '' CHECK (length(detail) <= 2000),
    attempts        integer NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX email_deliveries_created_idx ON email_deliveries (created_at DESC);
ALTER TABLE email_deliveries ENABLE ROW LEVEL SECURITY;
CREATE POLICY email_deliveries_select ON email_deliveries FOR SELECT TO servicelater_app USING (app_can('settings:manage'));

-- A notification is also an email when email is on, the recipient wants email and has an address, and the notification
-- asks for it: a template whose channels include email, or a pending approval while email.approvals is not off. The
-- delivery row and its job are written in the same transaction as the notification, so neither exists without the
-- other.
-- +goose StatementBegin
CREATE FUNCTION notifications_email() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    addr text;
    delivery uuid;
BEGIN
    IF (SELECT value FROM settings WHERE name = 'email.enabled') IS DISTINCT FROM 'true'::jsonb THEN
        RETURN NULL;
    END IF;
    IF NEW.kind = 'template' THEN
        IF NOT EXISTS (SELECT 1 FROM app_definitions d WHERE d.app_name = NEW.app AND d.kind = 'notification'
                          AND d.name = NEW.template AND d.current AND d.body -> 'channels' ? 'email') THEN
            RETURN NULL;
        END IF;
    ELSIF NEW.kind = 'approval_pending' THEN
        IF (SELECT value FROM settings WHERE name = 'email.approvals') = 'false'::jsonb THEN
            RETURN NULL;
        END IF;
    ELSE
        RETURN NULL;
    END IF;
    SELECT u.email INTO addr FROM users u
     WHERE u.id = NEW.user_id AND u.email_notifications AND u.deactivated_at IS NULL AND u.app_name IS NULL AND u.email <> '';
    IF addr IS NULL THEN
        RETURN NULL;
    END IF;
    INSERT INTO email_deliveries (notification_id, user_id, to_address, kind, what)
    VALUES (NEW.id, NEW.user_id, addr, 'notification', coalesce(NEW.template, NEW.kind)) RETURNING id INTO delivery;
    PERFORM enqueue_job('notify.email', json_build_object('delivery_id', delivery)::jsonb, now(), 'email:' || delivery);
    RETURN NULL;
END $$;

-- Open delivery logs refresh as emails go out. Routed to those who manage settings (internal/realtime).
CREATE FUNCTION email_deliveries_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('email_changes', json_build_object('id', NEW.id)::text);
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER notifications_email AFTER INSERT ON notifications FOR EACH ROW EXECUTE FUNCTION notifications_email();
CREATE TRIGGER email_deliveries_notify AFTER INSERT OR UPDATE ON email_deliveries FOR EACH ROW EXECUTE FUNCTION email_deliveries_notify();

-- Admins manage settings.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,freezes:manage,groups:manage,records:create:*,records:delete:*,records:read:*,records:update:*,roles:manage,settings:manage,users:manage,users:read,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
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
         '{calendars:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,freezes:manage,groups:manage,records:create:*,records:delete:*,records:read:*,records:update:*,roles:manage,users:manage,users:read,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();
DROP TRIGGER email_deliveries_notify ON email_deliveries;
DROP FUNCTION email_deliveries_notify();
DROP TRIGGER notifications_email ON notifications;
DROP FUNCTION notifications_email();
DROP TABLE email_deliveries;
ALTER TABLE users DROP COLUMN email_notifications;
DROP FUNCTION app_secret_status();
DROP FUNCTION app_clear_secret(text);
DROP FUNCTION app_set_secret(text, text, bytea, bytea);
DROP TABLE secrets;
DROP TABLE settings;
