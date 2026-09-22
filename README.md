# ServiceLater

A fast, modern ITSM platform. See [ServiceLater-MVP.md](ServiceLater-MVP.md) for the architecture.

| Path | Contents |
|---|---|
| `backend/` | Go API (Chi, pgx, sqlc, goose migrations in `backend/db/migrations`) |
| `web/` | SvelteKit SPA (built with adapter-static, embedded in the Go binary) |
| `apps/` | First-party apps (incident, change, CMDB, knowledge, catalog) in the §11 app format. See [docs/apps-platform.md](docs/apps-platform.md) |
| `deploy/` | Docker Compose (AIO and split), Helm |
| `docs/` | Design notes and decisions |

## Dev

    make dev-db        # Postgres (skip if you already have one)
    make install-apps  # once per database: the first-party apps (incident lifecycle and so on)
    make dev-backend   # :8080
    make dev-web
    make check         # lint + tests

## Running modes

One binary, one image. `SL_ROLES` selects what a process does (`web`, `api`, `worker`, or `all`, the default):

- **AIO:** `SL_ROLES=all` serves UI, API and background workers in one process.
- **Split:** run separate `web`, `api` and `worker` processes and scale each independently (`deploy/compose/docker-compose.split.yml`).

Two settings tune capacity: `SL_WORKER_CONCURRENCY` (background jobs and workflow steps run at once, default 4; about 8 is the sweet spot on a dedicated worker, see `docs/apps-platform.md` §7.6) and `SL_MAX_LIVE_CONNS` (live connections one person may hold across tabs and devices, default 10).

TLS is off by default (terminate at a load balancer or ingress). `SL_TLS=auto` uses Let's Encrypt; `SL_TLS=files` uses your certificate.

## Database

Postgres 17. The migrating DB user needs `CREATEROLE` (migration `00001` creates the `servicelater_app` role that RLS applies to; the Compose `POSTGRES_USER` is a superuser and already qualifies). Tests use a scratch database, `servicelater_test` by default (`make test`). The migration test also creates a throwaway database per run, so the role needs `CREATEDB` too (`ALTER ROLE servicelater CREATEDB`); it skips itself without it.

## Auth

Password login (bcrypt) issues two httpOnly, SameSite=Strict cookies: a 15-minute JWT access token and a 30-day rotating refresh token (stored hashed in the unlogged `sessions` table; replaying a rotated token revokes the whole session family). `SL_JWT_SECRET` is required for the `api` role. Set `SL_BOOTSTRAP_ADMIN_EMAIL` and `SL_BOOTSTRAP_ADMIN_PASSWORD` to create the first admin on an empty database. `make dev-backend` sets dev values (`admin@localhost` / `admin-password`).

Endpoints: `POST /api/v1/auth/login`, `/refresh`, `/logout`, `GET /api/v1/auth/me`. Logins are limited to 10 attempts per 15 minutes per client IP and email.

## Tickets API

All under `/api/v1/tickets`, cookie-authenticated, and scoped by Postgres RLS (end users see only their own tickets; agents and admins see all).

| Route | Who | Notes |
|---|---|---|
| `GET /tickets` | all | Filters: `type`, `status`, `open=true` (hides resolved and closed), `priority`, `assignee` (user id, `me` or `none`), `requester` (user id or `me`), `q` (title substring). Paging: `limit` (default 25, max 100), `offset`; response has `has_more`. |
| `POST /tickets` | all | `title`, `description`, `priority` (1 critical to 4 low, default 3). Staff may pass `requester_id`. |
| `GET /tickets/{id}` | all | `{id}` is the UUID, a reference such as `INC42` (any case), or a bare number, which means an incident. Hidden tickets are 404. |
| `PATCH /tickets/{id}` | staff | `title`, `description`, `priority`, `assignee_id` (staff user, or `null` to unassign), `resolution` (incidents only). |
| `POST /tickets/{id}/transitions` | staff | `{"to": "in_progress"}`. Invalid moves are 409. `closed` is terminal. |
| `GET /users/staff` | staff | Agents and admins, for assignee pickers. |
| `GET/POST /tickets/{id}/comments` | all | `{"body", "internal"}`. Internal notes are staff-only, and hidden from everyone else by RLS. |

Status flow: `new` to `in_progress`, `on_hold`, `resolved` or `closed`; `in_progress` and `on_hold` to `resolved`; `resolved` to `in_progress` (reopen) or `closed`.

Tickets have a `type` (`incident`, `change` ...) and a `reference` (`INC42`): numbers count within a type. Ticket and comment responses include display names (`requester_name`, `assignee_name`, `author_name`), so clients never need a user lookup.

## UI

