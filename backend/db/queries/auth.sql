-- All queries here run via db.WithSystem: the tables are not accessible to servicelater_app.

-- name: GetLoginUser :one
SELECT u.id, u.email, u.name, u.role, c.password_hash
FROM users u JOIN user_credentials c ON c.user_id = u.id
WHERE u.email = lower(sqlc.arg(email)::text) AND u.deactivated_at IS NULL AND u.app_name IS NULL;

-- name: UpsertCredentials :exec
INSERT INTO user_credentials (user_id, password_hash) VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET password_hash = EXCLUDED.password_hash, updated_at = now();

-- name: CreateSession :exec
INSERT INTO sessions (user_id, family_id, token_hash, expires_at) VALUES ($1, $2, $3, $4);

-- name: GetSessionForUpdate :one
SELECT * FROM sessions WHERE token_hash = $1 FOR UPDATE;

-- name: MarkSessionRotated :exec
UPDATE sessions SET rotated_at = now() WHERE id = $1;

-- name: DeleteSessionFamily :exec
DELETE FROM sessions WHERE family_id = $1;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at < now();

-- name: HitRateLimit :one
INSERT INTO rate_limits (key, count, window_start) VALUES ($1, 1, now())
ON CONFLICT (key) DO UPDATE SET
    count = CASE WHEN rate_limits.window_start < now() - make_interval(secs => sqlc.arg(window_secs)::float8)
                 THEN 1 ELSE rate_limits.count + 1 END,
    window_start = CASE WHEN rate_limits.window_start < now() - make_interval(secs => sqlc.arg(window_secs)::float8)
                        THEN now() ELSE rate_limits.window_start END
RETURNING count;

-- name: ResetRateLimit :exec
DELETE FROM rate_limits WHERE key = $1;

-- name: DeleteStaleRateLimits :execrows
DELETE FROM rate_limits WHERE window_start < now() - interval '1 day';
