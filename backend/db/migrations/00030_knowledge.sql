-- +goose Up

-- The knowledge base (docs/apps-platform.md §8.3): articles people write and read at run time, so core data like
-- configuration items. An article is named by its reference (KB12). Its body is Markdown, kept as written; the web
-- renders and sanitises it. Every save keeps a version, so nothing written is lost and changes can be compared.
CREATE SEQUENCE kb_article_number;

-- What search matches: the title and tags weigh more than the category, which weighs more than the body. A function,
-- because a generated column needs an immutable expression and array_to_string is not declared one (on text[] it is).
-- +goose StatementBegin
CREATE FUNCTION app_kb_search(p_title text, p_tags text[], p_category text, p_body text) RETURNS tsvector
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
    SELECT setweight(to_tsvector('english', p_title), 'A') ||
           setweight(to_tsvector('english', array_to_string(p_tags, ' ')), 'A') ||
           setweight(to_tsvector('english', p_category), 'B') ||
           setweight(to_tsvector('english', p_body), 'C')
$$;
-- +goose StatementEnd
CREATE TABLE kb_articles (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number      bigint NOT NULL UNIQUE DEFAULT nextval('kb_article_number'),
    title       text NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 200),
    body        text NOT NULL DEFAULT '' CHECK (length(body) <= 100000),
    category    text NOT NULL DEFAULT '' CHECK (length(category) <= 100),
    tags        text[] NOT NULL DEFAULT '{}' CHECK (cardinality(tags) <= 20),
    -- everyone: anyone signed in, once published; internal: those with knowledge:read_internal (agents).
    audience    text NOT NULL DEFAULT 'everyone' CHECK (audience IN ('everyone', 'internal')),
    status      text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'review', 'published', 'retired')),
    version     integer NOT NULL DEFAULT 1,
    author_id   uuid REFERENCES users (id) ON DELETE SET NULL,
    updated_by  uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    search      tsvector GENERATED ALWAYS AS (app_kb_search(title, tags, category, body)) STORED
);
CREATE INDEX kb_articles_search_idx ON kb_articles USING gin (search);
CREATE INDEX kb_articles_status_idx ON kb_articles (status, updated_at DESC);
CREATE INDEX kb_articles_category_idx ON kb_articles (lower(category)) WHERE category <> '';

-- Each save's content, numbered from 1. Written by the save itself, never edited.
CREATE TABLE kb_article_versions (
    article_id uuid NOT NULL REFERENCES kb_articles (id) ON DELETE CASCADE,
    version    integer NOT NULL,
    title      text NOT NULL,
    body       text NOT NULL,
    category   text NOT NULL,
    tags       text[] NOT NULL,
    audience   text NOT NULL,
    status     text NOT NULL,
    saved_by   uuid REFERENCES users (id) ON DELETE SET NULL,
    saved_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (article_id, version)
);

-- Records linked to articles: the article that answers an incident. Deleting a draft that was linked unlinks it.
CREATE TABLE task_articles (
    task_id    uuid NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    article_id uuid NOT NULL REFERENCES kb_articles (id) ON DELETE CASCADE,
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, article_id)
);
CREATE INDEX task_articles_article_idx ON task_articles (article_id);

-- Who reads an article: a published one by its audience; one not published (draft, review, retired) only those who
-- write or publish articles.
-- +goose StatementBegin
CREATE FUNCTION app_can_read_article(p_status text, p_audience text) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE
        WHEN p_status = 'published' THEN app_current_user_id() IS NOT NULL
             AND (p_audience = 'everyone' OR app_can('knowledge:read_internal') OR app_can('knowledge:publish'))
        ELSE app_can('knowledge:write') OR app_can('knowledge:publish')
    END
$$;
-- +goose StatementEnd

