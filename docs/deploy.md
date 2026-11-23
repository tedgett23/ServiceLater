# Deploying ServiceLater

One image, one binary, every role: `web` (the UI, and a proxy to the API), `api` (REST and live updates), `worker`
(rules, workflows, SLA timers, email, file clean-up). Postgres 17 is the only other service. The first-party apps are
inside the binary and are installed and upgraded at startup; migrations run at startup too, one instance at a time.

## The image

```sh
make image                                      # docker; tags servicelater:<commit> and servicelater:latest
make image CONTAINER=podman BUILD_FLAGS=--network=host   # podman where builds cannot reach the network themselves
```

It runs as a non-root user (65532) on distroless, 40-odd MB. `servicelater version` prints the version and the
commit it was built from; the server logs it at startup. Keep the Go version in the Dockerfile in step with the `go`
line of `backend/go.mod` (the image's Go never downloads a newer one).

## Docker Compose (or Podman Compose)

Copy `.env.example` to `.env` and fill in at least `POSTGRES_PASSWORD`, `SL_JWT_SECRET` (`openssl rand -base64 48`)
and `SL_SECRETS_KEY` (`servicelater secrets gen-key`, or the image: `docker run --rm servicelater secrets gen-key`).
Keep a copy of the secrets key: the mail server password, S3 keys and single sign-on client secrets cannot be read
without it.

- **All in one** (`deploy/compose/docker-compose.yml`): `docker compose -f deploy/compose/docker-compose.yml --env-file
  .env up -d`. The app on port 8080; Postgres, attachments, apps-local and certificates on named volumes.
- **Split** (`deploy/compose/docker-compose.split.yml`): `web`, `api` and `worker` as their own containers, scaled
  independently (`--scale api=3`). Every api and worker needs the same `SL_JWT_SECRET` and `SL_SECRETS_KEY`, and the
  same attachments folder (a shared volume, NFS or EFS; or S3 from Admin → Storage).
- Behind a proxy or load balancer that terminates TLS, set the base URL (Admin → Email → Base URL): email links and
  the single sign-on redirect address are built from it. Or let the binary do TLS itself (`SL_TLS=auto`).
- The server waits up to a minute for Postgres at startup, so it does not matter which comes up first. Both
  services restart unless stopped.

## The database

Postgres 17. The user in the connection string owns the database and runs the migrations, which need:

- **`CREATEROLE`**: migration 00017 creates the login role SQL queries run as, `<database>_query` (docs/query.md),
  and the server sets that role's password whenever it starts serving queries, so the owner keeps needing it (and,
  on Postgres 16 and later, admin rights on that role, which it has because it created it). The Compose files'
  Postgres user is a superuser, so they need nothing more. On a managed or shared Postgres: `ALTER ROLE servicelater
  CREATEROLE` before the first start.
- The extensions `pg_trgm` and `pgcrypto`, which ship with Postgres and are trusted, so the owner creates them.
- `pg_hba.conf` must let `<database>_query` sign in by password (SCRAM) from the api servers, as it lets the owner. The
  server sets that role's password itself, derived from a key in the database, so there is nothing to configure.

`deploy/smoke.sh` runs a query, so `make release-check` fails if any of this is missing.

## Before a release

`make release-check` builds the image, starts a fresh all-in-one stack from it with throwaway secrets, runs
`deploy/smoke.sh` (sign-in, filing, SLA clocks, search, a SQL query, problems, an attachment through the volume, an API key, the API
reference) and removes the stack and its volumes. With Podman: `make release-check CONTAINER=podman
COMPOSE=podman-compose BUILD_FLAGS=--network=host`. `deploy/smoke.sh <url> <admin email> <password>` also runs
against any test deployment; it creates records, so not against production.

## Upgrading

Pull or build the new image and `up -d` again with the same volumes. On startup the server applies the new
migrations (under an advisory lock, so replicas take turns) and upgrades the bundled apps; an operator's own changes
in apps-local are kept. Upgrading a split deployment: start the new api and worker; old replicas keep serving until
replaced. **Back up the database first** (`pg_dump`); migrations can be rolled back (`goose down`) but a backup is
the plan.

## Backups

The database (`pg_dump -Fc`), the attachments volume (unless they are in S3), apps-local, and the secrets key.

## Verified (2026-11-17, Podman 5.4 and podman-compose 1.3 on Debian 13)

- A fresh all-in-one stack: migrations, the seven bundled apps, the bootstrap admin; then sign-in, filing, SLA
  clocks, search in descriptions, problems, an attachment through the server's upload URLs into the volume, a service
  account's API key, the API reference. A restart on the same volumes changes nothing and keeps everything.
- The split stack through the web role, with a second api replica accepting the first one's sessions, the worker
  resolving a problem's incidents, and live updates over WebSocket through the web role's proxy.
- An upgrade from 09b7049 (migration 41) to the current image on a populated database: migrations 42-50 applied, a
  bundled app upgraded and a new one installed, and the old records, attachment, users and numbering intact.

**Migrations were consolidated on 2026-11-18** (before any real deployment): the 56 files that grew with the code
became 16, one per area (`backend/db/migrations/00001_foundation.sql` to `00016_sign_in.sql`), building the same
schema. A database made by the old files cannot be upgraded by the new ones: create it again (`DROP DATABASE ... WITH
(FORCE)`, `CREATE DATABASE ... OWNER servicelater`) and let the server migrate it. From here on migrations are
additive again.

Podman notes: it needs fully qualified image names (`docker.io/library/postgres:17`, as the Compose files now say);
podman-compose ignores `--scale`, does not wait for health checks (the server's wait covers that), and leaves
`${VAR:-default}` unexpanded when VAR is not in the env file (so keep every variable of `.env.example` in `.env`); in a container
without `/dev/net/tun`, builds need `--network=host`.

## Kubernetes

`deploy/helm/servicelater` is a placeholder (Chart.yaml and values.yaml, no templates). Not built or tested yet.
