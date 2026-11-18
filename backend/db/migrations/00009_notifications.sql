-- +goose Up
-- Notifications (docs/apps-platform.md §6.5): in the app, and by email, SMS and Teams; and email in.

-- A notification is a hint that something needs a person's attention. Like a realtime event it names what happened
-- and lets the person fetch it through the normal RLS-protected endpoint, never duplicating record or approval content
-- here. The wording is a template (an app's notification definition), filled in when the recipient reads it, from the
-- record as they are allowed to see it.
CREATE TABLE notifications (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id),
    -- approval_pending (about approval_id) or template (about task_id). A pending approval has a template too
    -- (core-itsm's approval-pending), but keeps its kind: what it points at is an approval, not a record its approver
    -- may necessarily read.
    kind        text NOT NULL,
    approval_id uuid REFERENCES workflow_approvals (id) ON DELETE CASCADE,
    task_id     uuid REFERENCES tasks (id) ON DELETE CASCADE,
    app         text,
    template    text,
    -- Who made the change it is about (the person an app acted for, when it was an app).
    actor_id    uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    read_at     timestamptz,
    CONSTRAINT notifications_template_check
        CHECK (kind <> 'template' OR (task_id IS NOT NULL AND app IS NOT NULL AND template IS NOT NULL))
);
CREATE INDEX notifications_user_idx ON notifications (user_id, created_at DESC);
CREATE INDEX notifications_task_idx ON notifications (task_id, user_id) WHERE task_id IS NOT NULL;

-- Only the recipient may see or mark their own notifications read. Nothing but SECURITY DEFINER code (below, past RLS)
-- ever inserts one, so there is no insert policy.
ALTER TABLE notifications ENABLE ROW LEVEL SECURITY;
CREATE POLICY notifications_select ON notifications FOR SELECT TO servicelater_app
    USING (user_id = app_current_user_id());
CREATE POLICY notifications_update ON notifications FOR UPDATE TO servicelater_app
    USING (user_id = app_current_user_id()) WITH CHECK (user_id = app_current_user_id());

-- A pending approval is a notification to its approver.
-- +goose StatementBegin
CREATE FUNCTION notify_on_approval_request() RETURNS trigger
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
CREATE TRIGGER workflow_approvals_notify AFTER INSERT ON workflow_approvals
    FOR EACH ROW EXECUTE FUNCTION notify_on_approval_request();

-- app_notify delivers one template notification about a ticket. Rules run as the person making the change, who may
-- not write other people's notifications, so this runs past RLS and decides who gets one itself. Whatever the rule
-- asked for, nobody is told:
--   - about their own change;
--   - while deactivated;
--   - about a record they cannot see (as the read policy judges it, restricted groups included), so an end user in a
--     group never hears of someone else's ticket;
--   - when they are the person an app acted for (an async rule set off by their own change). A workflow step is
--     different: it happens later, because someone decided or a task was finished, so the person it runs for is told
--     what it did; the engine marks its transactions (app.workflow_step).
-- The person acting must be able to see the ticket, and the template must be installed. It returns how many were
-- sent. A rule that names a thousand people is a mistake, not a notification.
-- +goose StatementBegin
CREATE FUNCTION app_notify(p_users uuid[], p_task uuid, p_app text, p_template text) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    actor uuid := app_current_user_id();
    behalf uuid := app_on_behalf_of();
    recipient uuid;
    sent integer := 0;
BEGIN
    IF actor IS NULL OR NOT EXISTS (
        SELECT 1 FROM tasks t WHERE t.id = p_task
           AND app_sees_task(t.id, t.type, t.requester_id, t.assignee_id, t.assignment_group_id, true)) THEN
        RAISE EXCEPTION 'not allowed to notify about this record' USING ERRCODE = '42501';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM app_definitions
                   WHERE app_name = p_app AND kind = 'notification' AND name = p_template AND current) THEN
        RAISE EXCEPTION 'no notification template %/%', p_app, p_template USING ERRCODE = '22023';
    END IF;
    IF coalesce(array_length(p_users, 1), 0) > 1000 THEN
        RAISE EXCEPTION 'a notification may go to at most 1000 people' USING ERRCODE = '22023';
    END IF;
    FOR recipient IN
        SELECT DISTINCT u.id
          FROM unnest(p_users) AS r(id)
          JOIN users u ON u.id = r.id
          JOIN tasks t ON t.id = p_task
         WHERE u.id <> actor
           AND (u.id IS DISTINCT FROM behalf OR current_setting('app.workflow_step', true) = 'on')
           AND u.app_name IS NULL
           AND u.deactivated_at IS NULL
           AND app_user_sees_task(u.id, t.type, t.requester_id, t.assignee_id, t.assignment_group_id)
    LOOP
        INSERT INTO notifications (user_id, kind, task_id, app, template, actor_id)
        VALUES (recipient, 'template', p_task, p_app, p_template, coalesce(behalf, actor));
        PERFORM pg_notify('notification_changes', json_build_object('user_id', recipient)::text);
        sent := sent + 1;
    END LOOP;
    RETURN sent;
