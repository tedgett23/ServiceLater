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

## Running modes

One binary, one image. `SL_ROLES` selects what a process does (`web`, `api`, `worker`, or `all`, the default):

- **AIO:** `SL_ROLES=all` serves UI, API and background workers in one process.
- **Split:** run separate `web`, `api` and `worker` processes and scale each independently (`deploy/compose/docker-compose.split.yml`).

TLS is off by default (terminate at a load balancer or ingress). `SL_TLS=auto` uses Let's Encrypt; `SL_TLS=files` uses your certificate.
