-- +goose Up
-- SQL queries people write themselves (internal/query, docs/query.md). They run on a separate login role that may
-- read only the views in the `query` schema. Those views are not trusting: each filters its rows by an identity it
-- checks itself (app_query_identity), signed by the server with pgcrypto's hmac. The session settings that RLS trusts
-- (app.permissions and the rest) mean nothing to them. Even if a forbidden function got past the server's check of
-- the SQL (set_config, say), the person would still see only what they may see. Then saved views: queries kept under
-- a name that others read like a table.

-- The login role queries run as. Roles belong to the whole cluster, not one database, so each database has its own,
-- named after it. The server sets the role's password from the key below, so every api server agrees on it without
-- any configuration.
-- +goose StatementBegin
CREATE FUNCTION app_query_role() RETURNS text
    LANGUAGE sql STABLE
    AS $$ SELECT left(current_database(), 57) || '_query' $$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE r text := app_query_role();
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = r) THEN
        EXECUTE format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT CONNECTION LIMIT 20', r);
    END IF;
    -- Belt and braces under the server's own per-query settings.
    EXECUTE format('ALTER ROLE %I IN DATABASE %I SET default_transaction_read_only = on', r, current_database());
    EXECUTE format('ALTER ROLE %I IN DATABASE %I SET statement_timeout = %L', r, current_database(), '60s');
    EXECUTE format('ALTER ROLE %I IN DATABASE %I SET idle_in_transaction_session_timeout = %L', r, current_database(), '60s');
    EXECUTE format('ALTER ROLE %I IN DATABASE %I SET search_path = query', r, current_database());
END
$$;
-- +goose StatementEnd

-- The public schema is closed to it (00001_foundation): it reads only this one.
CREATE SCHEMA query;
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA query TO %I', app_query_role());
    -- Every view made in it later is readable by the role, and nothing else is.
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA query GRANT SELECT ON TABLES TO %I', current_user, app_query_role());
END
$$;
-- +goose StatementEnd

