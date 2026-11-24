-- +goose Up
-- The chart a saved view is drawn as in the workbench (line, bars, stacked bars or a single figure, and which columns),
-- so whoever opens it sees the same chart. Null: shown as a table. Checked against the view's columns by the server
-- on every save (internal/api/views.go, query.Chart).
ALTER TABLE saved_views ADD COLUMN chart jsonb CHECK (chart IS NULL OR jsonb_typeof(chart) = 'object');

-- +goose Down
ALTER TABLE saved_views DROP COLUMN chart;
