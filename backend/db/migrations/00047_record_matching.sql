-- +goose Up
-- Suggested links between incidents and problems (docs/apps-platform.md §9.7): how alike two titles are, from 0 to 1.
-- The better of two measures: the share of words (stemmed, stop words left out) the shorter title has in common with
-- the other ("VPN drops every 20 minutes" and "VPN concentrator drops idle sessions" share vpn and drop), and trigram
-- word similarity, which catches near spellings ("print" and "Printers"). Callers add their own signals on top
-- (records linked to the same configuration item).
-- +goose StatementBegin
CREATE FUNCTION app_match_score(a text, b text) RETURNS float8
    LANGUAGE sql IMMUTABLE PARALLEL SAFE SET search_path = public
    AS $$
    SELECT GREATEST(
        COALESCE((SELECT count(*) FROM unnest(s.wa) w WHERE w = ANY (s.wb))::float8
                 / NULLIF(LEAST(cardinality(s.wa), cardinality(s.wb)), 0), 0),
        word_similarity(a, b), word_similarity(b, a))
    FROM (SELECT tsvector_to_array(to_tsvector('english'::regconfig, a)) AS wa,
                 tsvector_to_array(to_tsvector('english'::regconfig, b)) AS wb) s
$$;

-- Any of the words of a text, as a query: what narrows the candidates before they are scored.
CREATE FUNCTION app_any_word_query(q text) RETURNS tsquery
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
    SELECT websearch_to_tsquery('english'::regconfig,
        array_to_string(regexp_split_to_array(btrim(regexp_replace(q, '[^[:alnum:]]+', ' ', 'g')), '\s+'), ' or '))
$$;
-- +goose StatementEnd

-- Finds titles sharing a word without reading every record.
CREATE INDEX tasks_title_words_idx ON tasks USING gin (to_tsvector('english'::regconfig, title));

-- +goose Down
DROP INDEX tasks_title_words_idx;
DROP FUNCTION app_any_word_query(text);
DROP FUNCTION app_match_score(text, text);
