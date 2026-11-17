-- +goose Up
-- More notification channels (docs/apps-platform.md §6.5): SMS to people who verified a mobile number and turned SMS
-- on, and Microsoft Teams posts to a group's channel. A template opts into each channel (channels: [in_app, email,
-- sms, teams]). Deliveries are queued in the transaction that made the notification and sent by the worker, as email
-- already is (email keeps its own table; these two share one).

-- A pending approval's wording is a template too (core-itsm's approval-pending; change it in local/). The row keeps
-- its kind, because what it points at is an approval, not a record its approver may necessarily read.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION notify_on_approval_request() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    INSERT INTO notifications (user_id, kind, approval_id, app, template)
    VALUES (NEW.approver_id, 'approval_pending', NEW.id, 'core-itsm', 'approval-pending');
    PERFORM pg_notify('notification_changes', json_build_object('user_id', NEW.approver_id)::text);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- A person's mobile number, for SMS. Only they set it, and it counts only once they have typed back the code texted
-- to it: the code is kept as a hash, expires, and allows five tries. Owner-only but for a column grant without the
-- code; changes go through the functions below.
CREATE TABLE user_phones (
    user_id           uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    number            text NOT NULL CHECK (number ~ '^\+[1-9][0-9]{7,14}$'),
    verified_at       timestamptz,
    sms_notifications boolean NOT NULL DEFAULT false,
    code_hash         bytea,
    code_expires_at   timestamptz,
    code_attempts     integer NOT NULL DEFAULT 0,
    updated_at        timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE user_phones ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON user_phones FROM servicelater_app;
GRANT SELECT (user_id, number, verified_at, sms_notifications, updated_at) ON user_phones TO servicelater_app;
CREATE POLICY user_phones_select ON user_phones FOR SELECT TO servicelater_app
    USING (user_id = app_current_user_id() OR app_can('users:manage'));

-- +goose StatementBegin
-- A new number, not verified yet, with the code just texted to it (as a hash). SMS stays off until it is verified.
CREATE FUNCTION app_set_phone(p_number text, p_code_hash bytea, p_expires timestamptz) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF app_current_user_id() IS NULL THEN
        RAISE EXCEPTION 'not signed in' USING ERRCODE = '42501';
    END IF;
    INSERT INTO user_phones (user_id, number, code_hash, code_expires_at)
    VALUES (app_current_user_id(), p_number, p_code_hash, p_expires)
    ON CONFLICT (user_id) DO UPDATE SET number = EXCLUDED.number, verified_at = NULL, sms_notifications = false,
        code_hash = EXCLUDED.code_hash, code_expires_at = EXCLUDED.code_expires_at, code_attempts = 0, updated_at = now();
END $$;

-- The code typed back: true and the number verified when it matches in time, within five tries.
CREATE FUNCTION app_verify_phone(p_code_hash bytea) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    ok boolean;
BEGIN
    UPDATE user_phones SET code_attempts = code_attempts + 1
     WHERE user_id = app_current_user_id() AND code_hash IS NOT NULL
    RETURNING code_attempts <= 5 AND code_expires_at > now() AND code_hash = p_code_hash INTO ok;
    IF coalesce(ok, false) THEN
        UPDATE user_phones SET verified_at = now(), sms_notifications = true, code_hash = NULL, code_expires_at = NULL,
            code_attempts = 0, updated_at = now()
         WHERE user_id = app_current_user_id();
    END IF;
    RETURN coalesce(ok, false);
END $$;

CREATE FUNCTION app_set_sms_notifications(p_on boolean) RETURNS boolean
    LANGUAGE sql SECURITY DEFINER SET search_path = public
    AS $$
    UPDATE user_phones SET sms_notifications = p_on, updated_at = now()
     WHERE user_id = app_current_user_id() AND verified_at IS NOT NULL
    RETURNING true
$$;

CREATE FUNCTION app_clear_phone() RETURNS void
    LANGUAGE sql SECURITY DEFINER SET search_path = public
    AS $$ DELETE FROM user_phones WHERE user_id = app_current_user_id() $$;
-- +goose StatementEnd

-- SMS and Teams deliveries: one row per message, its status and what the provider said. An SMS is to a person (and
-- belongs to their notification); a Teams post is to a group's channel, once for the group, and carries what it is
-- about itself (it has no notification row).
CREATE TABLE channel_deliveries (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    channel         text NOT NULL CHECK (channel IN ('sms', 'teams')),
    notification_id uuid REFERENCES notifications (id) ON DELETE SET NULL,
    user_id         uuid REFERENCES users (id) ON DELETE CASCADE,
    group_id        uuid REFERENCES groups (id) ON DELETE CASCADE,
    task_id         uuid REFERENCES tasks (id) ON DELETE CASCADE,
    app             text,
    template        text,
    actor_id        uuid REFERENCES users (id) ON DELETE SET NULL,
    -- to is a phone number (sms) or the group's name (teams), as it was when the message was queued.
    to_address      text NOT NULL,
    kind            text NOT NULL DEFAULT 'notification' CHECK (kind IN ('notification', 'test', 'verification')),
    what            text NOT NULL DEFAULT '',
    status          text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sent', 'failed', 'skipped')),
    detail          text NOT NULL DEFAULT '' CHECK (length(detail) <= 2000),
    attempts        integer NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (channel <> 'teams' OR group_id IS NOT NULL)
);
CREATE INDEX channel_deliveries_created_idx ON channel_deliveries (created_at DESC);
ALTER TABLE channel_deliveries ENABLE ROW LEVEL SECURITY;
CREATE POLICY channel_deliveries_select ON channel_deliveries FOR SELECT TO servicelater_app USING (app_can('settings:manage'));

-- +goose StatementBegin
-- A notification is also an SMS when SMS is on, its template lists sms (a pending approval's is core-itsm's
-- approval-pending), and its recipient has a verified number with SMS turned on.
CREATE FUNCTION notifications_sms() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    phone text;
    delivery uuid;
BEGIN
    IF (SELECT value FROM settings WHERE name = 'sms.enabled') IS DISTINCT FROM 'true'::jsonb
       OR NEW.app IS NULL OR NEW.template IS NULL THEN
        RETURN NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM app_definitions d WHERE d.app_name = NEW.app AND d.kind = 'notification'
                      AND d.name = NEW.template AND d.current AND d.body -> 'channels' ? 'sms') THEN
        RETURN NULL;
    END IF;
    SELECT p.number INTO phone FROM user_phones p JOIN users u ON u.id = p.user_id
     WHERE p.user_id = NEW.user_id AND p.verified_at IS NOT NULL AND p.sms_notifications AND u.deactivated_at IS NULL;
    IF phone IS NULL THEN
        RETURN NULL;
    END IF;
    INSERT INTO channel_deliveries (channel, notification_id, user_id, to_address, what)
    VALUES ('sms', NEW.id, NEW.user_id, phone, NEW.app || '/' || NEW.template) RETURNING id INTO delivery;
    PERFORM enqueue_job('notify.sms', json_build_object('delivery_id', delivery)::jsonb, now(), 'sms:' || delivery);
    RETURN NULL;
