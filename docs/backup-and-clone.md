# Backups, snapshots and clones

Design agreed with the user on 2026-11-24. Status: step 1 (snapshots, Admin → Backups, maintenance mode, the
instance name, environment and banner) built; steps 2-4 (clones) designed.

Three things are covered here: keeping a copy of an instance (backups and snapshots), copying production down to
UAT or development (clones), and keeping identities straight when the two have drifted apart.

Two facts about the schema shape the design:

- **Every table holding real data has a UUID primary key** (users, groups, records, comments, items, articles,
  attachments, runs, ...). Two instances can never make two *different* things with the same id. The only identity
  problem is the *same* thing (one person) made separately on both, with two ids.
- **Record numbers (INC42) come from one sequence per record type** (`task_number_<type>`, plus `kb_article_number`),
  and logs use `bigint` ids (`record_history`, `workflow_steps`, `jobs`, `action_runs`). Those do collide between
  instances, so a restore or clone sets the sequences, never just copies rows.

## 1. Backups

Three layers, solving different problems. ServiceLater ships C and documents A and B.

| | What | Gives | Where |
|---|---|---|---|
| A | The database host's snapshots and point-in-time recovery (RDS, Cloud SQL, Azure Database for PostgreSQL) | Restore to any second, run by the provider | docs/deploy.md |
| B | Continuous archiving with pgBackRest (full + incremental backups and the WAL, to a folder or S3) | Restore to any second on a self-hosted database | docs/deploy.md: how to run it beside the Compose stack's Postgres |
| C | **ServiceLater snapshots**: the binary copies every table, and optionally the attachment files, from one consistent point in time into one package | Admin → Backups: a schedule, how many to keep, a folder or S3, "Take a snapshot now", restore, download | Built in |

C is the product feature: it works the same on Compose, Kubernetes or a managed database, and it is the engine clones
are built on. A or B is disaster recovery and is recommended alongside it for production.

### Snapshots (C)