A SvelteKit SPA: sign-in page, then a master-detail ticket queue (list on the left, ticket on the right; one pane at a time on narrow screens). Keyboard: `j`/`k` move through the queue, `c` creates a ticket, `/` searches. Edits (status, priority, assignee, comments) apply instantly and roll back if the server refuses them. Fonts are bundled (Public Sans, Big Shoulders Display), so nothing loads from a third party and it works air-gapped. Colors adapt to the OS light or dark setting.

## Realtime

`WS /ws/v1/subscribe` (cookie-authenticated, same-origin only) streams change hints from Postgres `LISTEN/NOTIFY` to every open browser tab, so the queue and open tickets update without reloading. Events carry ids only (`ticket.changed`, `comment.added`, `resync`); the UI refetches through the normal API, so RLS still decides what anyone can read.

- Staff hear about every ticket; everyone else only about their own. Internal notes never signal to non-staff.
- A socket closes when its access token expires (code 4401); the client refreshes the session and reconnects. It also reconnects with backoff after an outage and refetches what it shows, since NOTIFY is fire-and-forget.
- Limits: 10 connections per user; a client that falls behind is dropped and resyncs.
- Runs in the `api` role. Every api replica listens independently, so replicas need no coordination, but a load balancer in front must allow long-lived WebSocket connections.

## End-to-end tests

`web/e2e` holds Playwright tests that drive the real production binary (UI embedded) in a real browser against a scratch database: sign-in and session refresh, working a ticket as an agent, what a requester can and cannot see or do, live updates between two browsers (including a dropped connection), and responsive and dark-mode regressions.

    createdb servicelater_e2e            # once; owner must be the servicelater role
    cd web && npx playwright install chromium   # once
    make e2e                             # builds, then runs the suite

The scratch database is `servicelater_e2e` (override with `SL_E2E_DATABASE_URL`). It is wiped on every run, and the test users are created with `servicelater user create`. Run a single file with `cd web && npx playwright test e2e/live.spec.ts`, or watch it in a browser with `--headed` / `--ui`. Failures keep a screenshot and a trace (`npx playwright show-trace <trace.zip>`).

## CLI

    SL_USER_PASSWORD=... servicelater user create -email a@b.co -name "Ada" -role agent

Roles: `admin`, `agent`, `end_user`. The password comes from the environment or `-password-stdin`, never a flag.

## Apps

An app is a folder of YAML that customises ServiceLater. Today an app can define a **lifecycle**: the status flow of a
record type (state names, categories, the button for each move) with optional **guards** (CEL expressions that must
hold for a move, with a message when they do not). Everything the product knows about statuses comes from the
installed lifecycle. See `docs/apps-platform.md` for the design and roadmap.

    my-app/
      app.yaml          identity, version, dependencies
      default/          what the app ships (upgrades replace this)
        lifecycles/incident.yaml
      local/            optional: this instance's own changes (upgrades never touch this)

**The first-party apps are inside the binary.** At startup the server installs or upgrades `core-itsm` and
`incident-management` (change with `SL_BUNDLED_APPS`: `all`, `none` or a comma-separated list). To change one, put a
file with only what differs in `SL_APPS_LOCAL_DIR/<app-name>/`, laid out like the app's own `local/` folder:

    # $SL_APPS_LOCAL_DIR/incident-management/lifecycles/incident.yaml
    states:
      new:
        label: Backlog        # maps merge key by key; lists and scalars are replaced; null deletes a key

then run `servicelater app sync` (or restart). The directory belongs on a volume, so upgrading the image or the binary
never overwrites it, and everything you did not change still arrives with the upgrade. A bad local file is reported
and leaves the app as it was; the instance keeps running.

    servicelater app validate apps/incident-management       # check it, no database needed
    servicelater app pack apps/incident-management           # write <name>-<version>.swapp (shipped layer only)
    servicelater app install -dry-run apps/core-itsm apps/incident-management   # show the plan
    servicelater app install apps/core-itsm apps/incident-management            # all or nothing, dependencies first
    servicelater app sync [-dry-run]                          # apply the bundled apps and local changes now
    servicelater app list
    servicelater app uninstall incident-management           # records are kept and can still be closed

- Installs are **all or nothing** and validated before anything is written: manifest, dependencies, every definition,
  every guard (`record.prioity` or `priority == "high"` is rejected with the column), and that no record would be left
  in a status the new lifecycle lacks. To rename a state that records are in, set `renamed_from: old_state` on the new one.
- Updates add a new immutable version of each changed definition and reload every running instance within a moment:
  open screens pick up new labels, buttons and guards live.
- Uninstalling never deletes tickets. They fall back to a built-in flow and can still be closed. A bundled app comes
  back at the next start unless left out of `SL_BUNDLED_APPS`.
- Not available yet: rules, workflows, forms and the rest (the validator says so), and `app dev`.
