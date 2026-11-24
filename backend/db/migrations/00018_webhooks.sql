-- +goose Up
-- Workflows started by a webhook (docs/apps-platform.md §7.8): every call that reached a webhook, whether it started a
-- run, repeated one, or was refused, and why. Bodies are never kept here (a started run holds its own in
-- vars.payload). Written by the server as the system, so a refused call is recorded too; read by whoever may read
-- every run. Deliveries older than 30 days are removed by the cleanup loop.
CREATE TABLE workflow_webhook_deliveries (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    app          text NOT NULL,
    workflow     text NOT NULL,
    -- The value of the webhook's dedupe header, when the call had one.
    delivery_key text CHECK (length(delivery_key) <= 200),
    outcome      text NOT NULL CHECK (outcome IN ('started', 'duplicate', 'refused')),
    -- Why a call was refused (no secret set, a bad signature, a body that is not JSON...); empty otherwise.
    reason       text NOT NULL DEFAULT '',
    -- The run it started, or for a duplicate the run the first call started. No FK: deliveries outlive runs' rows.
    execution_id uuid,
    size         int NOT NULL DEFAULT 0,
    remote       text NOT NULL DEFAULT '',
    received_at  timestamptz NOT NULL DEFAULT now()
);
-- One run per delivery: a second call with the same key finds the first one's row.
CREATE UNIQUE INDEX workflow_webhook_deliveries_key_idx ON workflow_webhook_deliveries (app, workflow, delivery_key)
    WHERE outcome = 'started' AND delivery_key IS NOT NULL;
CREATE INDEX workflow_webhook_deliveries_recent_idx ON workflow_webhook_deliveries (app, workflow, received_at DESC);
CREATE INDEX workflow_webhook_deliveries_received_idx ON workflow_webhook_deliveries (received_at);
ALTER TABLE workflow_webhook_deliveries ENABLE ROW LEVEL SECURITY;
REVOKE INSERT, UPDATE, DELETE ON workflow_webhook_deliveries FROM servicelater_app;
CREATE POLICY workflow_webhook_deliveries_select ON workflow_webhook_deliveries FOR SELECT TO servicelater_app
    USING (app_can('workflows:read_all'));

-- +goose Down
DROP TABLE workflow_webhook_deliveries;
