-- App bookkeeping. All of these run through db.WithSystem: the tables are not readable by servicelater_app.

-- name: GetApp :one
SELECT * FROM apps WHERE name = $1;

-- name: ListApps :many
SELECT * FROM apps ORDER BY name;

-- name: UpsertApp :exec
INSERT INTO apps (name, version, manifest, installed_by) VALUES ($1, $2, $3, $4)
ON CONFLICT (name) DO UPDATE SET
    version = EXCLUDED.version, manifest = EXCLUDED.manifest, installed_by = EXCLUDED.installed_by, updated_at = now();

-- name: DeleteApp :exec
DELETE FROM apps WHERE name = $1;

-- name: ListCurrentDefinitions :many
SELECT * FROM app_definitions WHERE current ORDER BY app_name, kind, name;

-- name: ListAppCurrentDefinitions :many
SELECT * FROM app_definitions WHERE app_name = $1 AND current ORDER BY kind, name;

-- name: MaxDefinitionVersion :one
SELECT COALESCE(MAX(version), 0)::int FROM app_definitions WHERE app_name = $1 AND kind = $2 AND name = $3;

-- name: ClearCurrentDefinition :exec
UPDATE app_definitions SET current = false WHERE app_name = $1 AND kind = $2 AND name = $3 AND current;

-- name: InsertDefinition :exec
INSERT INTO app_definitions (app_name, kind, name, version, checksum, body, source) VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: DeleteDefinitions :exec
DELETE FROM app_definitions WHERE app_name = $1 AND kind = $2 AND name = $3;

-- Another installed app that already provides this kind of definition under this name.
-- name: FindDefinitionProvider :one
SELECT app_name FROM app_definitions WHERE kind = $1 AND name = $2 AND current AND app_name <> $3 LIMIT 1;

-- Statuses in use, per record type, so an install can refuse to strand records (see apps/install.go).
-- name: ListTaskStatusCounts :many
SELECT type, status, count(*)::bigint AS n FROM tasks GROUP BY type, status;

-- name: RenameTaskStatus :execrows
UPDATE tasks SET status = $3, state_category = $4 WHERE type = $1 AND status = $2;

-- Keep stored categories in step with the lifecycle after an app changes what a state means.
-- name: SyncTaskCategory :execrows
UPDATE tasks SET state_category = $3 WHERE type = ANY($1::text[]) AND status = $2 AND state_category <> $3;
