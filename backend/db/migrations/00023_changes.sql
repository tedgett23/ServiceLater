-- +goose Up

-- Change-specific columns (docs/apps-platform.md §9.2), the second extension table after incidents. A change has a
-- kind (standard changes are pre-approved, normal ones go to the change advisory board, emergency ones to a smaller
-- board, fast), an assessed risk and impact, a planned window, and the plans that say how it is done, tested and
-- undone. Empty text means not filled in yet; a guard in the change lifecycle decides when it must be.
CREATE TABLE changes (
    task_id             uuid PRIMARY KEY,
    type                text NOT NULL DEFAULT 'change' CHECK (type = 'change'),
    change_type         text NOT NULL DEFAULT 'normal' CHECK (change_type IN ('standard', 'normal', 'emergency')),
    risk                text NOT NULL DEFAULT '' CHECK (risk IN ('', 'low', 'medium', 'high')),
    impact              text NOT NULL DEFAULT '' CHECK (impact IN ('', 'low', 'medium', 'high')),
    planned_start       timestamptz,
    planned_end         timestamptz,
    implementation_plan text NOT NULL DEFAULT '' CHECK (length(implementation_plan) <= 10000),
    backout_plan        text NOT NULL DEFAULT '' CHECK (length(backout_plan) <= 10000),
    test_plan           text NOT NULL DEFAULT '' CHECK (length(test_plan) <= 10000),
    CONSTRAINT changes_window_check CHECK (planned_end > planned_start),
    FOREIGN KEY (task_id, type) REFERENCES tasks (id, type) ON DELETE CASCADE
);
-- The change calendar asks which windows overlap a range.
CREATE INDEX changes_window_idx ON changes (planned_start, planned_end) WHERE planned_start IS NOT NULL;
INSERT INTO changes (task_id) SELECT id FROM tasks WHERE type = 'change';

ALTER TABLE changes ENABLE ROW LEVEL SECURITY;
-- Visible, and creatable, exactly when its task is; changed by whoever may update changes.
CREATE POLICY changes_select ON changes FOR SELECT TO servicelater_app
    USING (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));
CREATE POLICY changes_insert ON changes FOR INSERT TO servicelater_app
    WITH CHECK (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id));
CREATE POLICY changes_update ON changes FOR UPDATE TO servicelater_app
    USING (app_can_record('update', 'change') AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_id))
    WITH CHECK (app_can_record('update', 'change'));

-- Changing the row is a change to the ticket: bump updated_at and notify, through the task's own trigger.
-- +goose StatementBegin
CREATE FUNCTION changes_touch_task() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    UPDATE tasks SET updated_at = now() WHERE id = NEW.task_id;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER changes_touch_task AFTER UPDATE ON changes
    FOR EACH ROW EXECUTE FUNCTION changes_touch_task();

-- +goose Down
DROP TRIGGER changes_touch_task ON changes;
DROP FUNCTION changes_touch_task();
DROP TABLE changes;
