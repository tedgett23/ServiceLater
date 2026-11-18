-- +goose Up
-- Scripts (docs/apps-platform.md §6.7) are more than workflows: changing a script step's or action's Starlark needs
-- scripts:edit as well as workflows:edit. Admins hold it.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,catalog:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,forms:manage,freezes:manage,groups:manage,knowledge:publish,knowledge:read_internal,knowledge:write,records:create:*,records:delete:*,records:read:*,records:update:*,restricted:read,roles:manage,scripts:edit,settings:manage,users:manage,users:read,workflows:edit,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();

-- An app's script files (scripts/<name>.star) are kept in the studio layer like its YAML: whole, by path.
ALTER TABLE app_overrides DROP CONSTRAINT app_overrides_path_check,
    ADD CONSTRAINT app_overrides_path_check CHECK (path ~ '^[a-z]+/[a-z0-9][a-z0-9_.-]*\.(yaml|star)$');

-- +goose Down
DELETE FROM app_overrides WHERE path LIKE '%.star';
ALTER TABLE app_overrides DROP CONSTRAINT app_overrides_path_check,
    ADD CONSTRAINT app_overrides_path_check CHECK (path ~ '^[a-z]+/[a-z0-9][a-z0-9_.-]*\.yaml$');
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,catalog:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,forms:manage,freezes:manage,groups:manage,knowledge:publish,knowledge:read_internal,knowledge:write,records:create:*,records:delete:*,records:read:*,records:update:*,restricted:read,roles:manage,settings:manage,users:manage,users:read,workflows:edit,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();

