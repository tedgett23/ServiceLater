-- Email in (00052). The worker writes as the system; admins read the log.

-- name: InboundSeen :one
SELECT EXISTS (SELECT 1 FROM inbound_emails WHERE message_id = $1)::bool AS seen;

-- name: LogInbound :exec
INSERT INTO inbound_emails (message_id, from_address, subject, action, detail, task_id, user_id, attachments, authentication)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (message_id) DO NOTHING;

-- How many incidents a sender filed by email lately: a flood is refused.
-- name: RecentInboundIncidents :one
SELECT count(*) FROM inbound_emails
WHERE lower(from_address) = lower($1) AND action = 'incident' AND created_at > now() - interval '1 hour';

-- name: ListInboundEmails :many
SELECT e.id, e.from_address, e.subject, e.action, e.detail, e.attachments, e.authentication, e.created_at,
       COALESCE(tt.prefix || t.number::text, '')::text AS reference
FROM inbound_emails e
LEFT JOIN tasks t ON t.id = e.task_id
LEFT JOIN task_types tt ON tt.type = t.type
ORDER BY e.created_at DESC, e.id
LIMIT sqlc.arg('limit');

-- A notification email's recipient and record, read as the system when a reply to it arrives.
-- name: GetNotificationForReply :one
SELECT n.id, n.user_id, n.task_id, n.kind, u.email, u.deactivated_at
FROM notifications n JOIN users u ON u.id = n.user_id
WHERE n.id = $1;

-- A person by email address, read as the system, for mail that is not a reply: active people only, not apps' or
-- service accounts.
-- name: GetSenderByEmail :one
SELECT u.id, u.deactivated_at FROM users u
WHERE u.email = lower(sqlc.arg('email')::text) AND u.app_name IS NULL
  AND NOT EXISTS (SELECT 1 FROM service_accounts s WHERE s.user_id = u.id);
