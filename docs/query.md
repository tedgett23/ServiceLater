# SQL queries

People who hold `query:run` (admins, and core-itsm's **Analyst** role) can run read-only SQL. The SQL runs over the
views in the `query` schema, as themselves: each view shows only what that person could see in the app.

## How it is kept safe

Row-level security in ServiceLater trusts session settings (`app.permissions`, `app.current_user_id`, ...). Anyone
who can type arbitrary SQL could change those settings, so user SQL never runs where they count. Three layers stand
between a query and the data, and each holds on its own:

1. **The check** (`internal/query/check.go`). The SQL is parsed with Postgres's own parser (libpg_query, compiled to
   WebAssembly by `wasilibs/go-pgquery`, so the build stays cgo-free). Every node of the tree must be on an allowlist:
   - one `SELECT` (WITH, joins, subqueries, set operations, window functions and VALUES included);
   - relations: only `query.*` views, or WITH names in scope;
   - functions, operators and casts: only those on the lists in `check.go`.

   Everything else is refused, with the line and column of the first problem. That covers writes in any form,
   `SELECT INTO`, `FOR UPDATE`, `SET`/`RESET`, `set_config`, `current_setting`, `pg_*`, `query_to_xml`, `regclass`
   casts, `COLLATE`, `ORDER BY ... USING` and more. Something new in Postgres is refused until it is added to a list.
   The text actually run, the query wrapped in a row limit, is checked again.
2. **The role.** Queries run on a login role of their own: `app_query_role()`, `<database>_query`, because roles
   belong to the whole cluster. It is not a member of the owner, so `RESET ROLE` gains nothing. The transaction is
   read-only, with a statement timeout. The role:
   - has `USAGE` on the `query` schema only (migration 00017 revokes `public` from `PUBLIC` and grants it to the app
     role);
   - has `SELECT` on its views (default privileges) and `EXECUTE` on `app_query_identity()`.

   The server sets the role's password from the signing key (HMAC-derived, sent as a SCRAM verifier), so every api
   server agrees on it and nothing needs configuring. `DISCARD ALL` runs after every query.
