-- +goose Up
-- Rate limits (settings limits.*, Admin → Rate limits; internal/ratelimit). Each api server counts API requests in
-- memory, so a request costs no database work, and once a second adds what it counted here and reads back the totals
-- every server has counted: the limit is shared by all of them, give or take a second. Counts are per caller and
-- per minute. Unlogged (lost on a crash, which only forgets a minute's counts); system only; windows older than five
-- minutes are removed by the servers that write them.
CREATE UNLOGGED TABLE rate_counts (
    key          text NOT NULL,
    window_start timestamptz NOT NULL,
    count        int NOT NULL,
    PRIMARY KEY (key, window_start)
);
CREATE INDEX rate_counts_window_idx ON rate_counts (window_start);
ALTER TABLE rate_counts ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON rate_counts FROM servicelater_app;

-- An API key's own limit, for an integration that needs more (or less) than the setting for keys: requests a
-- minute, 0 for no limit, NULL to follow the setting.
ALTER TABLE api_keys ADD COLUMN rate_limit int CHECK (rate_limit IS NULL OR rate_limit = 0 OR rate_limit BETWEEN 10 AND 1000000);
-- The app role reads named columns of api_keys only (never the hash), so the new one is granted by name.
GRANT SELECT (rate_limit) ON api_keys TO servicelater_app;

-- Setting a key's limit: by those who manage people, like making and revoking keys. False if there is no such key on
-- that account.
-- +goose StatementBegin
CREATE FUNCTION app_set_api_key_rate_limit(p_user uuid, p_id uuid, p_limit int) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public
    AS $$
BEGIN
    IF NOT app_can('users:manage') THEN
        RAISE EXCEPTION 'changing an API key needs the users:manage permission' USING ERRCODE = '42501';
    END IF;
    UPDATE api_keys SET rate_limit = p_limit WHERE id = p_id AND user_id = p_user;
    RETURN FOUND;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_set_api_key_rate_limit(uuid, uuid, int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_set_api_key_rate_limit(uuid, uuid, int) TO servicelater_app;

-- +goose Down
DROP FUNCTION app_set_api_key_rate_limit(uuid, uuid, int);
REVOKE SELECT (rate_limit) ON api_keys FROM servicelater_app;
ALTER TABLE api_keys DROP COLUMN rate_limit;
DROP TABLE rate_counts;