- **Format**: a `.slsnap` file: a tar archive, compressed and encrypted as one stream, holding `manifest.json` (instance name and
  environment, taken at, by whom, goose version, binary version, installed apps and versions, table list with row
  counts and checksums), one file per table in Postgres's binary `COPY` format, the sequence values, and optionally
  `attachments/` (the local folder's files; S3 attachments are referenced, not copied, unless asked).
- **Consistency**: one `REPEATABLE READ, READ ONLY` transaction; with parallel workers, `pg_export_snapshot()` so
  every worker reads the same moment. No locks beyond what a long read takes; the API keeps serving.
- **What is left out**: `sessions`, `rate_limits`, `rate_counts`, `jobs` (queued work would send production's mail
  again), `goose_db_version` (recorded in the manifest instead). Secrets are included *as ciphertext*: they are
  readable only with the same `SL_SECRETS_KEY`.
- **Where they go**: a folder (`SL_SNAPSHOTS_DIR`, default `/data/snapshots`) or the S3 bucket from Admin → Storage
  under a `snapshots/` prefix. Encrypted at rest with a key derived from `SL_SECRETS_KEY` (so a stolen file is
  useless without the key, and the key is what a restore needs anyway). An instance with no secrets key cannot take
  snapshots; the page says how to set one (`servicelater secrets gen-key`).
- **Schedule and retention**: settings `backup.*` (Admin → Backups): a cron line and time zone, keep the last N, keep
  a daily for D days and a weekly for W weeks. The worker role runs them in a loop of its own (`internal/backup`,
  every 5 s), not as jobs: a restore empties the job queue, and either can outlast a job's lease. Claims are SKIP
  LOCKED; each running one says it is alive every 15 s, and one silent for a minute failed. The record of snapshots
  and restores (`snapshots`, `snapshot_restores`, `00021_backups`) is in no snapshot and emptied by no restore.
- **Restore**: into this instance or a new, empty one. Refused when the snapshot's goose version is newer than the
  binary's; an older one is migrated up after loading. Runs in maintenance mode (below), in one transaction:
  truncate, load, set sequences, re-enable triggers. Everyone signs in again.
- **What a restore also needs**, and the page says so: the same `SL_SECRETS_KEY` (or stored secrets are unreadable
  and must be set again), the attachment files when they are in a folder and were not included, and `apps-local`.
- **CLI**: `servicelater snapshot create|list|restore|verify|delete`, for scripts and for restoring an instance whose
  web UI cannot start.
- **Permission**: `instance:backup` (admins). Every snapshot, download and restore is audited.

### Maintenance mode

While a restore waits (at most two minutes for a worker to take it) or runs, every API request but `GET /instance`
gets 503 with `Retry-After`, and the worker takes no jobs. Every server learns it from `snapshot_restores` once a
second (`internal/instance`). `GET /instance` needs no sign-in, so any page, the sign-in page included, shows the
maintenance screen; strangers learn only that a restore is running, not the instance's name or who started it. When
it ends the page goes to sign-in (a restore ends every session).

## 2. Clones

Copy production (the **source**) down to UAT or development (the **target**). Data loss on the target is acceptable:
its records are replaced, not merged.

### Environment identity

Settings `instance.name` and `instance.environment` (`production`, `uat`, `development`, `other`). Instances that are
not production show a coloured banner on every page ("UAT, cloned from Production on 24 Nov") so nobody mistakes
one for the other. **An instance marked production refuses to be cloned into** unless an admin first turns that guard
off on it, so development can never be cloned over production by a wrong URL.

### Profiles (on the source)

Filtering happens on the source, so data a profile leaves out never leaves production. A **clone profile**
(Admin → Clones → Profiles, `instance:clone`) says:

- **Records**: which record types; all, open ones only, or changed in the last N days; and exclusions by condition
  (the rule-environment CEL used everywhere else).
- **What comes with them**: internal notes (yes/no), attachments (none, for kept records only, up to a size), field
  history (none, last N days, all), audit logs (`action_runs`, `query_runs`, deliveries: off by default).
- **People**: everyone, or only those the kept records and kept groups refer to.
- **Masking** (off by default): names, email addresses, phone numbers, comment and description text, and form fields
  marked `mask: true` in their app (a form field attribute, so a field holding personal data is masked wherever it
  appears). Masked emails are `user-<n>@masked.invalid`; masking runs after identity matching (below).
- **Who may pull it**: the service accounts allowed to request this profile (see scheduled pull).

**Closure** is worked out, never left to the admin: a skipped record takes its comments, attachments, SLA clocks,
history, links and approvals with it; a kept record's link to a skipped one (its problem, its parent request, a linked
change) is cleared; kept records bring the people, groups and items they refer to even when "only referenced people"
is chosen. The **clone report** lists every link it cleared.

### Getting the package to the target

All three ways are built from the start:

1. **Download and upload**: Admin → Clones → Make a package on the source; Admin → Clone in → Upload on the target.
   The package is encrypted with a passphrase the admin chooses (AES-256-GCM, key from scrypt).
2. **Shared S3 bucket**: the source writes the package to a bucket and prefix; the target reads it from there.
3. **Scheduled pull** ("refresh UAT every Sunday at 02:00"): the target calls the source's API and streams the
   package down, on a schedule or on demand.
   - Trust is an **API key**: on the source, a service account whose key is narrowed to `instance:export` and which
     the profile allows. The target keeps the source URL, the key (a secret) and the profile name in settings
     `clone.*` (Admin → Clone in → Source).
   - `GET /api/v1/instance/clone-package?profile=uat` streams the package over TLS (no passphrase: it is encrypted
     on the target's disk with an ephemeral key while it is applied).
   - The source refuses a profile the key's account is not allowed, and audits every pull. The pull runs as the
     worker role on the target (`clone.pull` job), then applies like an upload.
   - The target must reach the source's API; firewall rules are the operator's.

### Applying a clone (on the target)

1. **Preview**: what comes in (counts by table and record type), what is kept, the source's and target's versions
   (goose and apps), the identity matches and conflicts found. Nothing has changed yet.
2. **Confirm** by typing the target's instance name (scheduled pulls confirm once, when the schedule is set).
3. Maintenance mode on; one transaction:
   - put aside what the target keeps (below), already remapped to the source's ids;
   - truncate the cloned tables; load the package with triggers off (`ALTER TABLE ... DISABLE TRIGGER USER` as the
     owner), so loading writes no history, notifications or live events;
   - put back what was kept; set every sequence past the highest value loaded;
   - re-enable triggers; run the safety switch (below).
4. Maintenance mode off; the bundled apps re-sync as at every start; the **clone report** is kept (Admin → Clone in →
   History) and audited on both instances.

### What the target keeps

Defaults, each changeable in Admin → Clone in → Keep:

- **Always the target's own**: settings (email, SMS, Teams, incoming mail, storage, sign-in, rate limits, backups,
  clone source), secrets, single sign-on providers and identities, the query key, the instance name and environment,
  the clone history.
