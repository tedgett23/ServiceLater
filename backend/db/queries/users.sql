-- name: CreateUser :one
INSERT INTO users (email, name, role)
VALUES (lower(sqlc.arg(email)::text), $1, $2)
RETURNING id, email, name, role, created_at;

-- name: GetUser :one
SELECT id, email, name, role, created_at FROM users WHERE id = $1;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: ListStaff :many
SELECT id, email, name, role, created_at FROM users WHERE role IN ('admin', 'agent') ORDER BY name;
