-- +goose Up
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
DELETE FROM settings WHERE name LIKE 'inbound.%';
DELETE FROM secrets WHERE name LIKE 'inbound.%';
