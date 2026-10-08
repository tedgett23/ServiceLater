-- +goose Up

-- An import changes thousands of items in one transaction (docs/apps-platform.md §8.2). One ci.changed event per row
-- would make every open CMDB page refetch thousands of times, so a bulk change turns them off for its transaction
-- (app.ci_bulk) and sends one {"bulk": true} event when it is done. The setting only quiets hints; history and RLS
-- are untouched.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cis_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF current_setting('app.ci_bulk', true) = 'on' THEN
        RETURN NULL;
    END IF;
    IF TG_TABLE_NAME = 'ci_relationships' THEN
        PERFORM pg_notify('ci_changes', json_build_object('id', coalesce(NEW.from_ci, OLD.from_ci))::text);
        PERFORM pg_notify('ci_changes', json_build_object('id', coalesce(NEW.to_ci, OLD.to_ci))::text);
    ELSE
        PERFORM pg_notify('ci_changes', json_build_object('id', coalesce(NEW.id, OLD.id))::text);
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cis_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_TABLE_NAME = 'ci_relationships' THEN
        PERFORM pg_notify('ci_changes', json_build_object('id', coalesce(NEW.from_ci, OLD.from_ci))::text);
        PERFORM pg_notify('ci_changes', json_build_object('id', coalesce(NEW.to_ci, OLD.to_ci))::text);
    ELSE
        PERFORM pg_notify('ci_changes', json_build_object('id', coalesce(NEW.id, OLD.id))::text);
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
