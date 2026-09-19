# ServiceLater

A fast, modern ITSM platform. See [ServiceLater-MVP.md](ServiceLater-MVP.md) for the architecture.

| Path | Contents |
|---|---|
| `backend/` | Go API (Chi, pgx, sqlc, goose migrations in `backend/db/migrations`) |
| `web/` | SvelteKit SPA (built with adapter-static, embedded in the Go binary) |
| `apps/` | First-party apps (incident, change, CMDB, knowledge, catalog) in the §11 app format |
| `deploy/` | Docker Compose (AIO and split), Helm |
| `docs/` | Design notes and decisions |

## Dev

    make dev-db        # Postgres
    make dev-backend   # :8080
    make dev-web
    make check         # lint + tests

## Running modes

One binary, one image. `SL_ROLES` selects what a process does (`web`, `api`, `worker`, or `all`, the default):

- **AIO:** `SL_ROLES=all` serves UI, API and background workers in one process.
- **Split:** run separate `web`, `api` and `worker` processes and scale each independently (`deploy/compose/docker-compose.split.yml`).

TLS is off by default (terminate at a load balancer or ingress). `SL_TLS=auto` uses Let's Encrypt; `SL_TLS=files` uses your certificate.

## Database

Postgres 17. The migrating DB user needs `CREATEROLE` (migration `00001` creates the `servicelater_app` role that RLS applies to; the Compose `POSTGRES_USER` is a superuser and already qualifies). Tests use a scratch database, `servicelater_test` by default (`make test`).

## Auth

Password login (bcrypt) issues two httpOnly, SameSite=Strict cookies: a 15-minute JWT access token and a 30-day rotating refresh token (stored hashed in the unlogged `sessions` table; replaying a rotated token revokes the whole session family). `SL_JWT_SECRET` is required for the `api` role. Set `SL_BOOTSTRAP_ADMIN_EMAIL` and `SL_BOOTSTRAP_ADMIN_PASSWORD` to create the first admin on an empty database. `make dev-backend` sets dev values (`admin@localhost` / `admin-password`).

Endpoints: `POST /api/v1/auth/login`, `/refresh`, `/logout`, `GET /api/v1/auth/me`. Logins are limited to 10 attempts per 15 minutes per client IP and email.

## Tickets API

All under `/api/v1/tickets`, cookie-authenticated, and scoped by Postgres RLS (end users see only their own tickets; agents and admins see all).

| Route | Who | Notes |
|---|---|---|
| `GET /tickets` | all | Filters: `status`, `open=true` (hides resolved and closed), `priority`, `assignee` (user id, `me` or `none`), `requester` (user id or `me`), `q` (title substring). Paging: `limit` (default 25, max 100), `offset`; response has `has_more`. |
| `POST /tickets` | all | `title`, `description`, `priority` (1 critical to 4 low, default 3). Staff may pass `requester_id`. |
| `GET /tickets/{id}` | all | `{id}` is the UUID or the ticket number. Hidden tickets are 404. |
| `PATCH /tickets/{id}` | staff | `title`, `description`, `priority`, `assignee_id` (staff user, or `null` to unassign). |
| `POST /tickets/{id}/transitions` | staff | `{"to": "in_progress"}`. Invalid moves are 409. `closed` is terminal. |
| `GET /users/staff` | staff | Agents and admins, for assignee pickers. |
| `GET/POST /tickets/{id}/comments` | all | `{"body", "internal"}`. Internal notes are staff-only, and hidden from everyone else by RLS. |

Status flow: `new` to `in_progress`, `on_hold`, `resolved` or `closed`; `in_progress` and `on_hold` to `resolved`; `resolved` to `in_progress` (reopen) or `closed`.

Ticket and comment responses include display names (`requester_name`, `assignee_name`, `author_name`), so clients never need a user lookup.

## UI

A SvelteKit SPA: sign-in page, then a master-detail ticket queue (list on the left, ticket on the right; one pane at a time on narrow screens). Keyboard: `j`/`k` move through the queue, `c` creates a ticket, `/` searches. Edits (status, priority, assignee, comments) apply instantly and roll back if the server refuses them. Fonts are bundled (Public Sans, Big Shoulders Display), so nothing loads from a third party and it works air-gapped. Colors adapt to the OS light or dark setting.
