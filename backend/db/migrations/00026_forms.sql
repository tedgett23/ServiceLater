-- +goose Up

-- A record's extra fields: the ones a form declares (docs/apps-platform.md §8), so an app adds fields without a
-- table change (D6). Keys are field names, values text, whole numbers or booleans (dates as YYYY-MM-DD, times as
-- RFC 3339 text). internal/records checks every value against the record type's form on each save; the database only
-- keeps the column an object of a sane size. RLS is the task's: whoever can see the record sees its fields.
ALTER TABLE tasks ADD COLUMN fields jsonb NOT NULL DEFAULT '{}'::jsonb
    CONSTRAINT tasks_fields_object CHECK (jsonb_typeof(fields) = 'object' AND pg_column_size(fields) <= 65536);

-- +goose Down
ALTER TABLE tasks DROP COLUMN fields;
