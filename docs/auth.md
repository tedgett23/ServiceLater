# Signing in: passwords, API keys and single sign-on

ServiceLater knows three ways in. All of them end as one user id; row-level security and permissions work the same
whichever was used.

## Passwords

`POST /api/v1/auth/login` (bcrypt, rate-limited) sets an `sl_access` cookie (a short-lived token) and an `sl_refresh`
cookie (rotating, reuse revokes the family). Admin → Sign-in can keep passwords for **admins only**
(`sign_in.passwords = "admins"`, only once a single sign-on provider is on): anyone else's correct password is
answered 403 with "sign in with your organisation's single sign-on". Admins keep passwords so a broken provider cannot
lock everyone out.

## API keys (integrations)

Decided with the user (2026-11-17): **keys belong to service accounts**, never to people, and a key may be **narrowed**.

- A **service account** (`POST /api/v1/service-accounts`, `users:manage`; Admin → Users → Add a service account) is a
  `users` row plus a `service_accounts` row (00048). It has a made-up address (`…@service.servicelater.invalid`, never
  emailed), cannot sign in, cannot be given a password, is not offered as an assignee, and gets roles and groups like
  a person. Deactivating it stops all its keys.
- A **key** (`POST /api/v1/users/{id}/api-keys`) is `slk_` + 256 random bits, shown once. Only its SHA-256 and first
  characters are stored (`api_keys`, owner-only but for a column grant without the hash). Requests send
  `Authorization: Bearer slk_…`; `auth.Tokens.Authenticate` looks the key up on every request (revocation is
  immediate) and records use at most once a minute. Keys may expire (`expires_at`).
- **Narrowing** (`scopes`): a list of permissions the account has. `authz.Narrow` (Go) and `app_narrow_permissions`
  (SQL) agree (`TestNarrowingMatchesTheDatabase`): a scope is kept if held, `records:<verb>:<type>` is kept under
  `records:<verb>:*`, and `records:<verb>:*` keeps only the record permissions of that verb the account has. Go narrows
  `Principal.Perms` (`PermissionCache.Load`, `/auth/me`); the database narrows too: `db.WithScopes` on the request
  context makes `db.WithUser` call `app_begin_key`, so RLS sees only the key's permissions.
- **Nobody makes a key that could do more than they can** (`app_create_api_key` checks every permission the key would
  have against the maker's), so `users:manage` alone cannot mint a key for an admin account.

## Single sign-on (OpenID Connect)

Decided with the user: **OIDC now, SAML next**; first sign-in **creates the account** (as an end user) and **groups
sync** from a claim.

- Providers are rows (`sso_providers`, 00049; Admin → Sign-in; `GET|PUT|DELETE /api/v1/sso-providers/{id}`,
  `settings:manage`): label, issuer, client id, allowed email domains, create accounts or not, groups claim, enabled.
  The client secret is a secret (`sso.<id>.client_secret`, hyphens as underscores); without one the client is public
  and PKCE protects it.
- Flow (`internal/sso`): `GET /auth/sso/{id}/start?next=` → the provider (authorization code, PKCE S256, nonce; the
  state, nonce, verifier and next travel in a 10-minute HMAC-signed `sl_sso` cookie, `SameSite=Lax` because the
  provider sends the browser back cross-site) → `GET /auth/sso/{id}/callback` swaps the code, verifies the ID token
  (issuer, audience, signature from the provider's keys, nonce) and resolves the person, then sets the same cookies a
  password sign-in does and goes to `next` (a local path only). Refusals go to `/login?sso_error=` with a sentence the
  person can act on.
- **Who is it** (`Manager.Resolve`, as the system): the provider's subject (`user_identities`), else a person (not an
  app's principal or a service account) with that **verified** email, else a new end user if the provider creates
  accounts. Allowed domains apply to everyone; deactivated people are refused.
- **Group sync**: `sso_group_maps` maps claim values to groups. At each sign-in the person joins every mapped group one
  of their values maps to and leaves every mapped group none does; unmapped groups are untouched.
- The redirect address is the email settings' base URL when set, else the request's; the admin page shows it to
  register at the provider.
- Tests run against a real-protocol fake provider (`internal/sso/ssotest`; e2e serves it with `cmd/fakeidp`, whose
  authorize page is a form).

Not built yet: SAML; SCIM provisioning; showing a person's linked identities on their admin page; a CLI for keys;
provider discovery by email domain.
