-- +goose Up
-- People, their credentials and sessions, the working hours teams keep, and the teams (groups) themselves.

CREATE TABLE users (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email               text NOT NULL UNIQUE,
    name                text NOT NULL,
    -- The base role; roles granted on top of it (access) add permissions. Only roles:manage may make or unmake an
    -- admin, and only users:manage may change a role (users_guard).
    role                text NOT NULL DEFAULT 'end_user' CHECK (role IN ('admin', 'agent', 'end_user')),
    -- Who approves what this person requests. Set only by those who manage people: otherwise anyone could name their
    -- own approver.
    manager_id          uuid REFERENCES users (id) ON DELETE SET NULL CHECK (manager_id <> id),
    -- An app's service principal (docs/apps-platform.md §11, D9): a row with app_name set cannot sign in (no
    -- credentials, and login refuses it), never shows in the directory or pickers, and holds exactly the permissions its
    -- manifest declares, approved at install. Async rules and workflows run as it, on behalf of the person who caused
    -- them.
    app_name            text UNIQUE,
    -- Deactivate, don't delete: a deactivated person cannot sign in, loses any staff or admin power at once, and drops
    -- out of pickers and group() approvers, while their name stays on what they did. Deleting is only possible for
    -- someone nothing references.
    deactivated_at      timestamptz,
    -- Whether notifications also reach them by email (their own row; users_update allows it).
    email_notifications boolean NOT NULL DEFAULT true,
    -- How they want the web UI drawn: following their device (auto), or always light or dark. Kept on the user so it
    -- follows them between browsers.
    theme               text NOT NULL DEFAULT 'auto' CONSTRAINT users_theme CHECK (theme IN ('auto', 'light', 'dark')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_app_principal_check CHECK (app_name IS NULL OR role = 'end_user')
);
CREATE INDEX users_manager_idx ON users (manager_id) WHERE manager_id IS NOT NULL;

-- The directory: anyone signed in reads people (assignee and requester names, pickers, approvers). Nobody else's
-- secrets live here: passwords are in user_credentials.
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
CREATE POLICY users_select ON users FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
CREATE POLICY users_insert ON users FOR INSERT TO servicelater_app WITH CHECK (app_can('users:manage'));
CREATE POLICY users_update ON users FOR UPDATE TO servicelater_app
    USING (id = app_current_user_id() OR app_can('users:manage'))
    WITH CHECK (id = app_current_user_id() OR app_can('users:manage'));
CREATE POLICY users_delete ON users FOR DELETE TO servicelater_app USING (app_can('users:manage'));

-- There is always at least one active admin, or nobody could manage anyone again. The lock serialises admin changes
-- so two admins demoting each other at once cannot both succeed; each statement in plpgsql sees what committed
-- before it took the lock.
-- +goose StatementBegin
CREATE FUNCTION users_keep_an_admin() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.role <> 'admin' OR OLD.deactivated_at IS NOT NULL THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.role = 'admin' AND NEW.deactivated_at IS NULL THEN
        RETURN NULL;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('servicelater.admins'));
    IF NOT EXISTS (SELECT 1 FROM users WHERE role = 'admin' AND deactivated_at IS NULL AND id <> OLD.id) THEN
        RAISE EXCEPTION 'there must be at least one active admin'
            USING ERRCODE = '23514', CONSTRAINT = 'users_last_admin';
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER users_keep_an_admin AFTER UPDATE OR DELETE ON users FOR EACH ROW EXECUTE FUNCTION users_keep_an_admin();

-- Credentials, sessions and rate limits are owner-only: RLS on with no policies and no grants for the app role, so
-- only db.WithSystem touches them. (The directory policy lets anyone signed in read users rows, which is why password
-- hashes are not on users.)
CREATE TABLE user_credentials (
    user_id       uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    password_hash text NOT NULL,
    updated_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE user_credentials ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON user_credentials FROM servicelater_app;

-- Refresh-token sessions. Unlogged: lost on a crash, people simply sign in again. One row per issued refresh token;
-- rotation inserts a new row in the same family.
CREATE UNLOGGED TABLE sessions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id  uuid NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    rotated_at timestamptz
);
CREATE INDEX sessions_family_idx ON sessions (family_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);
ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON sessions FROM servicelater_app;

