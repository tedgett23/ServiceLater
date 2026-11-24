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
replaced. **Take a snapshot first** (Admin → Backups, or `servicelater snapshot create`); migrations can be rolled back
(`goose down`) but a backup is the plan.

## Rate limits

Admin → Rate limits sets how much one caller may do: API requests a minute for a person and for an API key (a key
may have its own limit on its service account's page), sign-in attempts and their window, calls a webhook takes a
minute, SQL queries an hour and at once, SMS codes an hour, and live connections per person. Each api server counts
API requests in memory and shares its counts through Postgres once a second (the unlogged `rate_counts` table), so
the limit holds across replicas to within a second's traffic, and a request costs no database work for it. Responses
carry `RateLimit-Limit`, `RateLimit-Remaining` and `RateLimit-Reset`; a refused one is 429 with `Retry-After`.
Requests with no session (signing in, webhooks, signed file links) are limited by their own settings, not per address:
put a per-address limit in the proxy or load balancer in front if you need one. `SL_MAX_LIVE_CONNS` is no longer
read (the server warns if it is set).

## Backups

Three layers, for different things (design: docs/backup-and-clone.md). Use the first for everyday snapshots and
restores, and one of the other two for disaster recovery in production.

**Whatever you use, keep the secrets key (`SL_SECRETS_KEY`) somewhere safe and apart from the backups.** Stored
passwords and tokens, and ServiceLater snapshots, cannot be read without it. Keep `apps-local` too.

### ServiceLater snapshots (built in)

Admin → Backups (`instance:backup`, admins): take a snapshot now, on a schedule (cron and time zone), keep the
newest few plus one a day and one a week, download one, upload one from another instance, restore one. A snapshot is
every table from one consistent moment, compressed and encrypted under the secrets key, optionally with the
attachment files kept in a folder. Taking one blocks nobody; restoring one replaces everything in one transaction
(the instance shows a maintenance screen meanwhile, a restore that fails changes nothing, and everyone signs in
again). A snapshot from an older version is migrated up after loading; one from a newer version is refused.

- **Where they go**: the snapshots folder (`SL_SNAPSHOTS_DIR`, default `/data/snapshots`, the `snapshots` volume in
  the Compose files) or the S3 bucket of Admin → Storage, under `<prefix>snapshots/`. On a cluster, the worker takes
  and restores them and the api serves downloads, so a folder must be shared by both (the split Compose file mounts
  the same volume on each), or use S3. Copy folder snapshots off the host as well.
- **Command line**, for scripts and for an instance whose web UI cannot start: `servicelater snapshot create
  [-o file] [-attachments]`, `servicelater snapshot verify <file>`, `servicelater snapshot restore -yes
  [-change-schema] [-attachments] <file>`. It needs the same `SL_DATABASE_URL` and `SL_SECRETS_KEY` as the server.
  Restoring from the command line while servers run makes their requests wait for the restore's locks; stop them
  first if it will take long.
- **Size and speed**, measured: 200,000 tickets and 400,000 comments (a 244 MB database) made a 16 MB snapshot in
  half a second and restored in about 15 seconds.
- **Not to the second**: a snapshot is the moment it was taken. For recovery to any point, add one of the layers
  below.
- **Clone packages** (Admin → Clones, `instance:clone`): copies of production's records and people for UAT or
  development, filtered and optionally masked by a profile, encrypted with a passphrase you choose. They are made by
  the worker into the same folder (`.slclone` files) or under `<prefix>clones/` in S3, and stay until deleted.
  `servicelater clone package -profile uat -passphrase-file f` and `servicelater clone verify -passphrase-file f
  <file>` do the same from the command line. Loading one into the target instance is not built yet.

### The database host's backups (managed Postgres)

On RDS, Cloud SQL or Azure Database for PostgreSQL, turn on automated backups with point-in-time recovery (PITR) and
set the retention you need. Restoring makes a new database server: point `SL_DATABASE_URL` at it and restart. Nothing
in ServiceLater needs to change; the attachments bucket (if S3) has its own versioning and lifecycle rules.

### Continuous archiving with pgBackRest (self-hosted Postgres)

For a self-hosted database, pgBackRest keeps full and incremental backups plus the write-ahead log, to a folder or
S3, so you can restore to any second. In outline, for the Compose stack's `db` service:

1. Use a Postgres image with pgBackRest installed (the official `postgres:17` image plus the `pgbackrest` package),
   and a volume or bucket for its repository.
2. In `postgresql.conf`: `archive_mode = on`, `archive_command = 'pgbackrest --stanza=servicelater archive-push %p'`,
   `wal_level = replica`.
3. `/etc/pgbackrest/pgbackrest.conf`:

   ```ini
   [global]
   repo1-path=/var/lib/pgbackrest
   repo1-retention-full=4
   # or repo1-type=s3 with repo1-s3-bucket, repo1-s3-endpoint, repo1-s3-region and credentials
   [servicelater]
   pg1-path=/var/lib/postgresql/data
   ```
4. Once: `pgbackrest --stanza=servicelater stanza-create`. Then schedule `pgbackrest --stanza=servicelater
   --type=full backup` weekly and `--type=incr` daily (cron on the host, or a sidecar).
5. To restore: stop the servers and Postgres, then `pgbackrest --stanza=servicelater --delta restore` (add
   `--type=time --target="2026-11-24 09:15:00+00"` for a moment), start Postgres, then the servers.

pgBackRest's own documentation covers encryption, retention and S3 in full. Test a restore before you need one.

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
