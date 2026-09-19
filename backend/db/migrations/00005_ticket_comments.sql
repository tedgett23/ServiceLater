-- +goose Up
CREATE TABLE ticket_comments (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id  uuid NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    author_id  uuid NOT NULL REFERENCES users (id),
    body       text NOT NULL CHECK (length(body) BETWEEN 1 AND 10000),
    internal   boolean NOT NULL DEFAULT false, -- work notes: staff only
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ticket_comments_ticket_idx ON ticket_comments (ticket_id, created_at);

ALTER TABLE ticket_comments ENABLE ROW LEVEL SECURITY;

-- A comment is visible iff its ticket is visible to the caller (tickets RLS applies inside the subquery)
-- and, for internal notes, the caller is staff.
CREATE POLICY comments_select ON ticket_comments FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tickets t WHERE t.id = ticket_id) AND (NOT internal OR app_is_staff()));
CREATE POLICY comments_insert ON ticket_comments FOR INSERT TO servicelater_app
    WITH CHECK (author_id = app_current_user_id()
                AND EXISTS (SELECT 1 FROM tickets t WHERE t.id = ticket_id)
                AND (NOT internal OR app_is_staff()));
CREATE POLICY comments_delete ON ticket_comments FOR DELETE TO servicelater_app USING (app_is_admin());

-- Comments are append-only for now (no UPDATE policy). The payload carries ids only, never the body:
-- the realtime hub must authorise each subscriber before forwarding.
-- +goose StatementBegin
CREATE FUNCTION comments_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('comment_changes', json_build_object(
        'op', TG_OP, 'id', NEW.id, 'ticket_id', NEW.ticket_id, 'internal', NEW.internal)::text);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER comments_notify AFTER INSERT ON ticket_comments
    FOR EACH ROW EXECUTE FUNCTION comments_notify();

-- +goose Down
DROP TABLE ticket_comments;
DROP FUNCTION comments_notify();