-- The key the server signs each query's identity with. Only the owner reads it (the server, as the system).
CREATE TABLE query_keys (
    id         int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    key        bytea NOT NULL CHECK (length(key) = 32),
    created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE query_keys ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON query_keys FROM servicelater_app;
INSERT INTO query_keys (key) VALUES (gen_random_bytes(32));

-- Who a query runs for: the signed setting query.token is "<user id>|<expires, unix seconds>|<scopes>|<hex HMAC-SHA256
-- of what comes before it>". Scopes are an API key's, comma-separated, or * for none. A token that is missing,
-- forged, edited or expired is refused, as is anyone who does not hold query:run. The person's permissions, and the
-- restricted groups hidden from them, are worked out here from the database, never taken from the session. Every
-- query view joins this once.
-- +goose StatementBegin
CREATE FUNCTION app_query_identity(OUT user_id uuid, OUT perms text, OUT hidden text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    parts text[] := string_to_array(current_setting('query.token', true), '|');
    p text[];
BEGIN
    IF coalesce(array_length(parts, 1), 0) <> 4
       OR encode(hmac(convert_to(parts[1] || '|' || parts[2] || '|' || parts[3], 'UTF8'), (SELECT key FROM query_keys), 'sha256'), 'hex') <> parts[4] THEN
        RAISE EXCEPTION 'this query carries no valid identity' USING ERRCODE = '42501';
    END IF;
    IF to_timestamp(parts[2]::bigint) < clock_timestamp() THEN
        RAISE EXCEPTION 'this query''s identity has expired' USING ERRCODE = '42501';
    END IF;
    user_id := parts[1]::uuid;
    p := app_permissions_of(user_id);
    IF parts[3] <> '*' THEN
        p := app_narrow_permissions(p, string_to_array(parts[3], ','));
    END IF;
    IF NOT 'query:run' = ANY (p) THEN
        RAISE EXCEPTION 'you may not run queries' USING ERRCODE = '42501';
    END IF;
    perms := ',' || array_to_string(p, ',') || ',';
    hidden := CASE WHEN 'restricted:read' = ANY (p) THEN ','
                   ELSE ',' || coalesce((SELECT string_agg(g.id::text, ',') FROM groups g
                                          WHERE g.restricted
                                            AND NOT EXISTS (SELECT 1 FROM group_members m
                                                             WHERE m.group_id = g.id AND m.user_id = parts[1]::uuid)), '') || ','
              END;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_query_identity() FROM PUBLIC;
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('GRANT EXECUTE ON FUNCTION app_query_identity() TO %I', app_query_role());
END
$$;
-- +goose StatementEnd

-- Every query someone ran or tried to, and how it went. Written by the server as the system, so that a refused or
-- failed query is still recorded. People read their own; those who manage settings read everyone's.
CREATE TABLE query_runs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    api_key_id  uuid REFERENCES api_keys (id) ON DELETE SET NULL,
    sql         text NOT NULL,
    params      jsonb NOT NULL DEFAULT '[]',
    outcome     text NOT NULL CHECK (outcome IN ('ok', 'refused', 'failed', 'timeout')),
    error       text NOT NULL DEFAULT '',
    row_count   int NOT NULL DEFAULT 0,
    truncated   boolean NOT NULL DEFAULT false,
    duration_ms int NOT NULL DEFAULT 0,
    started_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX query_runs_user_idx ON query_runs (user_id, started_at DESC);
CREATE INDEX query_runs_started_idx ON query_runs (started_at DESC);
ALTER TABLE query_runs ENABLE ROW LEVEL SECURITY;
REVOKE INSERT, UPDATE, DELETE ON query_runs FROM servicelater_app;
CREATE POLICY query_runs_select ON query_runs FOR SELECT TO servicelater_app
    USING (user_id = app_current_user_id() OR app_can('settings:manage'));

-- The views SQL queries read (docs/query.md "Writing a view"). They run as their owner, so no RLS applies: each one
-- filters by app_query_identity() itself, and must never show more than the API would. The rules mirrored here:
--   records         the tasks_select policy (app_sees_task): the requester, or records:read:<type> and the record is
--                   not routed to a restricted group hidden from them (unless they are its assignee);
--   anything on a   visible when its record is (joined through query.records, so the rule lives in one place);
--   record
--   comments        internal notes only with comments:internal;
--   people          everyone with users:read (the directory is Go-checked), else only themselves; never app principals;
--   items           cmdb:read (and the record too, for links);
--   articles        app_can_read_article;
--   history         a record's when the record is visible, an item's with cmdb:read, a setting's with settings:manage.
-- Never call app_can, app_current_user_id, current_setting or anything else that reads session settings here: the
-- person writing the query controls those. TestQueryViewsTrustOnlyTheIdentity checks.
-- Columns are a contract saved queries depend on: add them; rename or drop only with a deprecation.

CREATE VIEW query.record_types WITH (security_barrier) AS
    SELECT tt.type, tt.prefix, tt.label, tt.plural, tt.description, tt.owner_app, tt.retired_at
    FROM app_query_identity() me, task_types tt;
COMMENT ON VIEW query.record_types IS 'Every record type: incidents, changes, requests, problems, tasks and the types apps define. A retired type''s records can still be queried.';
COMMENT ON COLUMN query.record_types.type IS 'The type''s name, as records.type has it (incident, change...).';
COMMENT ON COLUMN query.record_types.prefix IS 'What its references start with (INC, CHG...).';
COMMENT ON COLUMN query.record_types.label IS 'Its name for people, singular.';
COMMENT ON COLUMN query.record_types.plural IS 'Its name for people, plural.';
COMMENT ON COLUMN query.record_types.description IS 'What it is for.';
COMMENT ON COLUMN query.record_types.owner_app IS 'The app that defines it; null for the built-in types.';
COMMENT ON COLUMN query.record_types.retired_at IS 'When its app was removed; null while it is in use.';

CREATE VIEW query.records WITH (security_barrier) AS
    SELECT t.id, tt.prefix || t.number::text AS reference, t.type, t.number, t.title, t.description, t.status,
           t.state_category, t.priority, t.requester_id, t.assignee_id, t.assignment_group_id, t.parent_id, t.fields,
           t.created_at, t.updated_at
    FROM app_query_identity() me
    JOIN tasks t ON t.requester_id = me.user_id
        OR ((position(',records:read:' || t.type || ',' IN me.perms) > 0 OR position(',records:read:*,' IN me.perms) > 0)
            AND (t.assignment_group_id IS NULL
                 OR t.assignee_id = me.user_id
                 OR position(',' || t.assignment_group_id::text || ',' IN me.hidden) = 0))
    JOIN task_types tt ON tt.type = t.type;
COMMENT ON VIEW query.records IS 'Records of every type that you can see: incidents, changes, requests, problems, tasks and app types. Join incidents, changes or requests on record_id for their own columns.';
COMMENT ON COLUMN query.records.id IS 'The record''s id; other views name it record_id.';
COMMENT ON COLUMN query.records.reference IS 'How people name it: the type''s prefix and the number (INC42).';
COMMENT ON COLUMN query.records.type IS 'Its record type (see record_types).';
COMMENT ON COLUMN query.records.number IS 'Its number within its type.';
COMMENT ON COLUMN query.records.title IS 'Its title.';
COMMENT ON COLUMN query.records.description IS 'Its description.';
COMMENT ON COLUMN query.records.status IS 'Its status, a name from its type''s lifecycle (new, in_progress, resolved...).';
COMMENT ON COLUMN query.records.state_category IS 'What the status means: open, waiting, done or closed. Open work is open or waiting.';
COMMENT ON COLUMN query.records.priority IS 'Priority, 1 (critical) to 4 (low).';
COMMENT ON COLUMN query.records.requester_id IS 'Who it is for (people.id).';
COMMENT ON COLUMN query.records.assignee_id IS 'Who is working it (people.id); null when nobody is.';
COMMENT ON COLUMN query.records.assignment_group_id IS 'The team it is routed to (groups.id); null when none.';
COMMENT ON COLUMN query.records.parent_id IS 'For a task, the request it is part of (records.id).';
COMMENT ON COLUMN query.records.fields IS 'The extra fields its form declares, as a JSON object: fields->>''category''.';
COMMENT ON COLUMN query.records.created_at IS 'When it was filed.';
COMMENT ON COLUMN query.records.updated_at IS 'When it last changed.';

CREATE VIEW query.incidents WITH (security_barrier) AS
    SELECT i.task_id AS record_id, i.resolution, i.problem_id
    FROM incidents i JOIN query.records r ON r.id = i.task_id;
COMMENT ON VIEW query.incidents IS 'What incidents have beyond every record''s columns. Join records on records.id = incidents.record_id.';
COMMENT ON COLUMN query.incidents.record_id IS 'The incident (records.id).';
COMMENT ON COLUMN query.incidents.resolution IS 'How it was resolved; empty until it is.';
COMMENT ON COLUMN query.incidents.problem_id IS 'The problem behind it (records.id); null when none.';

CREATE VIEW query.changes WITH (security_barrier) AS
    SELECT c.task_id AS record_id, c.change_type, c.risk, c.impact, c.planned_start, c.planned_end,
           c.implementation_plan, c.backout_plan, c.test_plan, c.problem_id
    FROM changes c JOIN query.records r ON r.id = c.task_id;
COMMENT ON VIEW query.changes IS 'What changes have beyond every record''s columns. Join records on records.id = changes.record_id.';
COMMENT ON COLUMN query.changes.record_id IS 'The change (records.id).';
COMMENT ON COLUMN query.changes.change_type IS 'standard, normal or emergency.';
COMMENT ON COLUMN query.changes.risk IS 'low, medium or high; empty until assessed.';
COMMENT ON COLUMN query.changes.impact IS 'low, medium or high; empty until assessed.';
COMMENT ON COLUMN query.changes.planned_start IS 'When the work is planned to start.';
COMMENT ON COLUMN query.changes.planned_end IS 'When the work is planned to end.';
COMMENT ON COLUMN query.changes.implementation_plan IS 'How it is done.';
COMMENT ON COLUMN query.changes.backout_plan IS 'How it is undone.';
COMMENT ON COLUMN query.changes.test_plan IS 'How it is tested.';
COMMENT ON COLUMN query.changes.problem_id IS 'The problem it fixes (records.id); null when none.';

CREATE VIEW query.requests WITH (security_barrier) AS
    SELECT q.task_id AS record_id, q.catalog_item
    FROM requests q JOIN query.records r ON r.id = q.task_id;
COMMENT ON VIEW query.requests IS 'What requests have beyond every record''s columns. Their tasks are records whose parent_id is the request.';
COMMENT ON COLUMN query.requests.record_id IS 'The request (records.id).';
COMMENT ON COLUMN query.requests.catalog_item IS 'The catalog item it was ordered from; empty when staff filed it by hand.';

CREATE VIEW query.comments WITH (security_barrier) AS
    SELECT c.id, c.task_id AS record_id, c.author_id, c.on_behalf_of, c.body, c.internal, c.created_at
    FROM app_query_identity() me
    JOIN task_comments c ON NOT c.internal OR position(',comments:internal,' IN me.perms) > 0
    JOIN query.records r ON r.id = c.task_id;
COMMENT ON VIEW query.comments IS 'Comments on the records you can see. Internal notes only if you may read them.';
COMMENT ON COLUMN query.comments.id IS 'The comment''s id.';
COMMENT ON COLUMN query.comments.record_id IS 'The record it is on (records.id).';
COMMENT ON COLUMN query.comments.author_id IS 'Who wrote it (people.id), or the app that did.';
COMMENT ON COLUMN query.comments.on_behalf_of IS 'When an app wrote it, the person it acted for; else null.';
COMMENT ON COLUMN query.comments.body IS 'What it says.';
COMMENT ON COLUMN query.comments.internal IS 'Whether it is an internal note, hidden from the requester.';
COMMENT ON COLUMN query.comments.created_at IS 'When it was written.';

CREATE VIEW query.attachments WITH (security_barrier) AS
    SELECT a.id, a.task_id AS record_id, a.filename, a.content_type, a.size, a.created_by, a.created_at
    FROM attachments a JOIN query.records r ON r.id = a.task_id
    WHERE a.status = 'ready';
COMMENT ON VIEW query.attachments IS 'Files attached to the records you can see (finished uploads only; the files themselves are not here).';
COMMENT ON COLUMN query.attachments.id IS 'The attachment''s id.';
COMMENT ON COLUMN query.attachments.record_id IS 'The record it is on (records.id).';
COMMENT ON COLUMN query.attachments.filename IS 'The file''s name.';
COMMENT ON COLUMN query.attachments.content_type IS 'Its media type, as uploaded.';
COMMENT ON COLUMN query.attachments.size IS 'Its size in bytes.';
COMMENT ON COLUMN query.attachments.created_by IS 'Who attached it (people.id).';
COMMENT ON COLUMN query.attachments.created_at IS 'When it was attached.';

CREATE VIEW query.slas WITH (security_barrier) AS
    SELECT s.id, s.task_id AS record_id, s.app, s.sla, coalesce(s.definition ->> 'label', s.sla) AS label, s.stage,
           s.target_ms, s.elapsed_ms, s.running_since, s.due_at, s.breached_at, s.calendar, s.problem,
           s.started_at, s.stopped_at
    FROM task_slas s JOIN query.records r ON r.id = s.task_id;
COMMENT ON VIEW query.slas IS 'SLA clocks on the records you can see: one per record and SLA. A clock is breached when breached_at is set.';
COMMENT ON COLUMN query.slas.id IS 'The clock''s id.';
COMMENT ON COLUMN query.slas.record_id IS 'The record it times (records.id).';
COMMENT ON COLUMN query.slas.app IS 'The app that defines the SLA.';
COMMENT ON COLUMN query.slas.sla IS 'The SLA''s name within its app (respond, resolve...).';
COMMENT ON COLUMN query.slas.label IS 'The SLA''s name for people, as it was when the clock started.';
COMMENT ON COLUMN query.slas.stage IS 'running, paused, stopped or cancelled.';
COMMENT ON COLUMN query.slas.target_ms IS 'How much working time it allows, in milliseconds.';
COMMENT ON COLUMN query.slas.elapsed_ms IS 'Working time used before running_since, in milliseconds. While running, the time used is this plus the working time since running_since.';
COMMENT ON COLUMN query.slas.running_since IS 'When it last started running; null unless running.';
COMMENT ON COLUMN query.slas.due_at IS 'When the target runs out at the current rate; null unless running.';
COMMENT ON COLUMN query.slas.breached_at IS 'When it ran past its target; null if it has not.';
COMMENT ON COLUMN query.slas.calendar IS 'The business calendar it counts by; empty counts every hour.';
COMMENT ON COLUMN query.slas.problem IS 'Why the clock could not be worked out on the last save; empty when all is well.';
COMMENT ON COLUMN query.slas.started_at IS 'When the clock started.';
COMMENT ON COLUMN query.slas.stopped_at IS 'When it stopped; null while running or paused.';

CREATE VIEW query.people WITH (security_barrier) AS
    SELECT u.id, u.name, u.email, u.role, u.manager_id, u.deactivated_at,
           EXISTS (SELECT 1 FROM service_accounts s WHERE s.user_id = u.id) AS service_account, u.created_at
    FROM app_query_identity() me
    JOIN users u ON u.id = me.user_id OR position(',users:read,' IN me.perms) > 0
    WHERE u.app_name IS NULL;
COMMENT ON VIEW query.people IS 'People: everyone if you may read the directory, otherwise only you. Records name them by id (requester_id, assignee_id...).';
COMMENT ON COLUMN query.people.id IS 'Their id.';
COMMENT ON COLUMN query.people.name IS 'Their name.';
COMMENT ON COLUMN query.people.email IS 'Their email address.';
COMMENT ON COLUMN query.people.role IS 'Their base role: admin, agent or end_user. Roles granted on top are not shown.';
COMMENT ON COLUMN query.people.manager_id IS 'Their manager (people.id); null when none is set.';
COMMENT ON COLUMN query.people.deactivated_at IS 'When they were deactivated; null while active.';
COMMENT ON COLUMN query.people.service_account IS 'Whether this is a service account (signs in with API keys only).';
COMMENT ON COLUMN query.people.created_at IS 'When they were added.';

CREATE VIEW query.groups WITH (security_barrier) AS
    SELECT g.id, g.name, g.description, g.restricted, g.created_at
    FROM app_query_identity() me, groups g;
COMMENT ON VIEW query.groups IS 'Groups (teams) records are routed to.';
COMMENT ON COLUMN query.groups.id IS 'The group''s id.';
COMMENT ON COLUMN query.groups.name IS 'Its name.';
COMMENT ON COLUMN query.groups.description IS 'What it does.';
COMMENT ON COLUMN query.groups.restricted IS 'Whether records routed to it are kept to its members.';
COMMENT ON COLUMN query.groups.created_at IS 'When it was made.';

CREATE VIEW query.group_members WITH (security_barrier) AS
    SELECT m.group_id, m.user_id, m.added_at
    FROM app_query_identity() me, group_members m;
COMMENT ON VIEW query.group_members IS 'Who is in which group.';
COMMENT ON COLUMN query.group_members.group_id IS 'The group (groups.id).';
COMMENT ON COLUMN query.group_members.user_id IS 'The member (people.id).';
COMMENT ON COLUMN query.group_members.added_at IS 'When they joined it.';

CREATE VIEW query.cis WITH (security_barrier) AS
    SELECT c.id, c.class, c.name, c.status, c.description, c.owner_group_id, c.support_group_id, c.fields,
           c.created_at, c.updated_at
    FROM app_query_identity() me
    JOIN cis c ON position(',cmdb:read,' IN me.perms) > 0;
COMMENT ON VIEW query.cis IS 'Configuration items in the CMDB, if you may read it.';
COMMENT ON COLUMN query.cis.id IS 'The item''s id.';
COMMENT ON COLUMN query.cis.class IS 'Its class (server, application...), defined by an app.';
COMMENT ON COLUMN query.cis.name IS 'Its name, unique within its class.';
COMMENT ON COLUMN query.cis.status IS 'planned, active, maintenance or retired.';
COMMENT ON COLUMN query.cis.description IS 'What it is.';
COMMENT ON COLUMN query.cis.owner_group_id IS 'The group that owns it (groups.id).';
COMMENT ON COLUMN query.cis.support_group_id IS 'The group that supports it, where its records are routed (groups.id).';
COMMENT ON COLUMN query.cis.fields IS 'Its class''s fields, as a JSON object.';
COMMENT ON COLUMN query.cis.created_at IS 'When it was added.';
COMMENT ON COLUMN query.cis.updated_at IS 'When it last changed.';

CREATE VIEW query.ci_relationships WITH (security_barrier) AS
    SELECT r.id, r.from_ci, r.to_ci, r.type, r.created_at
    FROM app_query_identity() me
    JOIN ci_relationships r ON position(',cmdb:read,' IN me.perms) > 0;
COMMENT ON VIEW query.ci_relationships IS 'How items relate, read forwards: from_ci <type> to_ci ("payroll depends on postgres-01").';
COMMENT ON COLUMN query.ci_relationships.id IS 'The relationship''s id.';
COMMENT ON COLUMN query.ci_relationships.from_ci IS 'The item it starts from (cis.id).';
COMMENT ON COLUMN query.ci_relationships.to_ci IS 'The item it points to (cis.id).';
COMMENT ON COLUMN query.ci_relationships.type IS 'The relationship type (depends_on, runs_on...), defined by an app.';
COMMENT ON COLUMN query.ci_relationships.created_at IS 'When it was added.';

CREATE VIEW query.record_cis WITH (security_barrier) AS
    SELECT l.task_id AS record_id, l.ci_id, l.created_at
    FROM app_query_identity() me
    JOIN task_cis l ON position(',cmdb:read,' IN me.perms) > 0
    JOIN query.records r ON r.id = l.task_id;
COMMENT ON VIEW query.record_cis IS 'Which items records affect, for the records you can see, if you may read the CMDB.';
COMMENT ON COLUMN query.record_cis.record_id IS 'The record (records.id).';
COMMENT ON COLUMN query.record_cis.ci_id IS 'The item (cis.id).';
COMMENT ON COLUMN query.record_cis.created_at IS 'When it was linked.';

CREATE VIEW query.articles WITH (security_barrier) AS
    SELECT a.id, 'KB' || a.number::text AS reference, a.number, a.title, a.body, a.category, a.tags, a.audience,
           a.status, a.version, a.author_id, a.created_at, a.updated_at, a.published_at
    FROM app_query_identity() me
    JOIN kb_articles a ON CASE
        WHEN a.status = 'published' THEN a.audience = 'everyone'
             OR position(',knowledge:read_internal,' IN me.perms) > 0 OR position(',knowledge:publish,' IN me.perms) > 0
        ELSE position(',knowledge:write,' IN me.perms) > 0 OR position(',knowledge:publish,' IN me.perms) > 0
    END;
COMMENT ON VIEW query.articles IS 'Knowledge articles you can read: published ones for their audience, drafts and retired ones if you write or publish articles.';
COMMENT ON COLUMN query.articles.id IS 'The article''s id.';
COMMENT ON COLUMN query.articles.reference IS 'How people name it (KB12).';
COMMENT ON COLUMN query.articles.number IS 'Its number.';
COMMENT ON COLUMN query.articles.title IS 'Its title.';
COMMENT ON COLUMN query.articles.body IS 'Its text, in Markdown.';
COMMENT ON COLUMN query.articles.category IS 'Its category; empty when none.';
COMMENT ON COLUMN query.articles.tags IS 'Its tags, a text array.';
COMMENT ON COLUMN query.articles.audience IS 'everyone, or internal (staff only).';
COMMENT ON COLUMN query.articles.status IS 'draft, review, published or retired.';
COMMENT ON COLUMN query.articles.version IS 'How many times its content has been saved.';
COMMENT ON COLUMN query.articles.author_id IS 'Who wrote it (people.id).';
COMMENT ON COLUMN query.articles.created_at IS 'When it was started.';
COMMENT ON COLUMN query.articles.updated_at IS 'When it last changed.';
COMMENT ON COLUMN query.articles.published_at IS 'When it was first published; null if never.';

CREATE VIEW query.record_articles WITH (security_barrier) AS
    SELECT l.task_id AS record_id, l.article_id, l.created_at
    FROM task_articles l
    JOIN query.records r ON r.id = l.task_id
    JOIN query.articles a ON a.id = l.article_id;
COMMENT ON VIEW query.record_articles IS 'Which articles answer which records, where you can see both.';
COMMENT ON COLUMN query.record_articles.record_id IS 'The record (records.id).';
COMMENT ON COLUMN query.record_articles.article_id IS 'The article (articles.id).';
COMMENT ON COLUMN query.record_articles.created_at IS 'When they were linked.';

CREATE VIEW query.history WITH (security_barrier) AS
    SELECT h.id, h.subject, h.subject_id, h.at, h.actor_id, h.on_behalf_of, h.action, h.field, h.old, h.new
    FROM app_query_identity() me
    JOIN record_history h ON CASE h.subject
        WHEN 'task' THEN EXISTS (SELECT 1 FROM query.records r WHERE r.id = h.subject_id)
        WHEN 'ci' THEN position(',cmdb:read,' IN me.perms) > 0
        WHEN 'setting' THEN position(',settings:manage,' IN me.perms) > 0
        ELSE false
    END;
COMMENT ON VIEW query.history IS 'What changed, field by field: on the records you can see (subject task), on items if you may read the CMDB (ci), and on the history settings if you manage settings (setting).';
COMMENT ON COLUMN query.history.id IS 'The entry''s id; later entries have larger ids.';
COMMENT ON COLUMN query.history.subject IS 'What changed: task (a record), ci (an item) or setting.';
COMMENT ON COLUMN query.history.subject_id IS 'Its id: records.id or cis.id.';
COMMENT ON COLUMN query.history.at IS 'When.';
COMMENT ON COLUMN query.history.actor_id IS 'Who did it (people.id), or the app that did; null for the system.';
COMMENT ON COLUMN query.history.on_behalf_of IS 'When an app did it, the person it acted for.';
COMMENT ON COLUMN query.history.action IS 'created, changed, linked, unlinked or deleted.';
COMMENT ON COLUMN query.history.field IS 'What changed: a column (status, priority), an extra field (fields.category) or what was linked (ci, article, attachment...).';
COMMENT ON COLUMN query.history.old IS 'The value before, as JSON; null when there was none.';
COMMENT ON COLUMN query.history.new IS 'The value after, as JSON; null when there is none.';

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
    -- The chart it is drawn as in the workbench (line, bars, pie, a single figure..., and which columns), so whoever
    -- opens it sees the same chart. Null: shown as a table. Checked against its columns by the server on every save
    -- (internal/api/views.go, query.Chart).
    chart       jsonb CHECK (chart IS NULL OR jsonb_typeof(chart) = 'object'),
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
DROP VIEW query.history;
DROP VIEW query.record_articles;
DROP VIEW query.articles;
DROP VIEW query.record_cis;
DROP VIEW query.ci_relationships;
DROP VIEW query.cis;
DROP VIEW query.group_members;
DROP VIEW query.groups;
DROP VIEW query.people;
DROP VIEW query.slas;
DROP VIEW query.attachments;
DROP VIEW query.comments;
DROP VIEW query.requests;
DROP VIEW query.changes;
DROP VIEW query.incidents;
DROP VIEW query.records;
DROP VIEW query.record_types;
DROP TABLE query_runs;
DROP FUNCTION app_query_identity();
DROP TABLE query_keys;
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA query REVOKE SELECT ON TABLES FROM %I', current_user, app_query_role());
END
$$;
-- +goose StatementEnd
DROP SCHEMA query;
-- +goose StatementBegin
DO $$
DECLARE r text := app_query_role();
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = r) THEN
        EXECUTE format('ALTER ROLE %I IN DATABASE %I RESET ALL', r, current_database());
        EXECUTE format('DROP ROLE %I', r);
    END IF;
END
$$;
-- +goose StatementEnd
DROP FUNCTION app_query_role();
