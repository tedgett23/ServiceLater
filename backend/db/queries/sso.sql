-- name: ListSSOProviders :many
SELECT id, label, issuer, client_id, allowed_domains, create_users, groups_claim, enabled, created_at, updated_at
FROM sso_providers ORDER BY label;

-- name: GetSSOProvider :one
SELECT id, label, issuer, client_id, allowed_domains, create_users, groups_claim, enabled, created_at, updated_at
FROM sso_providers WHERE id = $1;

-- name: UpsertSSOProvider :one
INSERT INTO sso_providers (id, label, issuer, client_id, allowed_domains, create_users, groups_claim, enabled)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (id) DO UPDATE SET label = EXCLUDED.label, issuer = EXCLUDED.issuer, client_id = EXCLUDED.client_id,
    allowed_domains = EXCLUDED.allowed_domains, create_users = EXCLUDED.create_users,
    groups_claim = EXCLUDED.groups_claim, enabled = EXCLUDED.enabled, updated_at = now()
RETURNING id, label, issuer, client_id, allowed_domains, create_users, groups_claim, enabled, created_at, updated_at;

-- name: DeleteSSOProvider :execrows
DELETE FROM sso_providers WHERE id = $1;

-- name: ListSSOGroupMaps :many
SELECT m.claim_value, m.group_id, g.name AS group_name
FROM sso_group_maps m JOIN groups g ON g.id = m.group_id
WHERE m.provider_id = $1 ORDER BY m.claim_value, g.name;

-- name: ClearSSOGroupMaps :exec
DELETE FROM sso_group_maps WHERE provider_id = $1;

-- name: AddSSOGroupMap :exec
INSERT INTO sso_group_maps (provider_id, claim_value, group_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- System only, before anyone signs in: the buttons on the sign-in page.
-- name: ListEnabledSSOProviders :many
SELECT id, label FROM sso_providers WHERE enabled ORDER BY label;

-- name: GetEnabledSSOProvider :one
SELECT id, label, issuer, client_id, allowed_domains, create_users, groups_claim, enabled, created_at, updated_at
FROM sso_providers WHERE id = $1 AND enabled;

-- name: SystemSSOGroupMaps :many
SELECT claim_value, group_id FROM sso_group_maps WHERE provider_id = $1;

-- name: GetIdentityUser :one
SELECT user_id FROM user_identities WHERE provider_id = $1 AND subject = $2;

-- name: LinkIdentity :exec
INSERT INTO user_identities (provider_id, subject, user_id, email) VALUES ($1, $2, $3, $4)
ON CONFLICT (provider_id, subject) DO UPDATE SET email = EXCLUDED.email, last_sign_in_at = now();

-- A person (not an app's principal or a service account) by email, for linking a first SSO sign-in to them.
-- name: GetPersonByEmail :one
SELECT u.id, u.role, u.deactivated_at FROM users u
WHERE u.email = lower(sqlc.arg('email')::text) AND u.app_name IS NULL
  AND NOT EXISTS (SELECT 1 FROM service_accounts s WHERE s.user_id = u.id);

-- name: GetSignInUser :one
SELECT id, email, name, role, deactivated_at FROM users WHERE id = $1;

-- name: ListUserIdentities :many
SELECT i.provider_id, p.label, i.email, i.created_at, i.last_sign_in_at
FROM user_identities i JOIN sso_providers p ON p.id = i.provider_id
WHERE i.user_id = $1 ORDER BY p.label;