CREATE UNLOGGED TABLE rate_limits (
    key          text PRIMARY KEY,
    count        integer NOT NULL,
    window_start timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE rate_limits ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON rate_limits FROM servicelater_app;

-- An admin setting someone's password, or cutting off a deactivated person's sessions, goes through these, in the
-- same transaction as the rest of the change. A service account never gets a password.
-- +goose StatementBegin
CREATE FUNCTION app_set_password(p_user_id uuid, p_hash text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT app_can('users:manage') THEN
        RAISE EXCEPTION 'only an admin may set a password' USING ERRCODE = '42501';
    END IF;
    IF EXISTS (SELECT 1 FROM service_accounts WHERE user_id = p_user_id) THEN
        RAISE EXCEPTION 'a service account signs in with API keys, not a password' USING ERRCODE = '22023';
    END IF;
    INSERT INTO user_credentials (user_id, password_hash) VALUES (p_user_id, p_hash)
    ON CONFLICT (user_id) DO UPDATE SET password_hash = EXCLUDED.password_hash, updated_at = now();
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_revoke_sessions(p_user_id uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT app_can('users:manage') THEN
        RAISE EXCEPTION 'only an admin may sign someone out' USING ERRCODE = '42501';
    END IF;
    DELETE FROM sessions WHERE user_id = p_user_id;
END $$;
-- +goose StatementEnd

-- Business calendars: working hours in a time zone, less holidays, which SLA clocks count and groups keep. Like freeze
-- windows they are reference data people edit at run time (holidays change every year), which an app cannot define a
-- table for yet (docs/apps-platform.md, open question 11), so the table is core. A calendar that is missing when a
-- clock needs it counts every hour, and the problem is logged.
CREATE TABLE business_calendars (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- What an SLA definition writes after calendar: (lowercase letters, digits and hyphens).
    name       text NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9-]{1,62}$'),
    label      text NOT NULL CHECK (length(btrim(label)) BETWEEN 1 AND 200),
    -- An IANA zone (Europe/London); internal/api checks it, since Postgres has no immutable test for one.
    time_zone  text NOT NULL,
    -- The working week: [{"day": 1, "start": "09:00", "end": "17:00"}, ...], day 0 = Sunday.
    hours      jsonb NOT NULL CHECK (jsonb_typeof(hours) = 'array' AND jsonb_array_length(hours) BETWEEN 1 AND 70),
    holidays   date[] NOT NULL DEFAULT '{}' CHECK (cardinality(holidays) <= 1000),
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Everyone signed in reads them (a save as anyone, even an end user filing an incident, starts a clock that needs its
-- calendar); calendars:manage writes them.
ALTER TABLE business_calendars ENABLE ROW LEVEL SECURITY;
CREATE POLICY business_calendars_select ON business_calendars FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
CREATE POLICY business_calendars_insert ON business_calendars FOR INSERT TO servicelater_app WITH CHECK (app_can('calendars:manage'));
CREATE POLICY business_calendars_update ON business_calendars FOR UPDATE TO servicelater_app
    USING (app_can('calendars:manage')) WITH CHECK (app_can('calendars:manage'));
CREATE POLICY business_calendars_delete ON business_calendars FOR DELETE TO servicelater_app USING (app_can('calendars:manage'));

-- The calendars that ship: Business hours (8x5, Monday to Friday 09:00 to 17:00 UTC, which incident-management's
-- SLAs name) and Extended hours (12x5, 07:00 to 19:00). Every hour (24x7) is no calendar at all. Operators change
-- hours, zone and holidays to match their own; this never overwrites them. Tests call it again after wiping users,
-- which takes the calendars with them.
-- +goose StatementBegin
CREATE FUNCTION app_seed_business_calendars() RETURNS void
    LANGUAGE sql
    AS $$
    INSERT INTO business_calendars (name, label, time_zone, hours) VALUES
        ('business-hours', 'Business hours', 'UTC',
         '[{"day":1,"start":"09:00","end":"17:00"},{"day":2,"start":"09:00","end":"17:00"},{"day":3,"start":"09:00","end":"17:00"},
           {"day":4,"start":"09:00","end":"17:00"},{"day":5,"start":"09:00","end":"17:00"}]'),
        ('extended-hours', 'Extended hours', 'UTC',
         '[{"day":1,"start":"07:00","end":"19:00"},{"day":2,"start":"07:00","end":"19:00"},{"day":3,"start":"07:00","end":"19:00"},
           {"day":4,"start":"07:00","end":"19:00"},{"day":5,"start":"07:00","end":"19:00"}]')
    ON CONFLICT (name) DO NOTHING;
$$;
-- +goose StatementEnd
SELECT app_seed_business_calendars();

-- Groups are teams of people: who a record is routed to (its assignment group) and who a workflow asks (group() in an
-- approval's approvers). Membership is open to anyone, not just staff, because an approver need not be an agent.
CREATE TABLE groups (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
    description text NOT NULL DEFAULT '' CHECK (length(description) <= 1000),
    -- A restricted group (HR, Security) keeps the records routed to it to its members: other staff who may read the
    -- record type do not see them. The requester always sees their own, the assignee what is assigned to them, and
    -- anyone holding restricted:read sees past it. Restricting or lifting needs roles:manage (groups_guard_restricted).
    restricted  boolean NOT NULL DEFAULT false,
    -- The group's own working hours: what an SLA counting "the assignment group's calendar" uses. A calendar a group
    -- works by cannot be deleted.
    calendar_id uuid REFERENCES business_calendars (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
-- Expressions name a group (group('CAB')), so a name is unique whatever its case.
CREATE UNIQUE INDEX groups_name_key ON groups (lower(name));

CREATE TABLE group_members (
    group_id uuid NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    user_id  uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    added_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX group_members_user_idx ON group_members (user_id);

-- Like the directory, everyone signed in may read groups and who is in them: a workflow runs as the person who started
-- it, who may be an end user, and its group() must still resolve. groups:manage changes them. Changing the members of
-- a group that holds a role, or of a restricted group, also needs roles:manage: otherwise managing groups would be a
-- way to hand out roles or a view of restricted records.
ALTER TABLE groups ENABLE ROW LEVEL SECURITY;
CREATE POLICY groups_select ON groups FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
CREATE POLICY groups_insert ON groups FOR INSERT TO servicelater_app WITH CHECK (app_can('groups:manage'));
CREATE POLICY groups_update ON groups FOR UPDATE TO servicelater_app USING (app_can('groups:manage')) WITH CHECK (app_can('groups:manage'));
CREATE POLICY groups_delete ON groups FOR DELETE TO servicelater_app USING (app_can('groups:manage'));

ALTER TABLE group_members ENABLE ROW LEVEL SECURITY;
CREATE POLICY group_members_select ON group_members FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);

-- +goose StatementBegin
CREATE FUNCTION groups_touch() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER groups_touch BEFORE UPDATE ON groups FOR EACH ROW EXECUTE FUNCTION groups_touch();

-- +goose Down
DROP TABLE group_members;
DROP TABLE groups;
DROP FUNCTION groups_touch();
DROP TABLE business_calendars;
DROP FUNCTION app_seed_business_calendars();
DROP FUNCTION app_revoke_sessions(uuid);
DROP FUNCTION app_set_password(uuid, text);
DROP TABLE rate_limits;
DROP TABLE sessions;
DROP TABLE user_credentials;
DROP TABLE users;
DROP FUNCTION users_keep_an_admin();