3. **The identity.** Views do not use RLS: they run as their owner and filter explicitly by `app_query_identity()`.
   That function reads `query.token`: `<user>|<expiry>|<scopes>|<HMAC-SHA256>`, signed by the server with the key in
   the owner-only `query_keys` table. It checks the signature and expiry, then works out the person's permissions
   from the database (narrowed by their API key's scopes) and the restricted groups hidden from them. A forged,
   edited, expired or widened token is refused, and so is anyone without `query:run`. Changing the RLS settings with
   `set_config` changes nothing for the views. This works on a hot standby too (nothing is written), so a read
   replica can be added later.

`internal/query/run_test.go` (`TestTheQueryRoleHoldsWithoutTheCheck`) sends SQL straight to the query role, as if the
check had a hole, and asserts each of these.

## Limits

- 30 seconds per query; at most 5,000 rows, with `truncated` set when there were more.
- Two queries at once per person (per server) and 300 an hour (counted from the audit log); 429 beyond that.
- A query may be at most 20,000 characters.

## Audit

Every attempt is a `query_runs` row: who, which key, the SQL and parameters, the outcome
(ok/refused/failed/timeout), the error, rows, duration. It is written by the server as the system, so refusals are
kept too. People read their own (`GET /api/v1/query/runs`); `settings:manage` reads everyone's.

## API

- `QUERY /api/v1/query` `{sql, params}` (the HTTP QUERY method, RFC 10008: a safe, idempotent read with a body)
  returns `{columns: [{name, type}], rows, truncated, duration_ms}`. Refused or
  failed is a 422 `{error, line, column, timeout}`.
- `POST /api/v1/query` is the same, for clients and networks (some proxies, WAFs, API gateways) that do not pass QUERY.
  Both are reads: a query stops when its client goes away (`stopWithClient` for the POST alias; changes by POST finish
  instead, see `finishWrites`), and is recorded as failed with "whoever sent it went away".
- `GET /api/v1/query/schema` lists the views, with their columns and descriptions (from `COMMENT ON`).
- `GET /api/v1/query/runs` returns the caller's last 50 queries.

## The views

`backend/db/migrations/00018_query_views.sql`. Each one shows what the API would show the person, no more:

| View | Rows | Who sees them |
|---|---|---|
| `records` | every record (incidents, changes, requests, problems, tasks, app types), with `reference`, status, `state_category`, priority, people and group ids, `fields` (jsonb) | the requester, or `records:read:<type>` unless it is routed to a restricted group hidden from them (`app_sees_task`) |
| `incidents`, `changes`, `requests` | each type's own columns, by `record_id` | as `records` |
| `comments` | comments by `record_id` | as `records`; internal notes only with `comments:internal` |
| `attachments` | finished uploads' names, types and sizes (not the files) | as `records` |
| `slas` | SLA clocks, with the label they started with | as `records` |
| `history` | field-by-field changes (`subject` task, ci or setting) | a record's as `records`, an item's with `cmdb:read`, a setting's with `settings:manage` |
| `people` | people (never app principals), with `service_account` | everyone with `users:read`, otherwise only themselves |
| `groups`, `group_members` | teams and who is in them | everyone who may query, as in the app |
| `cis`, `ci_relationships`, `record_cis` | items, how they relate, which records affect them | `cmdb:read` (and the record, for links) |
| `articles`, `record_articles` | knowledge articles, with `reference` | published ones by audience, others with `knowledge:write` or `knowledge:publish` |
| `record_types` | the types, with `prefix` and `retired_at` | everyone who may query |

Something linked to a record joins `query.records`, so the record rule is written once. Tests:
`internal/query/views_test.go` (four people, a restricted group, internal notes, items and articles; every view is a
`security_barrier`, described, never reads session settings, and can be read through the role),
`internal/api/query_test.go`.

A gotcha found writing them: planning a query inlines the SQL functions in the table's index expressions as the
person running it, so an index function that names `public.` breaks every query on that table for the query role
(which may not use `public`). 00018 gives `app_record_words` a `SET search_path` so it is never inlined.
`TestEveryQueryViewCanBeRead` finds the next one.

## The workbench

`/queries` (web `routes/(app)/queries`, `lib/queries.ts`, `lib/sqlEditor.ts`; navigation "Queries" for `query:run`),
mockup A as the user chose it: the tables at the left (your views, shared views, built-in; each with its columns,
types and descriptions; `+` puts a name in the query), CodeMirror with Postgres highlighting and completion of tables
and columns (`@codemirror/lang-sql`), Ctrl/Cmd+Enter to run (sent with QUERY), inputs for `$1`... when the query has
them, a refused or failed query marked where it went wrong, Results (references link to their record, CSV download)
and History (your last 50, click to bring one back). **Save as view** (or **Save view** while editing your own,
`?view=name`) names it, describes it and shares it. The draft is kept in the browser. Types are Postgres's short
names everywhere (`int2`, `timestamptz`, `text[]`). Tests: `web/e2e/queries.spec.ts`.

**Chart** (`lib/charts/ResultChart.svelte`, `lib/charts/resultChart.ts`): line, bars, stacked bars or a single figure,
picked to fit the result (a time column and numbers: a line; text and numbers: bars; one number: a figure) and picked
again when the columns change; up to four numeric columns, coloured series-1..4 in their result order, never cycled.
Bar charts show the first 50 rows in query order and say how many more are in Results. Zero draws nothing; hover or the arrow keys read exact values; dates are drawn in UTC.
A saved view keeps its chart (`saved_views.chart`, `00020_saved_view_charts`; `query.Chart` checks it against the
view's columns on every save).

## Saved views

A query saved under a name, which other queries read like a table (`SELECT * FROM open_incidents`), and which
`GET /api/v1/views/{name}/rows` serves like a table API. Migration `00019_saved_views`, `internal/query/saved.go`,
`internal/api/views.go`.

- **Only the SQL is kept.** When a query names a saved view, the runner puts its SQL in front as a `WITH` clause (and
  the views it reads, each after those it uses) and checks the whole text again before it runs as the query role. So
  a view can never show more than the query views do, and **whoever reads a view sees their own rows**, not its
  author's: a shared "open incidents" shows an analyst no restricted HR records even if an admin wrote it.
- **Private or shared.** A view is its owner's alone, or `shared` with everyone who holds `query:run`. Names are one
  namespace (a name someone else has privately is a 409). Only the owner changes a view; the owner or a
  `settings:manage` holder removes one.
- **What may be saved:** one SELECT that runs for its owner, no parameters (filter when reading instead), a name of 2
  to 63 lower-case letters, digits and underscores that is not a built-in view or an SQL keyword, no view reading
  itself directly or through others, and a shared view reads only shared views. Its columns (name and type) are
  worked out by running it with no rows, and kept.
- **Views that others read** (`depends_on`) cannot be removed, change their columns, or become private while a shared
  one reads them (409, naming the views the caller can see and counting the rest). Saves and removals take an advisory
  lock, so two cannot race into a cycle.
- A saved view's name wins over a built-in view's (so a built-in view added later never changes what a saved query
  means), and a WITH in the query wins over both. A problem inside a view's SQL is reported as "in the view name: ...".

API (`query:run`): `GET /views`, `GET|PUT|DELETE /views/{name}`, and two ways to read a view's rows:

- `GET /views/{name}/rows?limit=&offset=&order=&<column>=<value>`: equality on columns, ANDed, the value converted to
  the column's type; `order=-col` descends.
- `QUERY /views/{name}/rows` with a JSON body (`internal/query/filter.go`, `ReadSQL`):

  ```json
  {"where": {"and": [{"priority": {"lte": 2}}, {"title": {"ilike": "%vpn%"}},
                     {"or": [{"assignee_id": {"is_null": true}}, {"state_category": {"in": ["open", "waiting"]}}]}]},
   "order": [{"priority": "asc"}, {"created_at": "desc"}], "columns": ["reference", "title"], "limit": 100, "offset": 0}
  ```

  Operators `eq ne lt lte gt gte in not_in between like ilike is_null contains` (like/ilike on text columns, contains
  a JSON value on jsonb ones); `and`/`or`/`not` nest up to 10 deep, 200 conditions, 1,000 values. A problem is a 422
  naming where it is (`where.and[1].priority.like: like compares text, and priority is int2`). Columns are checked
  against the view's and quoted; every value is a parameter, sent as text for Postgres to convert.

Both: limit 1 to 5,000 (default 100), `truncated` when there are more. A read of rows is a query like any other: limited and in `query_runs`.
`GET /query/schema` lists the caller's readable saved views as `saved_views` beside the built-in `views`.

Tests: `internal/query/saved_test.go`, `internal/api/views_test.go`.

## Writing a view

A view lives in `query`, is created `WITH (security_barrier)` (so a user's predicates cannot see rows before the
view's filter), joins `app_query_identity() me` once, and filters with `me.user_id`, `me.perms` (`,perm,perm,`) and
`me.hidden`. It must never be looser than the API: mirror the rules Go enforces as well as the RLS ones (for
example, the directory is `users:read` only). Give the view and each column a `COMMENT`: that is what the editor
shows. A view's columns are a contract that saved queries depend on, so add columns, and never rename or drop them
without a deprecation.
