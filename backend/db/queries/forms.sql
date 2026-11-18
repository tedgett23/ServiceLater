
-- name: CountFieldValues :many
-- How many records of a type hold a value for each extra field (fields the form no longer has included), counted as the
-- system: what the form editor locks and warns about.
SELECT k.key::text AS name, count(*)::bigint AS records
FROM tasks t, jsonb_object_keys(t.fields) AS k(key)
WHERE t.type = @type::text
GROUP BY k.key;
