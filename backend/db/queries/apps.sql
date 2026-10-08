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

-- name: DeleteAppFiles :exec
-- System only: an install replaces the app's stored files.
DELETE FROM app_files WHERE app_name = $1;

-- name: InsertAppFile :exec
INSERT INTO app_files (app_name, path, content) VALUES ($1, $2, $3);

-- name: ListAppFiles :many
-- System only: an installed app's own files, to compile it again with an edit.
SELECT path, content FROM app_files WHERE app_name = $1;

-- name: ListAppOverrides :many
-- The app's studio layer (edits made in ServiceLater). The installer reads it as the system.
SELECT path, content, updated_at FROM app_overrides WHERE app_name = $1 ORDER BY path;

-- name: GetAppOverride :one
SELECT o.path, o.content, o.updated_at, u.name AS updated_by_name
FROM app_overrides o LEFT JOIN users u ON u.id = o.updated_by
WHERE o.app_name = $1 AND o.path = $2;

-- name: UpsertAppOverride :exec
INSERT INTO app_overrides (app_name, path, content, updated_by) VALUES ($1, $2, $3, $4)
ON CONFLICT (app_name, path) DO UPDATE SET content = EXCLUDED.content, updated_by = EXCLUDED.updated_by, updated_at = now();

-- name: DeleteAppOverride :execrows
DELETE FROM app_overrides WHERE app_name = $1 AND path = $2;

-- name: ListAllAppOverrides :many
-- Every app's studio layer, for export (system only).
SELECT app_name, path, content FROM app_overrides ORDER BY app_name, path;
