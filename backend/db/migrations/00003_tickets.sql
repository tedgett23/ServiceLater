-- +goose Up
CREATE TABLE tickets (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number       bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    title        text NOT NULL,
    description  text NOT NULL DEFAULT '',
    status       text NOT NULL DEFAULT 'new' CHECK (status IN ('new', 'in_progress', 'on_hold', 'resolved', 'closed')),
    priority     smallint NOT NULL DEFAULT 3 CHECK (priority BETWEEN 1 AND 4),
    requester_id uuid NOT NULL REFERENCES users (id),
    assignee_id  uuid REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX tickets_requester_idx ON tickets (requester_id);
CREATE INDEX tickets_assignee_idx ON tickets (assignee_id) WHERE assignee_id IS NOT NULL;
CREATE INDEX tickets_status_idx ON tickets (status);

ALTER TABLE tickets ENABLE ROW LEVEL SECURITY;

-- Staff (admin/agent) see everything; end users see only their own tickets.
CREATE POLICY tickets_select ON tickets FOR SELECT TO servicelater_app
    USING (app_is_staff() OR requester_id = app_current_user_id());
CREATE POLICY tickets_insert ON tickets FOR INSERT TO servicelater_app
    WITH CHECK (app_is_staff() OR requester_id = app_current_user_id());
CREATE POLICY tickets_update ON tickets FOR UPDATE TO servicelater_app
    USING (app_is_staff()) WITH CHECK (app_is_staff());
CREATE POLICY tickets_delete ON tickets FOR DELETE TO servicelater_app USING (app_is_admin());

-- +goose StatementBegin
CREATE FUNCTION tickets_touch_and_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        NEW.updated_at = now();
    END IF;
    PERFORM pg_notify('ticket_changes', json_build_object(
        'op', TG_OP, 'id', NEW.id, 'number', NEW.number, 'status', NEW.status)::text);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER tickets_touch_and_notify BEFORE INSERT OR UPDATE ON tickets
    FOR EACH ROW EXECUTE FUNCTION tickets_touch_and_notify();

-- +goose Down
DROP TABLE tickets;
DROP FUNCTION tickets_touch_and_notify();
