-- +goose Up
-- How a person wants the web UI drawn: following their device (auto, the default), or always light or dark. Kept on
-- the user so it follows them between browsers; users_update already lets anyone change their own row.
ALTER TABLE users ADD COLUMN theme text NOT NULL DEFAULT 'auto' CONSTRAINT users_theme CHECK (theme IN ('auto', 'light', 'dark'));

-- +goose Down
ALTER TABLE users DROP COLUMN theme;