ALTER TABLE kb_articles ENABLE ROW LEVEL SECURITY;
CREATE POLICY kb_articles_select ON kb_articles FOR SELECT TO servicelater_app USING (app_can_read_article(status, audience));
-- Writers add drafts; a published or retired article is changed only by those who publish.
CREATE POLICY kb_articles_insert ON kb_articles FOR INSERT TO servicelater_app
    WITH CHECK ((app_can('knowledge:write') AND status IN ('draft', 'review')) OR app_can('knowledge:publish'));
CREATE POLICY kb_articles_update ON kb_articles FOR UPDATE TO servicelater_app
    USING ((app_can('knowledge:write') AND status IN ('draft', 'review')) OR app_can('knowledge:publish'))
    WITH CHECK ((app_can('knowledge:write') AND status IN ('draft', 'review')) OR app_can('knowledge:publish'));
-- Only an article never published can be deleted (by those who publish); retire one instead.
CREATE POLICY kb_articles_delete ON kb_articles FOR DELETE TO servicelater_app
    USING (app_can('knowledge:publish') AND published_at IS NULL);

ALTER TABLE kb_article_versions ENABLE ROW LEVEL SECURITY;
CREATE POLICY kb_article_versions_select ON kb_article_versions FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM kb_articles a WHERE a.id = article_id));
CREATE POLICY kb_article_versions_insert ON kb_article_versions FOR INSERT TO servicelater_app
    WITH CHECK (app_can('knowledge:write') OR app_can('knowledge:publish'));

-- A link is seen by whoever sees both the record and the article (a requester sees the published article an agent
-- linked to their incident), and made by whoever may update the record.
ALTER TABLE task_articles ENABLE ROW LEVEL SECURITY;
CREATE POLICY task_articles_select ON task_articles FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id) AND EXISTS (SELECT 1 FROM kb_articles a WHERE a.id = article_id));
CREATE POLICY task_articles_insert ON task_articles FOR INSERT TO servicelater_app
    WITH CHECK (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id AND app_can_record('update', t.type))
                AND EXISTS (SELECT 1 FROM kb_articles a WHERE a.id = article_id));
CREATE POLICY task_articles_delete ON task_articles FOR DELETE TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id AND app_can_record('update', t.type)));

-- Linking an article changes what a record shows: tell its watchers, as a CI link does.
CREATE TRIGGER task_articles_notify AFTER INSERT OR DELETE ON task_articles
    FOR EACH ROW EXECUTE FUNCTION task_cis_notify();

-- Agents write drafts and read internal articles; admins also publish (and the knowledge-base app's
-- knowledge-manager role grants that).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,freezes:manage,groups:manage,knowledge:publish,knowledge:read_internal,knowledge:write,records:create:*,records:delete:*,records:read:*,records:update:*,roles:manage,settings:manage,users:manage,users:read,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,knowledge:read_internal,knowledge:write,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_seed_builtin_roles() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO roles (name, label, description, permissions) VALUES
        ('admin', 'Admin', 'Everything, including people, groups and roles.',
         '{calendars:manage,cmdb:manage,cmdb:read,comments:delete,comments:internal,freezes:manage,groups:manage,records:create:*,records:delete:*,records:read:*,records:update:*,roles:manage,settings:manage,users:manage,users:read,workflows:read_all,workflows:run}'),
        ('agent', 'Agent', 'Works tickets of every type: sees, files and edits them, writes internal notes, runs workflows.',
         '{cmdb:read,comments:internal,records:create:*,records:read:*,records:update:*,users:read,workflows:read_all,workflows:run}'),
        ('end_user', 'End user', 'Files tickets and follows their own.', '{}')
    ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
        permissions = EXCLUDED.permissions, updated_at = now();
$$;
-- +goose StatementEnd
SELECT app_seed_builtin_roles();
DROP TRIGGER task_articles_notify ON task_articles;
DROP TABLE task_articles;
DROP TABLE kb_article_versions;
DROP TABLE kb_articles;
DROP FUNCTION app_can_read_article(text, text);
DROP FUNCTION app_kb_search(text, text[], text, text);
DROP SEQUENCE kb_article_number;
