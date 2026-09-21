-- +goose Up

-- Which layers produced a stored definition: default, or default+local, default+local+instance, and so on.
-- It is informational (the body is already the merged result) but answers "why does my screen say this?".
ALTER TABLE app_definitions ADD COLUMN source text NOT NULL DEFAULT 'default';

-- +goose Down
ALTER TABLE app_definitions DROP COLUMN source;
