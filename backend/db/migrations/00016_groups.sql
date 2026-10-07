-- +goose Up

-- Groups are teams of people: who a ticket is routed to (its assignment group) and who a workflow asks (group() in
-- an approval's approvers). Membership is open to anyone, not just staff, because an approver need not be an agent.
CREATE TABLE groups (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
    description text NOT NULL DEFAULT '' CHECK (length(description) <= 1000),
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

-- Like the user directory, everyone signed in may read groups and who is in them: a workflow runs as the person who
-- started it, who may be an end user, and its group() must still resolve. Only admins change them.
ALTER TABLE groups ENABLE ROW LEVEL SECURITY;
CREATE POLICY groups_select ON groups FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
CREATE POLICY groups_insert ON groups FOR INSERT TO servicelater_app WITH CHECK (app_is_admin());
CREATE POLICY groups_update ON groups FOR UPDATE TO servicelater_app USING (app_is_admin()) WITH CHECK (app_is_admin());
CREATE POLICY groups_delete ON groups FOR DELETE TO servicelater_app USING (app_is_admin());

ALTER TABLE group_members ENABLE ROW LEVEL SECURITY;
CREATE POLICY group_members_select ON group_members FOR SELECT TO servicelater_app USING (app_current_user_id() IS NOT NULL);
CREATE POLICY group_members_insert ON group_members FOR INSERT TO servicelater_app WITH CHECK (app_is_admin());
CREATE POLICY group_members_delete ON group_members FOR DELETE TO servicelater_app USING (app_is_admin());

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

-- The team a ticket is routed to. A group with tickets cannot be deleted (no cascade): those tickets would change
-- without passing through the save path, its rules and its audit.
ALTER TABLE tasks ADD COLUMN assignment_group_id uuid REFERENCES groups (id);
CREATE INDEX tasks_assignment_group_idx ON tasks (assignment_group_id) WHERE assignment_group_id IS NOT NULL;

-- +goose Down
DROP INDEX tasks_assignment_group_idx;
ALTER TABLE tasks DROP COLUMN assignment_group_id;
DROP TABLE group_members;
DROP TABLE groups;
DROP FUNCTION groups_touch();