END $$;

-- A notify action to a group also posts to the group's Teams channel, once, when Teams is on, the template lists
-- teams and the group has a channel (its webhook secret). Called by records.notifyFromRule in the save's transaction,
-- with what the post is about; app_notify has already checked the caller may notify about the record.
CREATE FUNCTION app_notify_group_channel(p_group uuid, p_task uuid, p_app text, p_template text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    gname text;
    delivery uuid;
BEGIN
    IF (SELECT value FROM settings WHERE name = 'teams.enabled') IS DISTINCT FROM 'true'::jsonb THEN
        RETURN false;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM app_definitions d WHERE d.app_name = p_app AND d.kind = 'notification'
                      AND d.name = p_template AND d.current AND d.body -> 'channels' ? 'teams') THEN
        RETURN false;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM secrets WHERE name = 'teams.group_' || replace(p_group::text, '-', '')) THEN
        RETURN false;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = p_task AND app_sees_task(t.id, t.type, t.requester_id, t.assignee_id, t.assignment_group_id, true)) THEN
        RAISE EXCEPTION 'not allowed to notify about this record' USING ERRCODE = '42501';
    END IF;
    SELECT name INTO gname FROM groups WHERE id = p_group;
    IF gname IS NULL THEN
        RETURN false;
    END IF;
    INSERT INTO channel_deliveries (channel, group_id, task_id, app, template, actor_id, to_address, what)
    VALUES ('teams', p_group, p_task, p_app, p_template, app_current_user_id(), gname, p_app || '/' || p_template)
    RETURNING id INTO delivery;
    PERFORM enqueue_job('notify.teams', json_build_object('delivery_id', delivery)::jsonb, now(), 'teams:' || delivery);
    RETURN true;
END $$;

-- The delivery log refreshes as messages go out, as email's does (routed to those who manage settings).
CREATE FUNCTION channel_deliveries_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('email_changes', json_build_object('id', NEW.id)::text);
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER notifications_sms AFTER INSERT ON notifications FOR EACH ROW EXECUTE FUNCTION notifications_sms();
CREATE TRIGGER channel_deliveries_notify AFTER INSERT OR UPDATE ON channel_deliveries FOR EACH ROW EXECUTE FUNCTION channel_deliveries_notify();
REVOKE ALL ON FUNCTION app_notify_group_channel(uuid, uuid, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_notify_group_channel(uuid, uuid, text, text) TO servicelater_app;

-- +goose Down
DROP TRIGGER channel_deliveries_notify ON channel_deliveries;
DROP TRIGGER notifications_sms ON notifications;
DROP FUNCTION channel_deliveries_notify();
DROP FUNCTION app_notify_group_channel(uuid, uuid, text, text);
DROP FUNCTION notifications_sms();
DROP TABLE channel_deliveries;
DROP FUNCTION app_clear_phone();
DROP FUNCTION app_set_sms_notifications(boolean);
DROP FUNCTION app_verify_phone(bytea);
DROP FUNCTION app_set_phone(text, bytea, timestamptz);
DROP TABLE user_phones;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION notify_on_approval_request() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    INSERT INTO notifications (user_id, kind, approval_id) VALUES (NEW.approver_id, 'approval_pending', NEW.id);
    PERFORM pg_notify('notification_changes', json_build_object('user_id', NEW.approver_id)::text);
    RETURN NULL;
END
$$;
-- +goose StatementEnd
DELETE FROM settings WHERE name LIKE 'sms.%' OR name LIKE 'teams.%';
DELETE FROM secrets WHERE name LIKE 'sms.%' OR name LIKE 'teams.%';
