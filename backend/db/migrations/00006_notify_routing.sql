-- +goose Up

-- Realtime routing needs to know who may hear about a change without asking the database per socket.
-- The payloads still carry ids only: clients refetch through the RLS-protected API.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tickets_touch_and_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        NEW.updated_at = now();
    END IF;
    PERFORM pg_notify('ticket_changes', json_build_object(
        'op', TG_OP, 'id', NEW.id, 'number', NEW.number, 'status', NEW.status,
        'requester_id', NEW.requester_id, 'assignee_id', NEW.assignee_id)::text);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION comments_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('comment_changes', json_build_object(
        'op', TG_OP, 'id', NEW.id, 'ticket_id', NEW.ticket_id, 'internal', NEW.internal,
        'requester_id', (SELECT requester_id FROM tickets WHERE id = NEW.ticket_id))::text);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tickets_touch_and_notify() RETURNS trigger
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION comments_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('comment_changes', json_build_object(
        'op', TG_OP, 'id', NEW.id, 'ticket_id', NEW.ticket_id, 'internal', NEW.internal)::text);
    RETURN NEW;
END
$$;
-- +goose StatementEnd
