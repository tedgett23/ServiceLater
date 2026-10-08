-- +goose Up

-- Files attached to records. The bytes live in the storage backend the instance uses (a folder or an S3 bucket,
-- internal/storage); a row holds what the file is, where it went (the backend and a key relative to it, never an
-- absolute path), and whether its upload has finished. A pending row is an upload in flight: only its uploader sees
-- it, and it is removed if it is not finished within a day.
CREATE TABLE attachments (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id      uuid NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    filename     text NOT NULL CHECK (length(filename) BETWEEN 1 AND 255),
    content_type text NOT NULL CHECK (length(content_type) BETWEEN 1 AND 255),
    size         bigint NOT NULL CHECK (size > 0),
    backend      text NOT NULL CHECK (backend IN ('local', 's3')),
    key          text NOT NULL CHECK (key ~ '^[A-Za-z0-9/._-]+$' AND length(key) <= 1024),
    -- The upload in flight: S3's multipart upload id (empty for a folder), and the size of every part but the last.
    upload_id    text NOT NULL DEFAULT '',
    part_size    bigint NOT NULL CHECK (part_size > 0),
    status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ready')),
    -- No ON DELETE action: like a comment, an attachment keeps who added it (deleting them is a 409).
    created_by   uuid NOT NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    ready_at     timestamptz,
    UNIQUE (backend, key)
);
CREATE INDEX attachments_task_idx ON attachments (task_id, created_at);
CREATE INDEX attachments_pending_idx ON attachments (created_by) WHERE status = 'pending';

-- Seen by whoever sees the record (an upload in flight only by its uploader); added by the record's requester or
-- whoever may update it; finished by its uploader; removed by its uploader or whoever may update the record.
ALTER TABLE attachments ENABLE ROW LEVEL SECURITY;
CREATE POLICY attachments_select ON attachments FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id)
           AND (status = 'ready' OR created_by = app_current_user_id()));
CREATE POLICY attachments_insert ON attachments FOR INSERT TO servicelater_app
    WITH CHECK (created_by = app_current_user_id() AND status = 'pending'
                AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id
                            AND (t.requester_id = app_current_user_id() OR app_can_record('update', t.type))));
CREATE POLICY attachments_update ON attachments FOR UPDATE TO servicelater_app
    USING (created_by = app_current_user_id() AND status = 'pending')
    WITH CHECK (created_by = app_current_user_id());
CREATE POLICY attachments_delete ON attachments FOR DELETE TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id
                   AND (created_by = app_current_user_id() OR app_can_record('update', t.type))));

-- A finished upload is in the record's history and tells its watchers; so is removing one. However a row goes (a
-- person, an expired upload, the record's deletion), its file is removed by a job, so storage never keeps a file
-- nothing refers to.
-- +goose StatementBegin
CREATE FUNCTION attachments_changed() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF OLD.status = 'pending' AND NEW.status = 'ready' THEN
            PERFORM app_history('task', NEW.task_id, 'linked', 'attachment', NULL,
                jsonb_build_object('id', NEW.id, 'filename', NEW.filename, 'size', NEW.size));
            PERFORM task_cis_notify_for(NEW.task_id);
        END IF;
        RETURN NULL;
    END IF;
    IF OLD.status = 'ready' THEN
        PERFORM app_history('task', OLD.task_id, 'unlinked', 'attachment',
            jsonb_build_object('id', OLD.id, 'filename', OLD.filename, 'size', OLD.size), NULL);
        PERFORM task_cis_notify_for(OLD.task_id);
    END IF;
    PERFORM enqueue_job('attachment.purge',
        jsonb_build_object('backend', OLD.backend, 'key', OLD.key, 'upload_id', OLD.upload_id, 'pending', OLD.status = 'pending'),
        now(), 'attachment-purge:' || OLD.id);
    RETURN NULL;
END $$;

-- ticket_changes for one record, as task_cis_notify sends it.
CREATE FUNCTION task_cis_notify_for(p_task uuid) RETURNS void
    LANGUAGE sql SECURITY DEFINER SET search_path = public
    AS $$
    SELECT pg_notify('ticket_changes', json_build_object(
        'op', 'UPDATE', 'id', t.id, 'type', t.type, 'number', t.number, 'status', t.status,
        'requester_id', t.requester_id, 'assignee_id', t.assignee_id)::text)
    FROM tasks t WHERE t.id = p_task;
$$;

-- Files added and removed are among the basics always recorded, whatever the history settings say: which screenshot
-- came when, and who removed one, must always have an answer.
CREATE OR REPLACE FUNCTION app_history_on(p_subject text, p_kind text, p_field text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$
    SELECT CASE
        WHEN p_subject = 'setting' THEN true
        WHEN p_subject = 'task' AND p_field IN ('', 'status', 'assignee_id', 'assignment_group_id', 'attachment') THEN true
        WHEN p_subject = 'ci' AND p_field IN ('', 'status') THEN true
        WHEN p_kind IS NULL THEN true
        WHEN p_subject = 'ci' AND (SELECT value FROM settings WHERE name = 'history.cmdb') = 'false'::jsonb THEN false
        WHEN p_subject = 'task' AND NOT coalesce(
            (SELECT value ? '*' OR value ? p_kind FROM settings WHERE name = 'history.record_types'), true) THEN false
        ELSE NOT EXISTS (SELECT 1 FROM history_exclusions e WHERE e.subject = p_subject AND e.kind = p_kind AND e.field = p_field)
    END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION task_cis_notify_for(uuid) FROM PUBLIC;
CREATE TRIGGER attachments_changed AFTER UPDATE OR DELETE ON attachments FOR EACH ROW EXECUTE FUNCTION attachments_changed();

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_history_on(p_subject text, p_kind text, p_field text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public
    AS $$
    SELECT CASE
        WHEN p_subject = 'setting' THEN true
        WHEN p_subject = 'task' AND p_field IN ('', 'status', 'assignee_id', 'assignment_group_id') THEN true
        WHEN p_subject = 'ci' AND p_field IN ('', 'status') THEN true
        WHEN p_kind IS NULL THEN true
        WHEN p_subject = 'ci' AND (SELECT value FROM settings WHERE name = 'history.cmdb') = 'false'::jsonb THEN false
        WHEN p_subject = 'task' AND NOT coalesce(
            (SELECT value ? '*' OR value ? p_kind FROM settings WHERE name = 'history.record_types'), true) THEN false
        ELSE NOT EXISTS (SELECT 1 FROM history_exclusions e WHERE e.subject = p_subject AND e.kind = p_kind AND e.field = p_field)
    END
$$;
-- +goose StatementEnd
DROP TRIGGER attachments_changed ON attachments;
DROP FUNCTION attachments_changed();
DROP FUNCTION task_cis_notify_for(uuid);
DROP TABLE attachments;
DELETE FROM settings WHERE name LIKE 'storage.%';
DELETE FROM secrets WHERE name LIKE 'storage.%';
