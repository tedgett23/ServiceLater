-- +goose Up

-- The audit of rule runs (docs/apps-platform.md §6.1): every async run, and every before/after rule that stopped a
-- save or could not be evaluated. Rules that ran and did nothing unusual are not recorded, so the table grows with
-- what an administrator needs to look at, not with traffic.
--
-- The rows are written from a separate connection, never inside the save that triggered them: an aborted save rolls
-- its own transaction back, and the record of why must survive that.
CREATE TABLE action_runs (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    app         text NOT NULL,
    rule        text NOT NULL,
    timing      text NOT NULL CHECK (timing IN ('before', 'after', 'async')),
    event       text NOT NULL,
    -- No foreign key: the record may be gone by the time anyone looks, and the audit outlives it. Null on create,
    -- where a before rule stops the save before there is a record.
    record_id   uuid,
    -- ok: ran. aborted: said no. failed: could not run (an error). skipped: nothing to do (the ticket is gone or hidden).
    outcome     text NOT NULL CHECK (outcome IN ('ok', 'aborted', 'failed', 'skipped')),
    detail      text NOT NULL DEFAULT '',
    duration_ms integer NOT NULL DEFAULT 0,
    job_id      bigint,
    attempt     integer NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX action_runs_created_idx ON action_runs (created_at DESC);
CREATE INDEX action_runs_rule_idx ON action_runs (app, rule, created_at DESC);

-- System only, like the queue: what a rule did is not something a requester reads.
ALTER TABLE action_runs ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON action_runs FROM servicelater_app;

-- +goose Down
DROP TABLE action_runs;