- **Kept by default, switchable**:
  - the studio layer (workflow, form and catalog edits made on the target and not yet on production: development's
    work in progress, like an unfinished update set);
  - people and service accounts marked **keep on clone** (test agents, development's integration accounts) with
    their passwords, API keys, roles, groups and phone numbers.
- **Never copied from the source**: sessions, queued jobs, rate counts, webhook calls, API keys (production's keys
  would be live credentials on the target), `instance.*` settings.

### Identity: same thing, two ids

1. **Records always come from the source**, whole. The target's own records are replaced; never merged (merging
   collides on numbers and helps nobody).
2. **What the target keeps is matched to the source by a natural key**: people by email (lowercased), service
   accounts by name, groups and roles by name, items by class and name, saved views by name.
3. **On a match, the source's id wins.** The source has Pat as P1; the target made Pat separately as D1; a source
   ticket is assigned to P1. After the clone Pat is P1 on the target, the ticket's assignee is right, and the
   target's own parts of Pat (password, roles and groups granted on the target, studio edits Pat made, Pat's API
   keys if a service account) are moved from D1 to P1. Only the small kept side is rewritten; the source's data is
   loaded as it is. (ServiceNow's clone overwrites `sys_id`s the same way; its data preservers put the target's
   records back with their old ids, which can duplicate a person. Matching by natural key avoids that.)
4. **Kept things with no match keep their ids.** UUIDs never collide, so a target-only tester stays as they are.
5. **Ids inside text or JSON** are handled explicitly: field history values that name people or groups are remapped by
   the same table; workflow runs still going are cancelled, not copied; saved query views (SQL text) and literal ids in
   workflow expressions that no longer resolve are listed in the report. Definitions should name things, not ids:
   `group('Service Desk')`, `role('cab-member')`, and a new `user('pat@example.com')`.
6. **Other unique values that clash** (a kept group "Network" and a source group "Network" with different ids, which
   the name match makes one; or a kept saved view with a source view's name but other SQL) are resolved by the source
   winning and the target's being renamed "Network (UAT)", and reported. A clone never fails halfway on a constraint.

### The safety switch

After every clone, until an admin turns each back on (Admin → Clone in shows them): outbound email, SMS and Teams;
`http.request` steps (they fail with "outbound calls are off after a clone"); scheduled workflows; incoming email
polling; webhooks (they answer 503). Without it, UAT emails real customers, posts to production's Teams channels and
reads production's mailbox.

### Clocks and running work

- SLA clocks are kept, with `next_alert_at` moved to after the clone, so the target does not fire a wave of breach
  notifications for time that passed on the source.
- Workflow runs still going are cancelled ("cancelled by a clone"); approvals still open close with them.
- Schedules start counting from the clone (no missed runs fired at once).
- Inbound email's last-seen marks are the target's own (kept with its settings).

### Versions

The package records the source's goose version and apps. A package newer than the target's binary is refused (update
the target first). An older one is migrated up after loading. Apps and their definitions come from the source; the
target's binary then syncs its bundled apps as at every start, and the target's kept studio layer is applied over
them; a studio edit that no longer compiles is set aside and reported, never lost.

## 3. Permissions

- `instance:backup`: snapshots, their schedule and restores. Admins.
- `instance:clone`: profiles, packages, clone in, the clone source. Admins.
- `instance:export`: may request a clone package for an allowed profile. Given only to a clone service account's
  key; nobody holds it by role.

## 4. Order of work

1. Snapshot engine, `servicelater snapshot`, Admin → Backups (schedule, retention, folder or S3, restore, download),
   maintenance mode, the instance name, environment and banner. Docs for A and B.
2. Clone profiles and packages on the source: filters, closure, masking, encryption; download and S3.
3. Clone in on the target: preview, keep rules, identity matching and remapping, the safety switch, the report.
4. Scheduled pull: `instance:export` keys, the export endpoint, the target's source settings and schedule.