END
$$;
-- +goose StatementEnd

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
-- +goose StatementEnd

-- Open delivery logs refresh as emails go out. Routed to those who manage settings (internal/realtime).
-- +goose StatementBegin
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

-- A new number, not verified yet, with the code just texted to it (as a hash). SMS stays off until it is verified.
-- +goose StatementBegin
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
-- +goose StatementEnd

-- The code typed back: true and the number verified when it matches in time, within five tries.
-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_set_sms_notifications(p_on boolean) RETURNS boolean
    LANGUAGE sql SECURITY DEFINER SET search_path = public
    AS $$
    UPDATE user_phones SET sms_notifications = p_on, updated_at = now()
     WHERE user_id = app_current_user_id() AND verified_at IS NOT NULL
    RETURNING true
$$;
-- +goose StatementEnd

-- +goose StatementBegin
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

-- A notification is also an SMS when SMS is on, its template lists sms (a pending approval's is core-itsm's
-- approval-pending), and its recipient has a verified number with SMS turned on.
-- +goose StatementBegin
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
-- +goose StatementEnd

-- A notify action to a group also posts to the group's Teams channel, once, when Teams is on, the template lists
-- teams and the group has a channel (its webhook secret). Called by records.notifyFromRule in the save's transaction,
-- with what the post is about; app_notify has already checked the caller may notify about the record.
-- +goose StatementBegin
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
-- +goose StatementEnd

-- The delivery log refreshes as messages go out, as email's does (routed to those who manage settings).
-- +goose StatementBegin
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

-- Email in (docs/apps-platform.md §6.5): the worker reads a mailbox (IMAP, or Microsoft 365 through Graph). A reply to
-- a notification becomes a comment on its record, as the person it was sent to; a new email from an allowed domain,
-- authenticated by DMARC, files an incident. Every message read is logged here, with what was done and why, and its
-- Message-ID so a message is never handled twice.
CREATE TABLE inbound_emails (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id   text NOT NULL UNIQUE,
    from_address text NOT NULL DEFAULT '',
    subject      text NOT NULL DEFAULT '' CHECK (length(subject) <= 1000),
    -- action is what it became: a comment, an incident, or nothing (ignored, refused, failed).
    action       text NOT NULL CHECK (action IN ('comment', 'incident', 'ignored', 'refused', 'failed')),
    detail       text NOT NULL DEFAULT '' CHECK (length(detail) <= 2000),
    task_id      uuid REFERENCES tasks (id) ON DELETE SET NULL,
    user_id      uuid REFERENCES users (id) ON DELETE SET NULL,
    attachments  integer NOT NULL DEFAULT 0,
    -- authentication is what the sender's checks said: dmarc pass, fail or none, and how it was decided.
    authentication text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX inbound_emails_created_idx ON inbound_emails (created_at DESC);
CREATE INDEX inbound_emails_from_idx ON inbound_emails (lower(from_address), created_at DESC);
ALTER TABLE inbound_emails ENABLE ROW LEVEL SECURITY;
CREATE POLICY inbound_emails_select ON inbound_emails FOR SELECT TO servicelater_app USING (app_can('settings:manage'));

-- +goose Down
DROP TABLE inbound_emails;
DROP FUNCTION app_notify_group_channel(uuid, uuid, text, text);
DROP TRIGGER notifications_sms ON notifications;
DROP FUNCTION notifications_sms();
DROP TABLE channel_deliveries;
DROP FUNCTION channel_deliveries_notify();
DROP FUNCTION app_clear_phone();
DROP FUNCTION app_set_sms_notifications(boolean);
DROP FUNCTION app_verify_phone(bytea);
DROP FUNCTION app_set_phone(text, bytea, timestamptz);
DROP TABLE user_phones;
DROP TRIGGER notifications_email ON notifications;
DROP FUNCTION notifications_email();
DROP TABLE email_deliveries;
DROP FUNCTION email_deliveries_notify();
DROP FUNCTION app_notify(uuid[], uuid, text, text);
DROP TRIGGER workflow_approvals_notify ON workflow_approvals;
DROP FUNCTION notify_on_approval_request();
DROP TABLE notifications;
