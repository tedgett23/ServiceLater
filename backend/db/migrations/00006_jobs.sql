-- +goose Up
-- The job queue the worker role runs, and the audit of what rules and SLA actions did.

-- The durable job queue (docs/apps-platform.md §6). Workers claim jobs with FOR UPDATE SKIP LOCKED, so any number of
-- them can pull concurrently without contention. Jobs are enqueued inside the transaction that causes them (the
-- transactional outbox pattern): if the save rolls back, so does the job, and nothing fires for a change that
-- never happened.
CREATE TABLE jobs (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind         text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_.]{1,60}$'),
    payload      jsonb NOT NULL DEFAULT '{}',
    -- pending: waiting for run_at. running: claimed by a worker. done. failed: out of attempts (the dead letter).
    state        text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'running', 'done', 'failed')),
    run_at       timestamptz NOT NULL DEFAULT now(),
    attempts     integer NOT NULL DEFAULT 0,
    max_attempts integer NOT NULL DEFAULT 5 CHECK (max_attempts >= 1),
    locked_by    text,
    locked_at    timestamptz,
    last_error   text,
    -- Enqueueing the same key twice creates one job, so a retried caller cannot double a side effect.
    dedupe_key   text UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz
);

-- The workers' hot query: due pending jobs, oldest first.
CREATE INDEX jobs_due_idx ON jobs (run_at, id) WHERE state = 'pending';

-- The reaper and the retention sweep.
CREATE INDEX jobs_running_idx ON jobs (locked_at) WHERE state = 'running';

CREATE INDEX jobs_finished_idx ON jobs (finished_at) WHERE finished_at IS NOT NULL;

-- Only workers (the owner role, db.WithSystem) touch this table. Users can enqueue, and only through the function
-- below, which validates what goes in; they cannot read, change or cancel anyone's jobs.
ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;

REVOKE ALL ON jobs FROM servicelater_app;

-- +goose StatementBegin
CREATE FUNCTION enqueue_job(
    p_kind text,
    p_payload jsonb DEFAULT '{}',
    p_run_at timestamptz DEFAULT now(),
    p_dedupe_key text DEFAULT NULL
) RETURNS bigint
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
DECLARE
    new_id bigint;
BEGIN
    IF p_kind !~ '^[a-z][a-z0-9_.]{1,60}$' THEN
        RAISE EXCEPTION 'invalid job kind' USING ERRCODE = '22023';
    END IF;
    IF octet_length(p_payload::text) > 65536 THEN
        RAISE EXCEPTION 'job payload too large' USING ERRCODE = '22023';
    END IF;
    INSERT INTO jobs (kind, payload, run_at, dedupe_key)
    VALUES (p_kind, p_payload, p_run_at, p_dedupe_key)
    ON CONFLICT (dedupe_key) DO NOTHING
    RETURNING id INTO new_id;
    IF new_id IS NOT NULL THEN
        -- Delivered on commit, so a worker never wakes for a job it cannot see yet.
        PERFORM pg_notify('jobs', p_kind);
    END IF;
    RETURN new_id; -- NULL when the dedupe key was already used
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION enqueue_job(text, jsonb, timestamptz, text) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION enqueue_job(text, jsonb, timestamptz, text) TO servicelater_app;

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
    -- sla: an SLA's warning or breach actions, which run like an async rule's.
    timing      text NOT NULL CHECK (timing IN ('before', 'after', 'async', 'sla')),
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
DROP FUNCTION enqueue_job(text, jsonb, timestamptz, text);
DROP TABLE jobs;
