-- +goose Up
-- Saved views (docs/query.md "Saved views"): a query someone saved under a name, which other queries, and
-- GET /api/v1/views/{name}/rows, read like a table. Only the SQL is kept: nothing is created in the database. The
-- runner puts a view's SQL in front of a query that names it as a WITH clause and checks the whole again, so a view
-- never shows more than the query views do, and whoever reads one sees their own rows, not its author's.
CREATE TABLE saved_views (
    name        text PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9_]{1,62}$'),
    description text NOT NULL DEFAULT '' CHECK (length(description) <= 2000),
    sql         text NOT NULL CHECK (length(sql) BETWEEN 1 AND 20000),
    -- Private to its owner, or readable by everyone who may run queries.
    shared      boolean NOT NULL DEFAULT false,
    -- What it gives, as [{name, type}], worked out by running it with no rows when it was saved.
    columns     jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(columns) = 'array'),
    -- The saved views its SQL reads directly. One that others read cannot be removed, made private while a shared one
    -- reads it, or given other columns.
    depends_on  text[] NOT NULL DEFAULT '{}',
    -- No ON DELETE action: like a comment, a view keeps who made it (deleting them is a 409; deactivate instead).
    owner_id    uuid NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX saved_views_owner_idx ON saved_views (owner_id);
CREATE INDEX saved_views_depends_idx ON saved_views USING gin (depends_on);
ALTER TABLE saved_views ENABLE ROW LEVEL SECURITY;
CREATE POLICY saved_views_select ON saved_views FOR SELECT TO servicelater_app
    USING (app_can('query:run') AND (owner_id = app_current_user_id() OR shared));
CREATE POLICY saved_views_insert ON saved_views FOR INSERT TO servicelater_app
    WITH CHECK (app_can('query:run') AND owner_id = app_current_user_id());
CREATE POLICY saved_views_update ON saved_views FOR UPDATE TO servicelater_app
    USING (app_can('query:run') AND owner_id = app_current_user_id())
    WITH CHECK (owner_id = app_current_user_id());
-- Those who manage settings may remove anyone's (a shared view gone wrong); everyone else their own.
CREATE POLICY saved_views_delete ON saved_views FOR DELETE TO servicelater_app
    USING (owner_id = app_current_user_id() OR app_can('settings:manage'));

-- The views that read a view, including ones the caller cannot see (private to someone else): a change that would
-- break them must be refused, and saying so needs them. The names of those the caller cannot see are not given.
-- +goose StatementBegin
CREATE FUNCTION app_saved_view_dependents(p_name text)
    RETURNS TABLE (name text, shared boolean, visible boolean)
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$
    SELECT CASE WHEN v.owner_id = app_current_user_id() OR v.shared THEN v.name ELSE '' END,
           v.shared, v.owner_id = app_current_user_id() OR v.shared
    FROM saved_views v
    WHERE p_name = ANY (v.depends_on)
    ORDER BY v.name
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION app_saved_view_dependents(text);
DROP TABLE saved_views;
