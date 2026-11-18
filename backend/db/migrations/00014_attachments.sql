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
            PERFORM task_notify(NEW.task_id);
        END IF;
        RETURN NULL;
    END IF;
    IF OLD.status = 'ready' THEN
        PERFORM app_history('task', OLD.task_id, 'unlinked', 'attachment',
            jsonb_build_object('id', OLD.id, 'filename', OLD.filename, 'size', OLD.size), NULL);
        PERFORM task_notify(OLD.task_id);
    END IF;
    PERFORM enqueue_job('attachment.purge',
        jsonb_build_object('backend', OLD.backend, 'key', OLD.key, 'upload_id', OLD.upload_id, 'pending', OLD.status = 'pending'),
        now(), 'attachment-purge:' || OLD.id);
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER attachments_changed AFTER UPDATE OR DELETE ON attachments FOR EACH ROW EXECUTE FUNCTION attachments_changed();

-- +goose Down
DROP TABLE attachments;
DROP FUNCTION attachments_changed();
