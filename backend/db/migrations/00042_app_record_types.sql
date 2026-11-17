-- +goose Up

-- Apps define record types of their own (record_type definitions, docs/apps-platform.md §9.5): a row here with its
-- own prefix and numbering, written by the install. Core rows have no owner. A type whose app is removed stays, marked
-- retired, so its records keep their references and can still be opened, but nothing lists or files them.
-- owner_app has no foreign key: the app's row goes on uninstall and the type must not.
ALTER TABLE task_types ADD COLUMN owner_app text;
ALTER TABLE task_types ADD COLUMN plural text NOT NULL DEFAULT '';
ALTER TABLE task_types ADD COLUMN description text NOT NULL DEFAULT '';
ALTER TABLE task_types ADD COLUMN retired_at timestamptz;
UPDATE task_types SET plural = label || 's';

-- An app's type can be removed and defined again (tests do), so a sequence left behind is reused, not an error.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION task_types_create_sequence() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    EXECUTE format('CREATE SEQUENCE IF NOT EXISTS %I', 'task_number_' || NEW.type);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- Nobody may file a record of a retired type, whatever it allowed before.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_anyone_may_file(p_type text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$ SELECT coalesce((SELECT anyone_may_file AND retired_at IS NULL FROM task_types WHERE type = p_type), false) $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_anyone_may_file(p_type text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$ SELECT coalesce((SELECT anyone_may_file FROM task_types WHERE type = p_type), false) $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION task_types_create_sequence() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    EXECUTE format('CREATE SEQUENCE %I', 'task_number_' || NEW.type);
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER TABLE task_types DROP COLUMN retired_at;
ALTER TABLE task_types DROP COLUMN description;
ALTER TABLE task_types DROP COLUMN plural;
ALTER TABLE task_types DROP COLUMN owner_app;
