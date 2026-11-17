# App platform design

Status: draft for review. Supersedes the scripting and UI wording in §11.1 of `ServiceLater-MVP.md`.

An app is a folder of human-readable files that customises ServiceLater: lifecycles, rules, workflows, forms,
catalog items, notification templates and roles. It can be zipped into a `.swapp`, diffed in a pull request, and
installed on any instance. The first-party modules (incident, change, CMDB, knowledge, catalog) ship as apps
using exactly the same mechanism, so nothing in the platform is special-cased for them.

This document settles how apps are structured, how their logic runs, and how we get there in small steps.

## 1. Decisions

| # | Decision | Status |
|---|---|---|
| D1 | Conditions and calculations are **expressions** (CEL): pure, type-checked at install, always cheap. | Decided |
| D2 | Logic comes in two tiers. **Basic** is a rule: a CEL expression plus built-in actions. **Advanced** is a workflow: a graph of steps implemented in Go. There is **no scripting language** in v1. | Decided |
| D3 | Rules have three timings: **before**, **after** and **async**. Before and after run inside the save and can block it, using only expressions and built-in actions, with no external I/O and tight budgets. They fail closed. | Decided |
| D4 | v1 apps have **declarative UI only**: forms, list views and layouts as data. No custom UI code. | Decided |
| D5 | No Python, Starlark, Lua or WASM in v1. Actions and steps name a `type`, so a script step could be added later without changing the app format. | Decided |
| D6 | v1 apps are **data only**: no app-defined tables or SQL migrations. Apps extend records through validated custom fields (`fields jsonb`), including on record types of their own (D17). | Decided |
| D7 | Definitions are stored as immutable, versioned rows in Postgres, cached per instance, and reloaded through `NOTIFY`. | Proposed |
| D8 | Statuses come from a **lifecycle definition**; core logic depends on state *categories*, not names. | Proposed |
| D9 | Before and after rules run as the acting user under their RLS. Async rules and workflows run as the app's service principal, with permissions declared in the manifest. | Built (§11.1) |
| D10 | Uninstalling an app never deletes user data (tickets, comments). | Proposed |
| D11 | Incident, change, request and problem each have **their own table**, defined in core (§9.2). | Decided |
| D12 | Workflows are **interpreted by a durable engine written in Go**, not compiled to code. The step library is the extension surface. | Decided |
| D13 | The workflow **engine ships before the visual builder**: workflows are YAML first, and the builder edits the same files later. | Proposed |
| D15 | An app has a shipped layer, `default/`, and an optional operator layer, `local/`. Local overrides default by merging, and upgrades replace `default/` and never touch local (§4.5). | Decided |
| D16 | The first-party apps are **embedded in the binary** and installed and upgraded at startup. An instance keeps its own changes to them in a local directory outside the binary (§4.5). | Decided |
| D14 | Record types **extend a shared `task` base** (class-table inheritance): common columns live in `tasks`, type-specific columns in an extension table per type. Rules, lifecycles, forms and SLAs defined on `task` apply to every type. | Decided |
| D17 | Apps define **record types of their own** (`record_type` definitions, §9.5): a `task_types` row with its own prefix and numbering, whose fields are `fields jsonb` declared by a form. Apps still never create tables or run DDL; typed extension tables stay core-only. | Built. References are never zero-padded, for any type (decided 2026-11-17: no width to estimate, and INC42 reads better than INC0000042). |
| D18 | **Each app is its own section in the navigation** (§9.6): an accordion at the left, one line per app the person can use, the open one showing its views; each section's queue holds only that app's record types. Custom apps' records are **private to the app's role holders** by default, not visible to every agent. | Decided 2026-11-17, not built |

Open questions are in §13.

## 2. Goals and non-goals

Goals
- **Bounded by construction.** The request path runs only expressions and a few budgeted built-in actions, with no
  external I/O. Anything slow or external runs after commit in workers that scale separately.
- **Reviewable.** Every app artefact is text a person can read in a diff.
- **Safe to install.** An app can only do what its manifest declares, and installs are validated before anything
  is written.
- **One mechanism.** First-party and third-party apps are indistinguishable to the runtime.
- **Nothing to sandbox.** With no embedded interpreter, there are no thread, memory or escape problems to solve.
  Every step is Go code we wrote and tested.

Non-goals for v1
- A scripting language, custom UI code, app-defined tables, a marketplace, app signing, multi-tenant isolation
  (each customer already has their own instance).

## 3. Lessons taken from ServiceNow and Splunk

- **ServiceNow** runs server-side JavaScript inside the request, on a fixed pool of app-server threads. A slow
  business rule blocks the transaction and everyone else's. We keep the useful shape (before, after and async
  rules) but there is no arbitrary code in it: before and after rules are expressions and built-in actions with no
  external calls, and anything slow or external is an async rule or a workflow in a worker (D2, D3).
- **Splunk** keeps app code out of its engine and makes most of an app declarative config. We go further for v1:
  apps contain no executable code at all. Definitions are data, logic is expressions and workflows, and the UI is
  declarative. That avoids Splunk's pain points (dependency management, no sandbox, process-spawn cost) entirely.

## 4. What an app contains

```
my-app/
  app.yaml            manifest: identity, version, dependencies, permissions, config
  README.md
  default/            what the app ships; upgrades replace this
    lifecycles/       status flows for a record type, with state categories and guarded transitions
    rules/            "on this event, if this expression is true, run these actions"
    workflows/        durable multi-step processes: approvals, waits, branches, loops
    forms/            field definitions, layout and validation for a record type
    catalog/          service catalog items
    knowledge/        knowledge articles (Markdown)
    notifications/    email / SMS / in-app templates
    roles/            custom roles and permission sets
    slas/             service level agreements: clocks with start, pause and stop conditions, targets and alerts
    ui/               declarative list views and dashboards
  local/              optional: this instance's own changes; upgrades never touch this
    lifecycles/ ...   the same folders, holding only what differs
```

`app.yaml` is identity, not configuration, so it is not layered. There is no `scripts/` (D2) and no `migrations/`
(D6): the validator rejects both anywhere in the app. `ui/` holds declarative view definitions only (D4).

**Values in definitions.** A value is a literal unless it starts with `=`, in which case the rest is a CEL
expression. `title: New ticket` is text; `to: =record.assignee.email` is computed. Fields that are always
conditions (`when`, `until`) are always expressions and need no `=`.

### 4.1 Manifest

```yaml
apiVersion: 1                      # version of the app format itself, so it can evolve
name: incident-management
version: 0.3.0
author: ServiceLater
description: Incident lifecycle, escalation and notifications.
ServiceLater_min_version: 0.4.0
dependencies:
  - core-itsm: ">=0.1.0"
permissions:                       # everything the app's rules and workflows may do; shown to the admin at install
  - incidents:read
  - incidents:update
  - comments:create
  - notify:email
  - notify:slack
  - http:https://hooks.slack.com   # outbound hosts an http step may call
config:                            # per-instance settings an admin fills in after install
  - key: escalation_channel
    label: Slack channel for P1 escalations
    type: string
  - key: slack_token
    label: Slack bot token
    type: secret                   # stored encrypted, never returned by the API
```

### 4.2 A lifecycle (this replaces the `transitions` map hardcoded in Go today)

```yaml
kind: lifecycle
record_type: incident
initial: new
states:
  new:         { label: New,         category: open }
  in_progress: { label: In progress, category: open }
  on_hold:     { label: On hold,     category: waiting }
  resolved:    { label: Resolved,    category: done }
  closed:      { label: Closed,      category: closed }
transitions:
  - { from: new,         to: in_progress, verb: Start work, primary: true }
  - { from: in_progress, to: on_hold,     verb: Put on hold }
  - { from: in_progress, to: resolved,    verb: Resolve
      when: 'record.resolution != ""',                 # guards arrive with expressions (slice 2)
      error: Add resolution notes before resolving. }
  - { from: resolved,    to: in_progress, verb: Reopen }
  - { from: resolved,    to: closed,      verb: Close }
```

A transition may carry a **guard**: `when` is an expression that must be true for the move to be allowed, and `error`
is the sentence a person sees when it is false. They are written together or not at all. The guard sees `record` as
the ticket would be after the move, `old` as it is now, and `user`.

```yaml
  - { from: new, to: in_progress, verb: Start work, primary: true,
      when: 'has(record.assignee_id)', error: Assign the incident before starting work. }
```

A move that can be blocked for more than one reason lists more `guards`, each with its own message; they are checked
in order after `when`, and the first that fails says why. A transition may also declare `lookups` (§5), counted
before its guards run, which read them as `lookups.name`. change-management's Scheduled to Implement uses both:

```yaml
  - from: scheduled
    to: implement
    verb: Start implementing
    lookups:
      freezes_now:
        count: freeze_window
        where: { starts_at: { lte: =now }, ends_at: { gt: =now } }
    guards:
      - when: has(record.planned_start) && now >= record.planned_start
        error: The planned window has not opened yet.
      - when: record.change_type == "emergency" || lookups.freezes_now == 0
        error: A change freeze is on. Only an emergency change can start during a freeze.
```

**The server decides what is offered.** Each single-ticket response carries `moves`: what the person can do next,
the suggested step first, with any move a guard currently blocks marked and explained. Only staff get moves. The UI
renders these and keeps no copy of the lifecycle's rules, so a guard, a renamed button or a stranded ticket all work
without a frontend change. Trying a blocked move anyway is refused with the same message (HTTP 422).

`automatic: true` marks a move the process makes, not a person: it is never offered, and a person asking for it is
refused (HTTP 409, "made by the process, not by hand"). Only a workflow's `record.transition` step, running as its
app for someone, can make it. The change lifecycle uses it for Authorize to Scheduled, so only a board's approval
authorizes a change. An automatic move cannot be `primary`.

`primary: true` marks the suggested next step from a state: the UI shows it first and emphasised. A state may set
`renamed_from: old_id`, and installing then moves every record in the old state to it (an update is otherwise
rejected while any record is in a state the new lifecycle lacks). A record stranded in a state its lifecycle no
longer has (an app was replaced or removed) can only be closed, and the API publishes exactly those moves so clients
never offer more than the server accepts.

A lifecycle with `record_type: task` is the default for every type, and a type's own lifecycle overrides it (§9.2).

**State categories** (`open`, `waiting`, `done`, `closed`) are what core code understands. The "Open" filter, the
"Unassigned" highlight, SLA pausing and reporting all use categories, so apps can rename or add states without
breaking core (D8). The verbs are the button labels the UI shows, so labels are no longer duplicated in the
frontend.

### 4.3 Rules

A rule is a record type, a timing, an event, an expression filter, and a list of actions. A rule on `record_type: task`
applies to every type; a rule on `incident` only to incidents (§9.2).

```yaml
kind: rule
name: require-resolution-notes
record_type: incident
timing: before                  # before | after | async
on: [update]                    # create | update | delete
when: 'record.status == "resolved" && size(record.resolution) == 0'
run:
  - action: abort
    message: Add resolution notes before resolving.
```

```yaml
kind: rule
name: default-priority-from-service
record_type: incident
timing: before
on: [create]
when: 'record.priority == 0 && record.service == "payments"'
run:
  - action: set
    field: priority
    value: =1
```

```yaml
kind: rule
name: escalate-critical
record_type: incident
timing: after
on: [create]
when: 'record.priority == 1'
run:
  - action: workflow.start
    workflow: critical-escalation
```

Any action in `run` may carry its own `when`. Rules are for basic logic: validate, default and compute fields, keep
related records consistent, and start a workflow. A **guard** (in a lifecycle) is for a single-transition check on
the record (with lookups if it needs other data); use a **rule** when you span several transitions or have side
effects. Anything with waiting, branching over time, loops or external calls is a workflow (§7).

### 4.4 A workflow

```yaml
kind: workflow
name: critical-escalation
record_type: incident
start: notify_owner
steps:
  notify_owner:
    type: notify.email
    to: =record.assignee.email
    template: critical-ticket
    next: wait_ack
  wait_ack:
    type: wait
    until: 'record.status != "new"'      # re-checked whenever the record changes
    timeout: 15m
    on_timeout: page_channel
    next: done
  page_channel:
    type: http.request
    method: POST
    url: =config.slack_webhook
    body: ={"text": "INC-" + string(record.number) + " is still unacknowledged"}
    next: done
  done:
    type: end
```

### 4.5 Layers: default and local

An app's shipped definitions live in `default/`. An operator's own changes to them live in `local/`, or, for apps
that ship inside the binary, in a directory on the instance. The rule is the one Splunk uses: **an upgrade replaces
`default/` and never touches `local/`**, so nothing an operator did is overwritten, and everything the upgrade
brings that the operator did not touch flows through.

**Where the local layer lives.** Four layers are merged, later ones winning:

1. `default/`: the shipped layer. Always present.
2. `local/` inside the app folder: for an app installed from a folder, so a developer or third party can keep their
   changes beside it. It is never packed into a `.swapp`.
3. The **instance local directory**, `SL_APPS_LOCAL_DIR/<app-name>/`, laid out like an app's `local/` folder. This
   is where changes to the embedded first-party apps go, because the binary cannot hold them. It belongs on a
   volume, so replacing the image or the binary leaves it alone.
4. The **studio layer**: override files kept in the database (`app_overrides`, migration 00031), written by
   ServiceLater's own editors (the workflow builder, §7.7). Every install and sync applies it (startup, `app sync`,
   `app install`), and upgrades never touch it. Each installed app's own files are kept too (`app_files`), so any
   installed app, bundled or not, can be compiled again with an edit.

**How they combine.** A local file at the same path as a shipped file is merged into it, not swapped for it:

- **Maps merge key by key**, recursively. A local file can change one label and say nothing else, so any change the
  upgrade makes to everything else still arrives.
- **Lists and scalars are replaced whole.** A local `transitions:` list replaces the shipped one.
- **`null` deletes a key**, which is how a local file removes a state.
- **New keys are added** after the shipped ones, so ordering stays stable.
- **A local file with no shipped counterpart adds a new definition**, and must be complete on its own.
- **`override: replace`** at the top of a local file replaces the definition below it whole instead of merging into
  it (the key itself is dropped). It is an escape hatch: upstream changes to that definition stop arriving. The
  builder falls back to it only when the smallest merging override cannot say what was drawn.

This is the Splunk model: `default/` is what the app ships and an upgrade replaces, `local/` is the operator's and
merges over it, attribute by attribute (here key by key, with `null` to delete). Splunk keeps `local/` as files on
every node and, in a search head cluster, replicates UI changes between members' `local/` folders. Here the database
is the cluster's shared state, so changes made in ServiceLater are kept once, in the studio layer, and every server
reads them; files remain the way to manage changes with git or configuration management.

**Moving changes between environments.** `servicelater app export-local [-app name] <dir>` writes the studio layer as
files (one folder per app, laid out like `local/`; with `-app`, that app's files straight into `<dir>`), and
`servicelater app import-local [-app name] [-replace] [-dry-run] <dir>` loads them into another instance, validating
and reinstalling each app (`-replace` makes the layer match the folder exactly). So an edit made in dev moves to
test and production as files, in version control, with no records moved. The same folder can instead be mounted as
`SL_APPS_LOCAL_DIR`, where it is the file layer below the studio one.

```yaml
# SL_APPS_LOCAL_DIR/incident-management/lifecycles/incident.yaml
states:
  new:
    label: Backlog          # only this changes; everything else is the shipped incident lifecycle
```

**What is checked.** The shipped layer must be valid **on its own**, because that is what is distributed. When local
layers exist, the merged result must be valid too, and problems name the layers involved
(`lifecycles/incident.yaml (default + local)`). A definition records which layers it came from, so "why does my
screen say this?" has an answer in `app install`'s plan and in `app list`.

**A bad local file never takes the instance down.** At startup and on `app sync`, if the merged result of an app is
invalid, that app is left exactly as it was and the error is logged; the instance keeps running on what is
installed. Removing the local file (or fixing it) and syncing restores the shipped definition or applies the fix.

**Bundled apps.** The first-party apps are embedded in the binary from the `apps/` folder, which is a Go module of
its own that embeds itself, so there is no copy step and no drift. At startup every instance installs or upgrades the
apps named by `SL_BUNDLED_APPS` (default: `core-itsm` and `incident-management`; `all`, `none` or a list are also
accepted), applying the instance local directory. `servicelater app sync` does the same on demand, for instance after
editing a local file. Because the sync runs at every start, an uninstalled bundled app comes back at the next
restart; leave it out of `SL_BUNDLED_APPS` to keep it off, and `app uninstall` says so.

## 5. Expressions (CEL)

Used wherever an app needs a condition or a computed value: transition guards, rule filters, `set` values, form
show/hide and validation, workflow branches and step inputs.

- **Pure**: no I/O, no side effects, no loops except over finite lists, and evaluation always terminates. CEL also
  has a cost limit, so a pathological expression over a huge list is aborted.
- **Type-checked at install** against a declared schema for each context. A typo is rejected when the app is
  installed, not discovered in production.
- **Cheap enough to run inline**: parsed and checked once per definition version, cached, and safe to evaluate
  concurrently. Every rule's `when` filter is an expression, so a save only runs the rules that match.
- **Analysable**: the platform can list the fields an expression reads, which powers reactive forms and the
  visual builder.

**Built (slice 2).** `internal/expr` compiles and evaluates expressions, and lifecycle guards use it.

- **Compile-time checking is real**: `record.prioity` is `undefined field 'prioity'`, `record.priority == "high"`
  is a type error, and an expression that does not produce true or false is rejected. Errors are one line with a
  column, so they read well in an install report. Environments are built from Go types, so the schema an expression
  is checked against is the same struct the server evaluates.
- **`has()`, never `null`.** Optional fields (`record.assignee_id`) are tested with `has(record.assignee_id)`.
  Comparing with `null` is rejected with a message saying so: for some types the comparison type-checks and then
  never matches, which would be a silent trap.
- **Limits.** An expression is at most 1,000 characters and has a runtime cost limit. `now` is an input, not a
  function, so a result depends only on what was passed in.
- **Fail closed.** If a stored guard can no longer be compiled (a platform upgrade removed a field) or evaluated,
  its move is blocked with a clear message, and the problem is logged. It is never allowed by default.
- **Measured** (this repository's benchmarks, one laptop-class machine): compiling an expression takes about
  70 µs, once per definition version; evaluating one takes about 3 µs. Through the real HTTP handlers and the
  database a transition takes about 1.3 ms with no guard and about 1.6 ms with one; the extra 340 µs is loading the
  ticket and the acting user for the guard, not the expression. The benchmark is
  `SL_BENCH=1 go test -run SavePath ./internal/api`, and it fails if a guard adds more than 1 ms.

**Context.** What an expression can see depends on where it runs:

| Where | Variables |
|---|---|
| Lifecycle guards | `record` (the ticket as it would be after the move), `old` (as it is now), `user` (the actor), `now`, and the transition's `lookups` |
| Rules (slice 5) | the same, plus `event` (`create`, `update` or `delete`; on create `old` is an empty record), `config`, and the rule's own `lookups` |
| Workflows | `record` (live), `inputs` (for subflows; an input named `record` binds `record`), `item` (inside `foreach`), `trigger`, `vars`, `steps.<id>.output`, `user`, `now`, `config` |
| Forms | the rule environment: `record` (the record as the save would leave it, extra fields as `record.fields.name`), `old`, `user`, `now`, `event`; no lookups |

`now` is an input, not a function, so evaluation is repeatable. The set of variables and their types is part of the
platform contract and is versioned with `apiVersion`.

**References and helpers.** References expand to objects (`record.assignee.email`, `record.requester.manager`) up to two
levels deep. Because expressions are analysable, the platform prefetches only the paths an expression actually
reads. A small documented function library sits on top of standard CEL (`scale(duration, double)`, `group("name")`,
date helpers).

**Actions in one rule run in order, and later expressions see the effect of earlier `set`s.**

**Lookups (built).** Some blocking checks need data beyond the record ("no open child incidents", "no overlapping
change on this item", "not during a change freeze"). A rule, or a lifecycle transition, may declare named, bounded,
read-only lookups, counted before its expressions run, which read them as `lookups.name`:

```yaml
lookups:
  freezes:
    count: freeze_window
    where:
      starts_at: { lt: =record.planned_end }   # field: { operator: value }; several operators make a range
      ends_at:   { gt: =record.planned_start }
```

The menu is deliberately one shape: **`count`** over one kind of row, with **AND-only** filters, operators
`eq ne lt lte gt gte in`, and filter values that are text or `=expressions` (times are always expressions, such as
`=now`; `in` takes an expression giving a list of text). `count > 0` covers "exists". There are no joins, no OR, no
aggregates and no free text. What can be counted, and by which fields, is a fixed menu of indexed columns
(`apps.LookupTargets`): every record type by `id`, `status`, `state_category`, `requester_id`, `assignee_id` and
`assignment_group_id`, changes also by `change_type`, `planned_start` and `planned_end`, and `freeze_window` by
`starts_at` and `ends_at`. At most five lookups per definition and eight filters per lookup; counting stops at 1000.

How it runs: the definition is checked at install (the target and fields exist, each value has the field's type, every
`lookups.x` an expression reads is declared, a filter does not read lookups). At run time the engine works out the
values from the record and calls the SQL function `app_lookup_count(target, filters)` (migration 00024) in the save's
transaction, **as the person acting**, so RLS decides what is counted: an end user's lookup over incidents counts only
their own. Every value is a bound parameter. A lookup that cannot run (a value cannot be evaluated, the database
refuses it) is an error, and the rule or guard fails closed. `state_category` is a core column maintained from the
lifecycle, so lookups never depend on status names. Not yet: `parent_id` (records have no parents yet), lookups in
workflow steps (§7.2 proposes `record.lookup` for those).

## 6. Rules and their three timings

| | **before** | **after** | **async** |
|---|---|---|---|
| Runs | Inside the save transaction, before the write | Inside the same transaction, after the write | After commit, in a `worker` |
| Can block the save | Yes: `abort` | Yes: `abort` rolls the whole save back | No |
| Can change | The record being saved, in memory (`set`) | Other records, atomically with the save | Anything permitted |
| Can start | Nothing | Workflows and notifications, queued in the transaction | Workflows, notifications, external calls |
| External I/O | No | No (it only enqueues) | Yes, to declared hosts |
| Runs in | `api` | `api` | `worker` |
| Retries | No (fails the request) | No | Yes, with backoff |

**Why the sync timings have no external I/O.** They run while the transaction holds a row lock, and a save can
still roll back. A network call in there would hold the lock for as long as a remote server takes to answer and
could not be undone. That is how one slow integration stalls a whole ServiceNow instance, so it is forbidden.
"After" rules can still trigger external work safely by *enqueueing* it: writing a job row is part of the
transaction, so it happens only if the save commits (the transactional outbox pattern).

### 6.1 Execution model

**Save path** (before and after):

1. The API opens the transaction and locks the row (`SELECT ... FOR UPDATE`), so rules see consistent data.
2. Matching **before** rules run in order. Each evaluates its expression and applies its actions: `set` changes
   the record in memory, `abort` stops the save with that message.
3. The write happens, and the lifecycle guard is checked.
4. Matching **after** rules run in order and may write other records or enqueue jobs in the same transaction.
5. Commit. Jobs for matching **async** rules are enqueued in the same transaction, so a rolled-back save never
   triggers anything.

**Async path**: a worker claims the job (`SKIP LOCKED`), reads the ticket as it is now, as the person who made the
change, and runs the rule's actions with retries. The rule's own `when` is decided at the save, where the old and new
record are both known (`record.priority != old.priority` cannot be asked later), and is not asked again. Each action's
own `when` is judged by the worker on the current data, with `old` equal to `record`.

Every async run is recorded in `action_runs` (app, rule, timing, outcome, duration, detail, job, attempt), and so is
every before or after rule that stopped a save or could not be evaluated. Sync rules that ran without incident are
not recorded, so the table grows with what an administrator needs to look at, not with traffic. The lines are written
from a connection of their own: an aborted save rolls its transaction back, and the record of why must survive it.
`servicelater jobs runs` lists them; they are purged after 30 days.

### 6.2 Built-in actions

| Action | before | after | async |
|---|---|---|---|
| `abort` (message) | yes | yes | no |
| `set` (field, value) on the record | yes | no | no |
| `comment.add` | no | yes, in the transaction | yes (built) |
| `record.update` (field, value) on the triggering record | no | no | yes (built) |
| `linked.update` (linked, set, status, body) on the open records linked to it | no | no | yes (built, §9.7) |
| `record.create` | no | yes, in the transaction | yes |
| `record.delete` | no | yes, in the transaction | yes |
| `workflow.start` | no | yes (queued) | yes |
| `notify` (template, to) — in-app built (§6.5) | no | yes, in the transaction | yes |
| `notify.email` / `notify.sms` / `notify.slack` | no | yes (queued) | yes |
| `http.request` | no | no | yes |

### 6.3 Limits

| Limit | before / after | async |
|---|---|---|
| Actions per rule | 20 | 100 |
| Time per save (all sync rules together) | 250 ms | n/a |
| Rule chain depth | 5 | 5 |
| Expression cost | CEL cost limit | CEL cost limit |
| Lookups per rule | 5, each counting up to 1000 rows | 5, the same |
| On overrun or error | **Fail closed**: the save is rejected with a message naming the rule | Retry, then dead-letter |

`on_error: skip` can be set per rule for non-critical rules, so a broken rule degrades instead of blocking work.
The defaults are starting points to be tuned against the benchmark in slice 2. Because there is no interpreter, the
cost of a rule is an expression evaluation plus a few database writes, which is small and predictable.

Recursion is bounded: a rule that writes another record triggers that record's rules, and chains are capped at a
fixed depth. A rule also cannot re-trigger on a change made by its own app (the causation chain, §11).

### 6.4 What is built

Before and after rules run on create, update and status transitions (`delete` and `async` are rejected at install for
now). The engine is `internal/rules`; it does no database work, so the API applies its result inside the save.

- **Actions today:** `abort`, `set` (title, description, priority, assignee_id, assignment_group, resolution) and
  `comment.add`. `assignment_group` takes a group's *name* (an app cannot know the ids of the install it lands in);
  a name that matches no group fails the save. Anything
  else is rejected at install with the list of what exists. `set` values are checked at install (a literal's type and
  range, an expression's result type) and again at save time, with the checks a request goes through.
- **Order:** the `task` rules first, then the type's own; within each by `order`, then app, then rule name. `set`
  changes are visible to later rules and actions. A rule sees the ticket as it *would be*; on a status transition that
  is the ticket after the move, as a guard sees it.
- **Failure:** an abort returns the rule's message as a 422 and nothing is saved. A rule that cannot be evaluated (a
  division by zero, the cost limit) blocks the save with a fixed message and logs the details, unless it says
  `on_error: skip`. The 250 ms budget is checked between steps and inside expressions and is never skippable.
- **Actor:** rules act as the person who saved, under RLS. A rule that does something that person may not (a requester's
  create being auto-assigned, an internal note on their own ticket) fails the save. An after rule can say `run_as: app`
  to act as its app instead (§11.1), and async rules always do.
- **Async rules:** `timing: async` rules may only `comment.add` for now (`abort` and `set` are rejected at install:
  the save has already happened). A save queues them in its own transaction, after its after rules, so a save that
  rolls back queues nothing. The job carries ids only (app, rule, record, event, actor). Filters that cannot be
  evaluated at the save queue nothing and are recorded as failed; the save is never blocked. `on_error: skip` is
  rejected for async rules, because a failing run is retried with backoff and then dead-lettered. A ticket that is
  gone or hidden, a rule that was uninstalled, or an action the actor may not take are recorded and finished without
  retrying. The worker keeps its own registry, reloaded by `NOTIFY`, so a worker started alone knows every rule.
- **One save path.** Creating, editing and moving a ticket is `internal/records` (`Service.Create`, `Update`,
  `Transition`), called by the api for a person's request and by the worker for an async rule's update. Validation,
  before and after rules, the audit and the queuing of async rules therefore happen the same way whoever asks, and
  nothing else writes to `tasks`. The api handlers only decode, call it and map its `*records.Error` to a status.
- **`record.update`** (async only; `set` is its before-rule counterpart) takes the same fields as `set`. The run's
  actions change the record in memory, each seeing the earlier ones, and the difference is then saved as **one edit**
  through `Service.Update`, as the app (for the person whose change set the rule off): the app must hold `records:update` for the type, the row is locked before
  the rule reads it, and every before and after rule judges the change. A refusal (a before rule aborts, a value is
  not allowed) is recorded as failed and not retried, and undoes the run's comments too. It updates the triggering
  record only; other records arrive with `record.create` and friends.
- **`linked.update`** (async only) changes the open and waiting records **linked** to the triggering one: `linked`
  names the link (`apps.LinkedRecords`; today only `incidents`, from a problem), `set` takes `set` fields (not extra
  fields yet) checked against the linked type, `status` a status of the linked type's lifecycle, `body` a comment on
  each. Values are worked out once, against the triggering record. Each linked record is its **own job**
  (`rule.linked`, `ruleworker.LinkedHandler`): comment, update and move as one save through `Service`, as the app for
  the person, carrying the run's cause (so the linked records' saves cannot set off the same app's rules). A record
  that refuses (a guard, a before rule) stays as it was and the refusal is audited against it; one that has settled
  or been unlinked since is `skipped`. Adding a link is a core change (a typed column).
- **Loop protection.** An update raises `update` events, which fire async rules, whose updates raise events. Two
  things cut that. Every queued run carries its **cause**: the chain of `app/rule` runs behind it and its depth. What a
  run saves is judged against it (`ruleruns.Cause.Blocks`): a rule may not be set off by a change made by **its own
  app** (an app's rules do not trigger each other, so two rules of one app cannot ping-pong), and a chain is cut at
  **depth 5** however many apps it crosses. Rules of different apps may set each other off. A blocked rule is not
  queued and is recorded as `skipped` with the reason, so a cut loop leaves a trace. A person's own save has no cause.
  Before and after rules cannot loop (they write nothing that raises events).
- **Layers:** a rule is a definition like any other, so `local/` can change one field of a shipped rule (`order`,
  `on_error`, an action) without copying it.

### 6.5 Notifications (built: in-app and email)

Who hears about what is app content, not code. It is split in two, so the wording and the policy change separately:

- **What it says** is a `notification` definition in `notifications/*.yaml`: a `title`, an optional `body`, and
  `channels`: `in_app` (always required) and optionally `email`; `sms` and `slack` are rejected as not supported yet. Placeholders come from a
  fixed list checked at install: `{{record.reference}}`, `record.title`, `record.type`, `record.status` (its label),
  `record.priority`, `record.requester_name`, `record.assignee_name`, `record.assignment_group`, `{{actor.name}}`.
- **When, and to whom** is a rule (after or async) with a `notify` action: `template` (a template in the same app,
  checked at install) and `to`, a list of `assignee`, `requester`, `assignment_group` (its active members) or an
  `=expression` giving user ids.

```yaml
# apps/core-itsm/default/notifications/ticket-assigned.yaml
kind: notification
name: ticket-assigned
title: "{{record.reference}} was assigned to you"
body: "{{record.title}}"

# apps/core-itsm/default/rules/notify-assignee.yaml
kind: rule
name: notify-assignee
record_type: task
timing: after
on: [create, update]
when: has(record.assignee_id) && (!has(old.assignee_id) || record.assignee_id != old.assignee_id)
on_error: skip
run:
  - action: notify
    template: ticket-assigned
    to: [assignee]
```

`core-itsm` ships two: `notify-assignee` (the new assignee) and `notify-group` (the assignment group's members, when a
ticket lands there with nobody on it). An operator rewords a template or changes who is told in `local/`, like any
other definition.

What the platform enforces whatever a rule says (`app_notify`, migration 00018): nobody is told about their own change,
nobody deactivated, and nobody who cannot see the record (staff, or its requester), so an end user in a group never
learns of someone else's ticket. At most 1000 recipients per action. The row stores the app, template, record and
actor, never the text: the template is filled in when the recipient lists their notifications, from the record as
*they* may see it, so a notification never says more than its reader could look up (and a record they can no longer
see drops out of their list). A template whose app was removed shows "Update on INC42". Delivery happens in the
save's transaction, so a rolled-back save tells nobody; a template missing at run time is recorded in `action_runs`
and never blocks the save.

**Email** (migration 00029, `internal/notify`): when the instance's email settings turn it on, a trigger on
`notifications` queues an email (an `email_deliveries` row and a `notify.email` job, in the same transaction) for each
notification whose template lists `email`, and for each `approval_pending` while the `email.approvals` setting is not
off, to recipients who have not turned email off (`users.email_notifications`, on their account page). The worker
fills the template in **as the recipient** (their RLS, `notify.Renderer`, the same code the bell uses), so an email
never says more than they could read in the app; it is plain text with a link back (`email.base_url`) and an
`Auto-Submitted` header. One read in the app before the worker reaches it is skipped. A refusal from the mail server
(5xx, a failed sign-in) fails it at once; anything else is retried by the job queue and fails after the last try. Every
email and its outcome is in the delivery log (Admin → Email). Sending uses `github.com/wneessen/go-mail`
(STARTTLS, TLS, or none only to a relay on this machine). core-itsm 0.4.0's templates send email.

**Still hard-coded, and must move to templates or config:**

- The `approval_pending` notification (title "Something needs your approval", link `/approvals`) is created by a
  database trigger and worded in Go (`internal/api/notifications.go`). It should become a template that the approval
  step names, so an app can word it and decide who else hears about it.
- Opening a ticket marks every notification about it read. Whether a kind clears on view, on action, or only when
  dismissed should be part of the template.
- The placeholder list and the fixed recipient kinds (`assignee`, `requester`, `assignment_group`) are code. Adding
  one is a platform change, not an app change.
- Per-person preferences are one switch (email on or off); muting a kind or choosing a channel per kind belongs in
  config, not in each rule.

### 6.6 SLAs (built)

A service level agreement is a definition kind of its own (`slas/*.yaml`), not something built from waits: a
hand-rolled timer cannot pause while a record waits on someone else, or count only working hours (F7 in
apps-examples.md).

```yaml
# apps/incident-management/default/slas/resolve.yaml
kind: sla
name: resolve
label: Resolve                  # what people see next to the clock
record_type: incident           # or task, for every type
# start, pause and stop are conditions on the record (the rule environment: record, old, user, now, event).
# Left out, they are: start true; pause record.state_category == "waiting"; stop state_category in done/closed.
target: '=record.priority == 1 ? duration("4h") : record.priority == 2 ? duration("8h") : duration("24h")'
calendar: business-hours        # a business calendar; left out, every hour counts
warnings:                       # at most 3, lowest first
  - at: 75                      # percent of the target used
    run:
      - action: notify
        template: resolve-warning
        to: [assignee]
on_breach:
  - action: notify
    template: resolve-breached
    to: [assignee, assignment_group]
```

`target` is a literal (`4h`, `90m`, `2d`, `1d12h`) or an `=expression` giving a CEL duration (write `duration("48h")`:
CEL durations have no days). Warning and breach actions are `notify`, `workflow.start` and `comment.add`, checked at
install exactly as an async rule's are (templates and workflows must be in the same app); a workflow they start can
reassign or escalate. `record.update`, `set` and `abort` are refused: nobody is saving.

**Clocks are rows** (`task_slas`, migration 00025): one per record per SLA, holding its stage (running, paused,
stopped, cancelled), target, the working time used before it last started running, the due time, when it breached,
how many warnings were sent, and when the timer must next look at it. A clock keeps a copy of the definition it
started with (as a workflow execution does), so editing an SLA changes new clocks, not running ones. RLS shows and
lets a save write a clock exactly when its record is visible.

- **A save moves the clocks** (`internal/records/slas.go`, in the save's transaction, as the person saving, after the
  write and before the after rules). A clock attaches the first time its `start` holds; then on every save `stop` is
  checked, then `pause`, and the clock runs if neither holds. So a pause begins the moment the record changes, with no
  queue in between. The target is worked out again on every save, keeping the time used: raising the priority
  shortens the time left. A stopped clock (met, or breached if it breached on the way) runs again if `stop` stops
  holding: a reopened incident picks up where it stopped. A clock whose app no longer defines it is cancelled.
- **The timer** (`internal/slatimer`, in the worker role, every second) claims running clocks whose next warning or
  breach is due (`FOR UPDATE SKIP LOCKED`, so several workers can run it), records them and queues an `sla.alert` job,
  once per alert (a dedupe key). A save that finds a clock past its target records the breach itself, so nothing
  waits on the timer. A breach found by the timer notifies `ticket_changes`, so open pages update at once.
- **The alert job** runs the actions as the app's principal acting for nobody (no person's change set it off), through
  the same runner as async rules. The app needs `records:read:<type>` in its manifest to see the record. Each run is
  audited in `action_runs` with timing `sla`.
- **Failing safe.** An SLA that cannot be worked out never blocks the save: a clock that cannot tell whether it starts
  is not started, one already running keeps its stage and shows the problem, and both are audited (`timing: sla`,
  event `evaluate`).

**Business calendars** are core reference data (`business_calendars`, like freeze windows; open question 11): a name
(what `calendar:` writes), a label, an IANA time zone, the working week (stretches per weekday, `HH:MM` to `HH:MM`,
end may be `24:00`) and holiday dates. Everyone signed in reads them; `calendars:manage` (admins) writes them, in
Admin > Calendars or at `/api/v1/business-calendars`. Hours are wall-clock hours in the zone, so daylight saving
moves UTC, not the office day. Editing a calendar asks the timer to reschedule every running clock that counts by it;
one that running or paused clocks count by cannot be deleted. A calendar that is missing or broken counts every hour,
and the clock says so. `internal/businesstime` does the arithmetic (`Elapsed`, `Add`); a nil calendar is 24x7.

`incident-management` ships `respond` (met by an assignee; 30m to 8h by priority) and `resolve` (4h to 40h by
priority), both counted by the `business-hours` calendar (Monday to Friday, 09:00 to 17:00 UTC, created by the
migration, which operators edit), with warnings at 75% and breach notifications. The ticket page shows each clock
(time left, paused, met, overdue, missed); the queue shows the soonest due time or "SLA breached", and its Due view
(`GET /tickets?sort=due`) lists breached work first, then by due time. `?breached=true` filters to breached tickets.

Not built yet: SLA reporting (met and missed rates), a reset condition (a new clock when, say, the record is
reassigned to another team), pause reasons, per-SLA schedules chosen by expression, and SLAs on records other than
tasks.

## 7. Workflows

Workflows are the advanced tier: anything that waits, branches, loops, asks a person, or calls another system.
The definition is a graph stored as data (§4.4) and edited in the visual builder.

### 7.1 How they run

A workflow is **interpreted by a durable engine in Go**, not translated to code (D12):

- **Long-running.** An approval can wait days. The engine persists its position in `workflow_executions` and
  `workflow_steps`, and resumes after a restart or deploy.
- **Live and auditable.** The builder highlights the running step in real time (over the WebSocket hub), and every
  step's input, output, timing and error is recorded.
- **Each step is a Go function** taking a typed input and returning a typed output. The engine claims work through
  the job queue (`SKIP LOCKED`), retries failures with backoff, applies per-step timeouts, and uses an idempotency
  key so a retried step does not repeat its effect.
- **Version-pinned.** A running execution keeps the definition version it started on; new executions use the
  current one (MVP doc §5.5).
- **Acyclic.** A workflow is a DAG. Repetition comes from `foreach` and `wait`, not from graph cycles, which keeps
  every execution finite and easy to reason about.

Triggers are a rule's `workflow.start`, a `schedule` (a cron expression in the workflow's `trigger:` block), a manual
run, or a webhook (MVP doc §4.4). Schedule and rule triggers are in v1; manual and webhook follow.

### 7.2 Step library (v1 proposal)

This is the extension surface: advanced behaviour is built by composing these steps.

| Group | Steps |
|---|---|
| Control | `condition` (branch on an expression), `foreach` (run a sub-flow per item, bounded, one item after another), `parallel` and `join` (later), `wait` (a duration, or until an expression is true, with a timeout), `subflow`, `end` |
| Data | `set` (workflow variables), `record.get`, `record.query` (bounded, returns a list), `record.create`, `record.update`, `record.delete`, `comment.add` |
| People | `approval` (one or many approvers, with escalation on timeout) |
| Outbound | `notify.email`, `notify.sms`, `notify.slack`, `http.request` (declared hosts, response readable as JSON), `webhook` |

For example, "for each affected CI, open a task and wait for all of them to close" is `record.query` → `foreach`
over `record.create` → `wait until` all are done. No code is needed.

**Working with lists.** `record.query` returns several records, and `foreach` iterates them:

- **Output**: `steps.<id>.output` has `items` (a list of records, with references expanded like `record`), `count`,
  and `has_more`. A query always has a `limit` (default 100, maximum 1,000) and an `order_by`, and it takes the same
  AND-only filters as lookups (§5). A `null` filter value means "is empty".
- **`foreach`** takes `items` (any list, normally a query's output), a `max` cap, and either a subflow to `run` for
  each item (passed as `with`) or, for the common single-step case, an inline `do:` step. Items run one after another in v1 (running several at once, `parallel: N`, is a later
  addition). If an item
  fails, `on_item_error: stop` (the default) halts the loop and `continue` records the failure and moves on.
  The output has `results`, one entry per item.
- **A snapshot, not a live view.** The list is fixed when the query ran. Records can change before their turn, so a
  subflow that acts on an item should re-check its state first.
- **Cost.** Each item is its own subflow execution with its own audit rows, which is right for a few hundred items
  and wasteful for thousands. For mass changes a bulk step (`record.update_many`, `record.delete_many`, one
  statement with a filter) is the better tool; it is a candidate for the step library, not a v1 requirement.

**Creating and deleting.** `record.create` takes a `record_type` and a `set` map, and its output is the new
`record` (with its `id` and `number`), so later steps can use it. In a workflow, a retry of the same step reuses an
idempotency key, so it never creates a second record. `record.delete` takes a `target`. Both need an explicit
permission (`incidents:create`, `incidents:delete`), which the install plan highlights. A delete removes the row and
its comments and leaves an audit tombstone (who, when, which number). Normal lifecycles should close or cancel
records instead; delete is for cleanup. A `before` rule on `delete` can block it. Neither is allowed in a `before`
rule, because the write has not happened yet.

### 7.3 Reuse and extension

- A workflow with declared **inputs** is a reusable **subflow**, so an app can package a common procedure and
  other workflows can call it. This is how apps offer "custom actions" without custom code.
- New step types are implemented in Go and ship with the platform, versioned and tested like the rest of it.
- Because every step already names a `type`, a `script` step (Starlark, Lua or WASM) could be added later as one
  more type, first in async workflows only, without changing the app format (D5).

### 7.4 What this costs us

We should be explicit about the trade-off of having no scripting language:

1. **Blocking logic is limited to expressions and lookups.** A rule that must reject a save based on something
   we did not anticipate cannot be written in v1. The workaround is an async workflow that flags or reverts the
   change after the fact, which is weaker.
2. **The step library becomes a product surface we must grow.** Every gap ("I need to parse this XML") is a
   feature request to us, not something an admin can fix themselves.
3. **Some customers will ask for scripting.** The `type` extension point means we can add it without redesign, and
   the criteria for doing so are in §13.

In return there is no interpreter to sandbox, no memory limits to enforce, no thread-safety hazards, and no
runaway user code, and every step is code we have tested.

### 7.5 Concurrency and throughput

Goroutines are how a worker runs many executions at once, but they sit **on top of** the durable queue and do not
replace it.

- **Model.** A `worker` runs a bounded pool of executors, each a goroutine running the same loop: claim a job
  (`SKIP LOCKED`, with a lease), run the execution forward until it must wait or has finished, persist the result,
  claim the next. The executors share nothing and do not coordinate, because the queue hands each one different
  work. The pool size is `SL_WORKER_CONCURRENCY` (a small default, tuned by the slice 6 harness). More throughput
  comes from a larger pool, then from more worker processes.
- **All-in-one installs.** The workers share the process and the database with the API, so the pool is bounded and
  workers draw on their own connection budget. A burst of workflows can slow other workflows but cannot starve web
  requests of database connections. Most workflows at any moment are waiting, which costs rows and not executors,
  so a modest pool serves hundreds of concurrent workflows.
- **Waiting is data, not goroutines.** A `wait` is a row with a wake-up time, and a timer loop enqueues those that
  are due. A `wait until` condition is indexed by record and re-evaluated when `NOTIFY` reports that the record
  changed. A million waiting workflows cost rows, not goroutines, and survive a restart.
- **What actually limits speed** is database round trips (persisting state after each step), external calls, and
  queue latency. A step's own CPU cost is negligible. So goroutines raise how many executions run at once but do
  not make one execution faster. The levers that do:
  - run consecutive fast steps in the same goroutine and persist once per segment, not once per step;
  - wake workers with `LISTEN/NOTIFY` instead of polling, and claim jobs in batches;
  - make external I/O happen outside database transactions and never hold a connection while waiting on a network;
  - keep persisted step state small, and index due jobs.
- **No parallelism inside one execution in v1.** There is no `parallel` step and `foreach` runs its items in
  order. Concurrency comes from running many executions at once, not from splitting one, which avoids join logic
  and partial-failure handling. Both can be added later without changing existing workflows.
- **Safeguards**: per-app and global concurrency caps; one goroutine per execution at a time (the lease); panic
  recovery around every step so a bug fails one execution and never the worker; context timeouts on every step; and
  a graceful shutdown that finishes or releases leases.
- **Measure, then tune.** Slice 6 includes a load harness reporting executions per second, step latency at p50 and
  p99, and queue lag, and the pool size is chosen from that data.

### 7.6 What is built (slice 6)

The engine, the storage, the triggers, the API and a read-only run viewer. Not yet built are listed at the end.

**Definition.** `default/workflows/<name>.yaml` (`kind: workflow`): `name`, `record_type`, `start` and `steps`, a map of
step id to definition. Layers apply as everywhere: a local file can change one step's `timeout` without copying the
workflow. Everything is checked at install and every problem is reported at once: unknown step types and fields, a
`next` that goes nowhere, a **loop** (the path is named: `a -> b -> c -> a`), a step nothing leads to, expressions that
do not compile or have the wrong type, and a `wait until` with no `timeout`. A rule's `workflow.start` must name a
workflow of the same app and record type.

**Steps in this version:** `set` (variables, one `=expression` or literal each, in order, so a later one can use an
earlier one), `condition` (`if`, `then`, `else`), `wait` (`for: 15m`, or `until: <expression>` with a `timeout` and an
optional `on_timeout` step; without one a timeout fails the run), `record.update` (the same fields and checks as a
rule's), `record.transition` (`to: <status>`: moves the record through `records.Service.Transition`, so the move
must exist and its guard must agree, exactly as for a person; it may also make `automatic` moves; a refused move
fails the run with the reason; if the workflow's app defines the type's lifecycle, `to` is checked at install),
`comment.add`, `approval` and `end`. Expressions see `record` (as it is *now*, re-read for every step),
`user`, `now` and `vars`. Variables hold text, numbers or true/false, and are stored with the execution.

**Approval.** `approvers` is an expression producing a list of user ids: `group('CAB')` is the ids of a group's
members, as of when the step runs, and lists combine (`group('CAB') + [record.requester_id]`). `group()` exists only in
workflow expressions, takes the name in quotes (so the groups an expression needs are known when it is compiled), and
fails the step if no such group exists rather than asking nobody; `rule` is `any` (the default), `all` or `majority`, and `timeout`/`on_timeout`
work exactly as a `wait`'s. The first visit files one pending request per approver (`workflow_approvals`); later
visits are the timer or someone deciding, waking it. The rule is recomputed from the requests every time, so it never
disagrees with what is stored, and it short-circuits as soon as the outcome cannot change: `any` stops at the first
yes, `all` stops at the first no. There is no `steps.<id>.output` yet (§7.2's aspirational syntax), so the result
lands in variables instead, namespaced by the step's id: `vars.<id>_decision` (`approved`, `rejected` or, on timeout,
`timeout`), `vars.<id>_comment` and `vars.<id>_decided_by`, all from whichever request settled it. Deciding is
`POST /api/v1/approvals/{id}/decide` and the inbox is `GET /api/v1/approvals`; neither is staff-only, because anyone
named in `approvers` can be asked, and only the addressed approver may answer, once (RLS: `workflow_approvals`'s own
policies, plus narrow read access to the one execution and ticket a pending request is about, through two
`SECURITY DEFINER` functions, so an approver outside a ticket's normal audience can still see what they are asked to
approve without a general RLS exception). A decision wakes the execution at once (a trigger on `workflow_approvals`
brings its `wake_at` forward, the same way a `wait until` is brought forward by a ticket change), not the poll.

**Groups as approvers.** An approval may also (or instead) list `groups`: an expression producing group *names*, which,
unlike `group('name')`, can be worked out from the record, such as the owners of the items a change touches:
`groups: record.cis.filter(c, has(c.owner_group)).map(c, c.owner_group)`. Each group has **one vote**: every active
member is asked (`workflow_approvals.group_name`, migration 00035; someone in two of the groups is asked once for
each), and the first member to answer casts it; the rest of the group's requests close (`closed_at`) and leave their
inboxes. Each person in `approvers` is a vote too, and `rule` counts votes, so `rule: all` with groups means every
group must approve. A group that does not exist, or has nobody active in it, fails the step, since its vote could
never be cast. When a step is over (settled, timed out, or the run ends), any request still open closes, so an `any`
approval no longer leaves the others with something to answer. The inbox says which group a request is for.
change-management's `cab-approval` (0.7.0) uses this: a normal change linked to items with owner groups asks those
owners (all of them) before the board, and a no or five days' silence sends it back to Assess. The app's manifest now
holds `cmdb:read`, so its steps can see the items.

**How it runs.**
- An **execution** is a row (`workflow_executions`) holding the definition **as it was when it started** (so an upgrade
  never changes a run in flight), its current step, its variables and its status (`queued`, `running`, `waiting`, `done`,
  `failed`, `cancelled`); every visit to a step is a row in `workflow_steps` with its outcome, detail and timing.
- Starting one, from a rule's `workflow.start` (in an after rule it is part of the save's transaction; in an async rule,
  of the worker's) or by hand (`POST /workflow-runs`), inserts the row and queues a `workflow.run` job in the same
  transaction, so a save that rolls back starts nothing.
- A `workflow.run` job moves the execution forward **one step per transaction**, as the person who started it, under
  their RLS. The step's effect and the record that it happened commit together, so a retry never repeats an effect. A
  step that fails for a reason retrying will not change (a refused edit, an expression error, a missing ticket) rolls its
  transaction back and ends the execution in a second transaction, so earlier steps keep their history and effects.
  A database fault is retried by the job queue; when attempts run out the execution is marked `failed`.
- A job runs at most 25 steps, then queues itself again, so a long workflow cannot hold a worker.
- **Waiting is data.** A sleeping execution has a wake-up time. A timer in every worker (each second, `SKIP LOCKED`)
  queues the ones that are due. A `wait until` is also woken **when its ticket changes** (a trigger brings the wake-up
  forward) and re-checked at least every 30 seconds, because a condition may depend on the clock.
- **Loop protection carries over.** What a workflow saves carries its cause (`app/workflow`, depth + 1), so an app's
  rules do not answer its own workflow's edits, and the chain is cut at depth 5 (§6.4).
- Runs are kept for 30 days after they finish.

**API and viewer (staff only).** `GET /workflows`, `GET /workflow-runs?status=&ticket=&workflow=`, `GET /workflow-runs/{id}`
(the graph it started with, its variables and every step), `POST /workflow-runs` (`{"workflow": "app/name", "ticket": ...}`)
and `POST /workflow-runs/{id}/cancel`. The web app has **Runs**: the list, and one run with its steps (which is done, which
is current, which was not reached) and what happened at each visit. It refreshes itself while a run is unfinished.

**Measured** (`SL_BENCH=1 go test -run WorkflowThroughput -v ./internal/workflowengine`; a five-step workflow that
sets, branches, comments and edits a ticket through the full save path; 8 cores, database on the same machine):

| Executors | Executions per second | Step p99 |
|---|---|---|
| 1 | 116 | 1 ms |
| 4 | 406 | 1 ms |
| 8 | 586 | 2 ms |
| 16 | 646 | 4 ms |
| 32 | 617 | 7 ms |

Throughput scales with the pool up to about 8 and then flattens, because the database is the limit. The default stays
at 4 (`SL_WORKER_CONCURRENCY`) because in the all-in-one install the pool shares the machine and its connection budget
with the API; raise it to 8 on a dedicated worker. The step itself costs about a millisecond, so nearly all of an
execution's time is waiting for a free executor and for the database.

**Not built yet, and why:** `notify.email` / `sms` / `slack` and
`http.request` (need mail transport, secrets and host allow-lists, which arrive with the config and secrets work),
`foreach` / `record.query` / `subflow` (a bounded list and calling another workflow), `record.create` / `record.delete`,
and the `schedule` trigger. Steps run as the workflow's app (its service principal, §11.1) on behalf of the person who
started it: they can do what the app's manifest declares, whoever started it, and `user` is still that person. Live
highlighting of the running step arrives with the visual builder; the viewer refreshes by polling.

### 7.7 The workflow builder (built, slice 9)

Admins (`workflows:edit`) edit workflows in the browser at `/workflows/{app}/{name}`; staff who follow runs
(`workflows:read_all`) can open the same page to look. The builder is a Svelte Flow canvas (`lib/builder`): steps
are boxes with a dot to connect into and one dot per way out (next; then and else; next and on timeout), connections
are drawn between dots, and a selected step or connection is deleted with Delete. The panel beside it holds the
selected step's fields by type (expressions as CEL text, values as text, a number, true/false or `=expression`,
statuses from the record type's lifecycle) and the workflow's start step. Where the steps sit is saved with it
(`layout:` in the definition, ignored by the engine), so a diagram stays as it was arranged.

Saving sends the whole definition (`PUT /workflows/{app}/{name}`). The server writes it back as YAML
(`apps.WorkflowYAML`, the same file an author would write) and keeps, in the app's studio layer, **only what changed**
over the workflow as the layers below make it (`apps.WorkflowOverride`: changed keys, `null` for removed steps and
fields), so the app's later changes to the parts the edit left alone still arrive; a workflow of a new name is kept
whole, and positions are kept only for steps someone placed. If merging the smallest override would not give exactly
what was drawn, the whole workflow is kept with `override: replace`. The server compiles the app with every layer, and installs it in the same transaction as
the override row: an edit that does not compile, or that the install refuses, changes nothing and answers with every
problem, as `servicelater app validate` would. A workflow of a new name is created in the chosen app;
`DELETE /workflows/{app}/{name}/edits` takes the edits away (back to the app's version, or gone if it was created
here). Running executions keep the snapshot they started with. The page shows the stored change (what
`export-local` writes) and the whole definition as YAML, and what the app may do: a workflow's steps act as its app, so one created in an app
whose manifest reads no records cannot touch them (the builder says so, and suggests the app that owns the type).

**Where its runs are, live.** The builder shows, on each step, how many runs are there now (highlighted with the
marker, as the run viewer marks a run's current step) and how many visits the runs started in the last 7 days made,
and how many failed there; the panel lists the runs in flight with their records. `GET /workflows/{app}/{name}/activity`
gives it, and a trigger on `workflow_executions` (00034) sends `run.changed` (run id, app and workflow name only, to
those with `workflows:read_all`) when a run starts, moves or ends, so the page asks again; a 30 second refresh covers
waits that end on their own time. A run started on an older version can be at a step the current one no longer has.

Not built yet: editing rules and the other definition kinds in the browser, undo, comparing an edit with the app's version, and
starting a workflow from its own page.

## 8. Declarative UI

Apps describe what to show; the core Svelte UI renders it (D4).

- **Forms** (built, §8.1): fields (type, label, required, options), layout, and expression-driven show/hide and validation.
- **List views**: columns, default filters, and sort for a record type.
- **Dashboards**: tiles bound to saved queries.

There is no way for an app to ship executable UI code in v1. If we later want custom components, they would be
prebuilt web components loaded into named slots, but that is deliberately deferred.

### 8.1 Forms (built)

A form is a definition kind (`forms/<type>.yaml`, `internal/apps/form.go`), one per record type: two apps cannot
both provide the form for a type, as with lifecycles. It does three things.

**It declares extra fields.** They are stored in `tasks.fields` (jsonb, migration 00026), so an app adds fields
without a table change (D6). Types: `text`, `textarea`, `number` (whole), `boolean`, `date` (YYYY-MM-DD), `datetime`
(RFC 3339), `choice` (options, each optionally offered only `when` a condition holds). Each can have a `label`,
`help`, `default`, `max_length`, `min`/`max`, and the conditions below. A field name is lowercase with underscores
and may not be one of the record type's own columns.

**It places and relabels core fields.** A record's own columns (title, description, priority; an incident's
resolution; a change's type, risk, impact, window and plans) are listed in `apps.CoreFields`. A form can give one a
label and help and the same conditions, never a type or options.

**It lays them out and says when each applies.** `layout.create` is the form for filing one (it must include
`title`); `layout.detail` is the record's sections (title, description and priority have places of their own).
Every extra field must be in one of them. Conditions are CEL in the rule environment (no lookups):
`required` or `required_when`, `visible_when` (a hidden field is never required), `read_only_when` (people may not
change it; rules and workflows may), and `validate` checks on the whole record (`when` must hold, else `error`).

```yaml
kind: form
record_type: incident
fields:
  category: { type: choice, label: Category, options: [hardware, network] }
  subcategory:
    type: choice
    label: Subcategory
    visible_when: has(record.fields.category)
    options:
      - { value: vpn, label: VPN, when: 'record.fields.category == "network"' }
  priority:                      # a core field: only label, help and conditions
    read_only_when: has(record.fields.impact) && has(record.fields.urgency)
layout:
  create: [title, description, category, subcategory, priority]
  detail:
    - section: Classification
      fields: [category, subcategory]
validate:
  - when: '!has(record.fields.subcategory) || has(record.fields.category)'
    error: Choose a category first
```

**The server enforces all of it, on every save** (`records.checkForm`, after the before rules, for creates, edits
and moves alike, whether a person, a rule or a workflow saves): an extra field that changed must be declared and
hold a value of its type within its limits, an offered option, and every shown required field must be filled in.
A value already stored that the form no longer allows stays until someone changes it. Read-only is judged on what a
person's request asked for, before the rules run (`checkReadOnly`), so a rule can keep setting a read-only field and
a person's own change to it is refused, not silently undone. A condition that cannot be evaluated refuses the save,
as a guard that cannot be checked blocks its move. Making a field required affects every later save of a record
that lacks it, including a workflow's.

**Other definitions read and set extra fields** as `record.fields.name` (and `set` / `record.update` with
`field: fields.name`), type-checked at install against **the same app's** form for their record type: a typo or a
text field compared with a number fails the install. Each definition stores the types of the fields it names
(`fields` in its body), so it compiles the same way from storage, from a clock's or an execution's snapshot,
whatever the form becomes. A field that is not set is an error to read, so test it with `has(record.fields.x)`.
Reading another app's fields is not supported yet.

**API and UI.** `GET /forms/{type}` gives the form (every placed field, resolved, with `reactive`: the fields its
conditions read). Single tickets carry `fields` and `field_states` (hidden, required, read-only, offered options).
While filing, `POST /forms/{type}/states` gives the states for a draft and, in `values`, what the before rules would
set (they are tried on the draft; nothing is written or audited), so a read-only priority shows its worked-out value;
the dialog asks again when a reactive field changes. The web keeps no field list: `NewTicket.svelte` and
`RecordFields.svelte` render whatever the form says. A type with no app form gets one made of its own fields
(`apps.FormFor`).

**Customising.** Operators change a shipped form in `local/` like any definition: fields merge by name, so a
local file can add a field or relabel one; layout lists replace the shipped ones whole. Editing forms in the UI
would write the same local layer, once it can be kept in the database (open question 16).

Not built yet: forms on `task` (inherited by every type), user and group pickers as field types, reading another
app's fields, per-field read permissions (a hidden field is not a secret: anyone who can read the record can read
its fields through the API), extra fields in list views, search and lookups, and editing forms in the UI.

### 8.2 CMDB (built)

Configuration items are **core data people edit** (`cis`, migration 00027), like freezes: an item has a `class`, a
`name` (unique within its class, case aside), a `status` (planned, active, maintenance, retired: core values,
`apps.CIStatuses`), a description, an owner group and a support group, and its class's fields in `fields` (jsonb, as a
ticket's extra fields). What an item *can be* and how items *relate* are app definitions, shipped by the `cmdb` app
and changed in its local layer. The design aims at the useful core of a CMDB without the depth of ServiceNow's: no
table per class, a handful of relationship types, no discovery or reconciliation engine yet.

**Classes** (`classes/<name>.yaml`, `internal/apps/cmdb.go`): `label`, `description`, optional `extends` (a class of
the same app, at most 5 deep, no loops) and `fields` in the form field syntax (`text`, `choice`, `date`...; `required`,
`default`, limits; no conditions, since an item is not a record). A class has the fields of the classes it extends
first, and counts as each of them: `GET /cis?class=hardware` finds servers too (`Registry.ClassAndDescendants`). Two
apps cannot define one class name. `cmdb` 0.2.0 ships ten: business service, application, database, hardware with
server, computer, mobile device and network device under it (load balancers and firewalls are a network device's
`device_type`), cloud resource and other. An operator adds a class, or a field to a shipped one, in `local/`:

```yaml
# local/classes/load_balancer.yaml
kind: class
name: load_balancer
label: Load balancer
extends: network_device
fields:
  vip: { type: text, label: Virtual IP }
```

**Relationship types** (`relationships/<name>.yaml`): `label` reads from the first item, `reverse` from the second,
and `impact: true` says trouble with the second affects the first. Shipped: `depends_on` (Used by) and `runs_on`
(Runs) carry impact; `part_of` (Contains) and `connected_to` do not. Relationships are rows (`ci_relationships`:
`from_ci`, `to_ci`, `type`); removing a type's definition leaves its rows, which then carry no impact.

**Impact** is the items trouble reaches: the impact relationships followed backwards (what depends on it, what runs on
it, and so on), breadth first, one query per step, each item once (so loops end), at most 10 steps and 1000 items
(`internal/cmdb.Impact`). Postgres needs no graph extension for this; a recursive CTE was rejected because it
enumerates paths, which explodes on dense graphs.

**Records link to items** (`task_cis`): an incident's affected server, a change's database. Linking or unlinking is
an **update of the record** through the one save path (`records.Patch.SetCIs`, or `cis` when filing), so before,
after and async rules run on it and can refuse it; it needs `cmdb:read` and update on the record (RLS checks both).
Expressions read the linked items as `record.cis`, a list of `{id, name, class, status, support_group(_id),
owner_group(_id)}` in name order, as the person saving may see them (none without `cmdb:read`); test a group with
`has(c.support_group)`, and a change of items with `size(record.cis) != size(old.cis)`. Single tickets carry `cis`
(only for `cmdb:read`), `GET /tickets/{id}/impact` is what the linked items reach, and an item lists the records
linked to it. An item linked to a record cannot be deleted (409): retire it.

**Routing** (cmdb 0.3.0, `rules/route-to-support-group.yaml`, a `task` rule): a record with no assignment group goes
to the support group of the first linked item that has one, when it is filed with items or its items change. A group
someone chose is left alone; turn it off or narrow it in `local/`.

**Freezes scoped to items** (`freeze_window_cis`, 00028): a freeze that lists items applies only to records linked to
one of them; one that lists none applies to every change. Lookups filter freezes with
`applies_to: { in: =record.cis.map(c, c.id) }` (only `in`; a freeze for everything always matches), and
change-management 0.6.0's freeze rule and Implement guard do so, so a change is checked again when its items change.
Everyone sees which item ids a freeze lists; names need `cmdb:read`. The calendar marks a change only with the freezes
that apply to it (for a reader who cannot see links, every overlapping freeze, erring on the side of the freeze).
change-management's `change-manager` role now holds `cmdb:read`, since the check reads the items as the person saving.
A freeze on an item does not reach what depends on it yet: freeze the database, not only the service.

**Permissions**: `cmdb:read` (agents and admins) sees items, relationships and links; `cmdb:manage` (admins, and the
cmdb app's `cmdb-manager` role) changes items and relationships. End users see none of it, including on their own
tickets. Realtime `ci_changes` → `ci.changed` (ids only, to `cmdb:read`).

**API**: `GET /cmdb/model` (classes with resolved fields, relationship types, statuses), `/cis` (list with `q`,
`class`, `status`, `support_group`, `include_retired`; CRUD), `POST|DELETE /cis/{id}/relationships`,
`GET /cis/{id}/impact`, `PUT|DELETE /tickets/{id}/cis/{ciId}`, `GET /tickets/{id}/impact`. **Web**: `/cmdb` (list),
`/cmdb/new`, `/cmdb/{id}` (details, relationships, "If it fails", linked tickets), and an "Affected items" section on
the ticket.

Approvals by an item's owner: an approval step's `groups` (§7.4), used by change-management's `cab-approval`.

**Import and export (CSV).** Items come in from spreadsheets, one class per file, and relationships in a file of
their own (`from_class, from_name, type, to_class, to_name`). `GET /cmdb/csv/items/{class}` (cmdb:read) exports a
class's items in exactly the format the import reads (`?template=true`: the header row only: `name, status,
description, owner_group, support_group`, then the class's fields, inherited first); `GET /cmdb/csv/relationships`
does the same for relationships. `POST` to the same paths (cmdb:manage) with the file as the body imports it:

- **A class's file covers its family**: the class and every class extending it (hardware: computers, servers...).
  When it has any, the template and export have a `class` column (a name or label of the family; empty is the file's
  class) and every field of the family, inherited first; a cell for a field the row's class lacks is a problem.
  Exporting a class includes its family's items, so a family is managed in one spreadsheet.
- Rows match items by class and name (case aside, as `cis_class_name_key` does): a new name adds an item, a known one
  updates it. A new name that another class of the family already has is a problem ("there is already a server
  called web-01: set class to Server to update it"), not a second item; moving an item to another class is left to
  the UI. An empty cell leaves the value alone; an import never clears a value, removes an item or a
  relationship. A name twice in one file is a problem on the second row.
- Headers may be column names or labels. Cells take what the UI takes, checked by the same code (`applyCI`,
  `cmdb.ApplyFields`): statuses and choices by value or label, booleans as yes/no, dates as YYYY-MM-DD, groups and
  relationship types by name (types also by how they read, "depends on"). An unknown column refuses the whole file,
  so a typo never drops a column quietly.
- **Preview first:** without `apply=true` nothing is written and the answer lists every row as create, update (with
  the columns it changes), unchanged or error (with why). `apply=true` imports in one transaction only if no row has
  a problem (422 with the report otherwise); `skip_errors=true` imports the rest.
- Files are read as spreadsheets save them (`internal/cmdb/csv.go`): a byte order mark, comma, semicolon or tab, at
  most 5 MB and 10,000 rows. Exports start with a byte order mark and escape cells a spreadsheet would run as a
  formula; the import undoes that, so an export uploads again unchanged.
- An import writes history like any change, as the person importing. It sends one `ci.changed` event with
  `bulk: true` instead of one per row (migration 00036 quiets the triggers for the transaction via `app.ci_bulk`).

Web: `/cmdb/import` (Import on the CMDB list): pick a class or relationships, download the template or what is there,
see what each column takes, upload, check the preview, import.

Not built yet: freezes that follow impact, lookups over items, sync from other systems (and matching on a field
other than the name, such as a serial number), Excel files, clearing values by import, and items for requesters (picking the affected service when filing).

### 8.3 Knowledge base (built)

Articles are **core data people write** (`kb_articles`, migration 00030), like configuration items; the
`knowledge-base` app (0.2.0) ships who looks after them (the `knowledge-manager` role). An article has a reference
(KB12, its own sequence), a title, a Markdown body (stored as written; the web renders it with `marked` and sanitises it
with DOMPurify, so the body is never trusted HTML), a category (free text, offered from the ones in use), tags, an
audience (`everyone`, or `internal` for staff) and a status:

- `draft` → `review`: a writer (`knowledge:write`, agents) sends it for review, or pulls it back;
- `review` or `draft` → `published`: those who publish (`knowledge:publish`, admins and knowledge managers);
- `published` → `retired`, and `retired` → `published` or `draft`: those who publish.

The server offers each reader the moves they may make (`moves`). Writers change drafts and articles in review; a
published or retired article only those who publish (it stays published). Every content save is a new version
(`kb_article_versions`), readable as a whole. Only an article never published can be deleted; one that was is retired.

**Who reads what** (RLS, `app_can_read_article`): a published `everyone` article, anyone signed in; a published
`internal` one, `knowledge:read_internal` (agents) or those who publish; anything not published, writers and those who
publish.

**Search** is Postgres full-text search: a stored `tsvector` (title and tags weigh most, then category, then body;
`app_kb_search`, English) with a GIN index, `websearch_to_tsquery` (words, "phrases", -word), ranked by `ts_rank`,
with a `ts_headline` snippet whose matches are marked with U+2E22/U+2E23 rather than HTML (the web highlights them).
`GET /kb/suggest?q=` treats a sentence as any-of-its-words and returns up to five published articles: the New ticket
dialog shows them under the summary as it is typed.

**Records link to articles** (`task_articles`): whoever may update the record links one; anyone who sees the record
sees the linked articles they may read (a requester sees the public article an agent linked to their incident). An
agent who writes articles can start one from a resolved or closed record: it starts with the record's description
and resolution, and is linked to it.

API: `GET /kb/articles` (`q`, `category`, `status`), `GET /kb/suggest`, `GET /kb/categories`, `POST|GET|PATCH|DELETE
/kb/articles/{id}` (id, KB12 or 12), `POST /kb/articles/{id}/status`, `GET /kb/articles/{id}/versions[/{n}]`,
`PUT|DELETE /tickets/{id}/articles/{articleId}`. Web: `/kb`, `/kb/new` (`?from=INC42`), `/kb/{ref}`, `/kb/{ref}/edit`.

Not built yet: app-shipped articles (the `knowledge/` folder is still rejected), comparing two versions side by side,
feedback and view counts, attachments and images uploaded with an article, search in other languages (the search
configuration is English), notifications when an article is sent for review, and per-group audiences.

### 8.4 Service catalog (built)

The catalog is **app content**: each item is a `catalog_item` definition (`catalog/*.yaml`, `internal/apps/catalog.go`),
installed, layered (`local/`, the studio layer) and promoted like workflows. One app provides each item name. An item
has a label, a category, a summary, a Markdown description, an icon from a fixed set (`apps.CatalogIcons`), an order,
and:

- `available_to` (`roles` and/or `groups`; absent means everyone signed in): who sees and may order it. It is checked
  on every listing and every order (`records.MayOrder`: a base role, a role granted directly or through a group, or a
  group membership). Items are definitions, not rows, so this is a Go check; the requests they file are ordinary
  records under RLS.
- its questions, in the **form field syntax** (`fields`, `layout`, `validate`, with conditions): an item's form is a
  request's form. The save path checks a request against the form of the item it was ordered from
  (`Registry.FormOf`), on create and on every later edit, as any form. An item's form does not ask for a summary: its
  `title` (text, or `=an expression` over the answers) names the request. Left out, the layouts are the questions in
  order. All of an app's items add their questions to its request fields, so its rules and workflows read
  `record.fields.x` typed (two items asking the same name must agree on its type).
- `workflow`: a workflow of the same app for requests, started as the request is filed, for the person who ordered it
  (it runs as the app).

**Requests and tasks.** A request (REQ) has its own row (`requests`: the item it was ordered from). Anyone may file one,
but only by ordering an item open to them (`task_types.from_catalog`); filing one by hand needs `records:create:request`.
Records may have a parent (`tasks.parent_id`, one level): a request's **tasks** (TASK, type `work_task`) are each a
team's part of the work, worked from their own queue. A task's requester is the request's, so its requester follows
them; the request shows its tasks, and `record.open_tasks` counts those still open or waiting. A task's change wakes
workflows waiting on its parent and tells the parent's watchers.

**Fulfilment** uses the workflow engine: `approval` steps (the requester's manager is `record.requester_manager_id`,
from `users.manager_id`, which only those who manage people set; a second level is another approval step), and
`task.create`, which files a task (`title`, `description`, `group`) and waits until it is done or cancelled
(`vars.<id>_outcome`: done, cancelled, timeout), or with `wait: false` files it and carries on, so several tasks run at
once and a later `wait` until `record.open_tasks == 0` collects them. A `group` that does not exist files the task
without a team (the step says so) rather than stopping the request. Notifications a workflow step sends may reach the
person it runs for (the requester), unlike a person's own change.

API: `GET /catalog`, `GET /catalog/{name}`, `POST /catalog/{name}/states`, `POST /catalog/{name}/request`
(`fields`, `description`, `requester_id`), `GET /forms/request?item=`. Web: `/catalog` (the portal: report a problem,
the catalog by category with search, and what you asked for; end users land here), `/catalog/{name}` (ordering, with
files and, for staff, who it is for), Tasks on a request, a Manager on each person in Admin → Users.

`service-catalog` (0.2.0, bundled) ships the request and task lifecycles, four items (new laptop, software access, new
starter, something else) with their workflows, the `software-approver` role and fulfilment and rejection notices. The
teams its workflows name (Procurement, Desktop Support, Service Desk, HR, Facilities) are groups to create, or to
rename in `local/`.

**The catalog editor** (Admin → Catalog, `catalog:manage`: admins, and the app's `catalog-manager` role) changes items
the way the workflow builder changes workflows (§7.7): an edit is kept in the app's studio layer as only what it changes
(`apps.CatalogItemOverride`, falling back to `override: replace` when the smallest override would not reproduce the
item), the app is reinstalled with it at once, and upgrades keep it; `servicelater app export-local` writes it as a
`local/catalog/` file. A new item goes in an app chosen when it is created. The editor sets everything an item file
can: label, category, picture, summary, description, title, position, who may order it, the questions (kinds, choices
with conditions, required and shown conditions, defaults, limits; the layouts follow the order of the questions),
checks and the workflow, with the order form previewed beside it. API: `GET /catalog-items`, `GET|PUT
/catalog-items/{name}`, `DELETE /catalog-items/{name}/edits`.

Not built yet: a cart and quantities, prices and costs as anything more than a question, item visibility by department or location (people
have neither yet), variables shared between items, and tasks under changes and incidents.

## 9. Data model and ownership

Definitions are config, not user data. They live in Postgres so they can change without a restart (§5.5 of the
MVP doc):

```
apps(name pk, version, manifest jsonb, installed_at, installed_by)
app_definitions(id, app_name fk, kind, name, version, checksum, body jsonb, current bool)
```

- Definitions are **immutable and versioned**: an update inserts a new row and moves the `current` pointer.
- Everything an app owns has `app_name`, so uninstall removes exactly its definitions and nothing else.
- Content that people edit (tickets, comments, and later articles) is **user data**, never deleted by an app
  uninstall (D10).

### 9.1 What core keeps

Core owns users, teams, the record tables, comments, auth, the job queue, the workflow engine and realtime. Apps
supply the *behaviour* around them. Concretely, `tickets.status` stops being constrained by a hardcoded `CHECK`
list and is validated against the record's lifecycle instead.

### 9.2 Record types: a shared `task` base (D11, D14)

Incident, change, request and problem each have their own table (D11), and all of them extend a shared `tasks`
table (D14). ServiceNow lets a table *extend* another (`incident` extends `task`), so shared behaviour lives once
on the parent and every child inherits it. We get the same effect in Postgres with **class-table inheritance**: one
base table for what every type has, and one small extension table per type for what only it has.

```
tasks                                     incidents                changes
  id, type, number, title,                  task_id  (pk, fk)        task_id  (pk, fk)
  description, status,                      impact, urgency,         risk, risk_score,
  state_category, priority,                 resolution,              planned_start,
  requester, assignee,                      resolution_code, ...     planned_end, ...
  assignment_group, service, ci_id,
  parent_id, sla_due, fields jsonb,
  created_at, updated_at
```

In expressions a record is flat: `record.priority` and `record.resolution` both work, whichever table holds the column.

**Built (slice 3).** The schema, API and UI now work this way, with these specifics:

- `task_types` is the registry: a type, its prefix (INC, CHG, REQ, PRB) and a label. Inserting a row creates that
  type's number sequence, so **a new type needs no code to be numbered**. `tasks.number` is assigned per type by a
  trigger, unique within `(type, number)`. A record is named by its **reference**, the prefix and number: `INC42`.
  `GET /tickets/{id}` accepts an id, a reference in any case, or a bare number, which means an incident so that
  links from before there were several types keep working.
- `incidents` is the first extension table, with `resolution` as its first typed column. Its key is a composite
  foreign key `(task_id, type) → tasks(id, type)` with `CHECK (type = 'incident')`, so an incident row can never
  hang off a change. Its RLS is "visible when its task is; only staff write". Changing it also bumps the task's
  `updated_at`, which notifies live screens.
- `changes` is the second (migration 00023, for change management): `change_type` (standard, normal, emergency),
  `risk` and `impact` (low, medium, high, or empty: not assessed), `planned_start`/`planned_end` (end after start),
  and the implementation, back-out and test plans. The API nests them as `change` on a change (absent on other
  types) and takes them nested on create and PATCH; expressions see them flat (`record.risk`,
  `has(record.planned_start)`: an unset time is a zero timestamp, so `has()` tells). Rules and workflows for
  changes may set `risk` and `impact` (`apps.FieldOwners` says which settable fields belong to which type,
  `apps.FieldValues` which values an enumerated one takes, and both are checked at install).
- Request and problem have no extension table yet. Their types **are registered** (so their prefixes are reserved
  and lifecycles can be written for them), and until they gain a table they behave as lightweight types: a `tasks`
  row, listed, commented on and moved like any other.
- **Lifecycles inherit.** A type's own lifecycle wins; else the `task` lifecycle applies; else the built-in one. An
  install checks stranded records only for the types a lifecycle governs: a `task` lifecycle governs the types
  without their own, so it is neither blocked by, nor rewrites, incidents that have theirs.
- Comments reference `tasks.id` with a real foreign key. The queue lists tasks of every type without joining any
  extension table, and `GET /tickets?type=change` narrows it. Rules on `task` run for every type, before the
  type's own (§6.4).

**Adding a record type** (a core change, by design):

1. Register it: a migration inserts into `task_types` (type, prefix, label). Its sequence is created for you.
2. If it needs typed columns, add an extension table modelled on `incidents`: primary key `task_id`, a `type` column
   with `CHECK (type = '<type>')`, the composite foreign key, RLS, and a touch trigger.
3. Read it in `GetTicketView` with one more `LEFT JOIN`, and add its columns to the ticket response and to
   `expr.Record` (and `toExprRecord`), so guards can use them as flat fields.
4. Add the type to `apps.KnownRecordTypes`. A test keeps that list in step with the registry.
5. Create its extension row in `records.createExtension`, carry its fields through `records.Ticket` (load, patch,
   `writeChanges`), and accept them in the API, as `changes` does.
6. Ship a lifecycle for it in an app: a type is *in use*, and can be filed, only when an installed app defines
   its own lifecycle (`Registry.Provides`). Decide whether anyone may file one (`task_types.anyone_may_file`) or
   only those with `records:create:<type>`.

What it gives us:

- **Real foreign keys.** Comments, attachments, audit history and SLA clocks reference `tasks.id`, so there is no
  polymorphic `(record_type, record_id)` pair without a foreign key.
- **One queue.** The queue, search, realtime and assignment work on `tasks` alone, so they list every type in one
  fast query, with no joins.
- **Inheritance for rules.** A rule, lifecycle default, form or SLA defined on `record_type: task` applies to
  incidents, changes, requests and problems alike, and a rule on `incident` applies only to incidents. Base rules
  run before type rules. A type can override the base lifecycle with its own.
- **One RLS policy.** Visibility is defined once on `tasks`. An extension table's policy is "the task is visible",
  like comments today.
- **Numbers per type.** `tasks` holds `type` and `number`, unique together, with a sequence and prefix per type
  (INC, CHG, REQ, PRB).
- **App-defined types without DDL.** An app can declare a new type that is a `tasks` row with its own lifecycle and
  form and its extra fields in `fields jsonb`, with no extension table. Core types get typed columns; custom types
  stay lightweight. Adding a typed table for a new type is still a core change in v1.
- **Extra fields** on any type live in `fields jsonb`, validated against the form definition, so apps extend
  records without DDL (D6).

What it costs:

- **A join to read a full record**, and **two inserts to create one**, in one transaction. Reading the queue does
  not join. sqlc handles the joins as ordinary typed queries.
- **Keep it shallow.** Two levels (`task` then `incident`). A third level (`major_incident`) would add another join
  and is not needed; model that as a flag or a custom type.
- **The base table must stay lean.** ServiceNow's `task` grows hundreds of columns because everything extends it.
  Our rule: a column goes on `tasks` only if it is meaningful to every type. Everything else lives in the extension
  table or in `fields`.
- **The current `tickets` table becomes `tasks` plus an `incidents` extension**, as a two-phase migration. Nothing
  is released, so this is cheap now.

Why not Postgres's built-in `INHERITS`? It does not carry primary keys, unique constraints or foreign keys across
the hierarchy, so a comment could not reference the parent table, and its interaction with row-level security is
easy to get wrong. Class-table inheritance uses only ordinary tables and constraints.

### 9.3 If an app is removed

If `incident-management` is uninstalled while tickets exist, those tickets are kept and fall back to a minimal
built-in lifecycle (new, open, closed) so nothing breaks. Uninstall also refuses while another installed app
depends on it. Running workflow executions of a removed app are cancelled and recorded as such.

### 9.4 History (built)

Every record and configuration item keeps what happened to it, field by field (`record_history`, migration 00032):
when, who (a person; an app's principal and the person it acted for; or the system, such as the SLA timer), what
(`created`, `changed`, `linked`, `unlinked`, `deleted`), which field (a column, an extra field as `fields.<name>`, or
what was linked: an item, an article, a record, a relationship) and the old and new values as stored. It is written
by triggers on `tasks`, `incidents`, `changes`, `task_cis`, `task_articles`, `cis` and `ci_relationships`, so every
way of changing them is recorded alike, and nothing else can write it: the app role may only read it, for records
and items it may see, and admins cannot change or delete an entry either.

`GET /tickets/{id}/history` and `GET /cis/{id}/history` give it oldest first (the latest 500 entries), with each
field's label and its values as people read them (names for ids, status and option labels), resolved for the reader:
an item they may not see reads as "an item". The ticket's Activity section shows comments, the history, or
everything in order (remembered per browser); an item's page has a History section. A new record type gets history
for its `tasks` columns and extra fields at once; a new extension table needs a trigger like `changes_history`.

**Choosing what is recorded** (migration 00033). The basics are always recorded: a record's creation, status,
assignee and assignment group; an item's creation, deletion and status. Beyond them:

- **Admin → History** (`history.record_types`: record types, or `*` for every type including later ones;
  `history.cmdb`), defaulting to everything. Every change to these settings is itself recorded (subject `setting`,
  shown on that page), so a gap can be told from a quiet record. Turning something off acts from then on.
- **`history: false`** on a form field or a CMDB class field leaves its changes out, set by the app or in its local
  layer like any other change. The installer keeps these in `history_exclusions` (a class's exclusion applies to the
  classes that extend it); the admin page lists them.

One function decides (`app_history_on`, called by `app_history` for every entry the triggers make), at the cost of
an indexed lookup or two per change.

Not built yet: comments edited or deleted (they are not edited today), workflow and approval events in the same
list (they are in the run history), SLA breaches, filtering by person or field, export, and a retention policy.

### 9.5 App-defined record types (built)

An app can add a kind of record of its own: a `tyler` app adds Things, numbered TYL1, TYL2 and so on. A new type is
**data, not schema**: a row in `task_types` plus records in `tasks` whose own fields live in `fields jsonb`, declared
and checked by the type's form (§8.1). No app ever creates a table, runs DDL or ships a migration (D6); typed
extension tables (`incidents`, `changes`, `requests`) stay core-only. Everything already built per type works on an app
type unchanged: numbering, references, queue and filters, lifecycles, forms, rules, workflows, SLAs, history,
attachments, notifications, permissions, the API and the UI, none of which keeps a list of types.

**The definition.** A new definition kind, `record_type`, in `types/<name>.yaml`:

```yaml
# apps/tyler/default/types/tyler_thing.yaml
kind: record_type
name: tyler_thing          # the type's id: records:read:tyler_thing, record_type: tyler_thing, ?type=tyler_thing
prefix: TYL                # references are prefix + number: TYL1
label: Thing
plural: Things             # optional; the label + "s" by default
anyone_may_file: false     # optional: true lets everyone signed in file one (like incidents); else records:create:tyler_thing
description: "Things Tyler tracks."
```

- `name`: `^[a-z][a-z0-9_]{1,30}$`, not a core type (`task`, `incident`, `change`, `request`, `problem`,
  `work_task`). Prefixing it with the app's name (`tyler_thing`) is recommended, so two apps never compete for one.
- `prefix`: `^[A-Z]{2,5}$`, unique across every type, core and app.
- One app provides each type name and each prefix (an install that would take either from another app is refused, as
  for lifecycles and roles). A type's other definitions (`lifecycles/tyler_thing.yaml`, `forms/tyler_thing.yaml`,
  rules, SLAs, workflows) may come from the same app or from apps that depend on it.
- Compiling needs no database, so a definition naming a dependency's type is recognised by the name: it must start
  with the dependency's name (`tyler_` for the `tyler` app, hyphens as underscores). Such types are kept on the compiled
  app (`ForeignTypes`) and the install checks that a dependency defines them. Any other name that is neither core nor
  the app's own is an error at compile time, so typos are still caught by `servicelater app validate`.

**What installing does.** In the install's transaction, each `record_type` upserts its `task_types` row (new columns:
`owner_app`, `plural`, `description`, `retired_at`; core rows have no owner; migration 00042). Inserting the row creates the type's
number sequence through the existing trigger, so numbering needs no new code. A type with no lifecycle anywhere uses the
`task` lifecycle, as today; `Registry.Provides` still decides whether it may be filed (an app must give it a lifecycle,
directly or through `task`). The record permissions `records:<verb>:tyler_thing` exist at once, because
`authz.RecordCatalog` and `GET /permissions` are built from `task_types`; the app's roles and manifest may name them.

`apps.CoreRecordTypes` is the fixed list of the platform's own types (task included); definitions are checked against
it plus the app's own and its dependencies' types. The registry serves the set in use: `KnownType` (core or an
installed app's), `RecordTypes` (every type records are held of) and `Exists` (also retired ones), and the API checks
type parameters against it instead of a list in code.

**References.** A reference is the type's prefix and the number, the same for every type, core or app: INC42, TYL7.
Numbers are **never zero-padded** (decided 2026-11-17): padding means guessing how many records a type will ever have,
grows past its width anyway, and is longer to read and say; a sorted list orders by number, not by the text. The
number is stored as an integer, so a fixed width would gain nothing in storage or indexing either. **The API is where
references are formatted**: every record in a response carries `reference` and `prefix` (built in the queries), and
the web UI shows them as given, styling the prefix, never rebuilding a reference from the number; a client of the API
sees exactly what people see. Looking a record up by reference is case-insensitive (`tyl7` finds TYL7).

**Fields and search.** The type's form declares its fields (text, choice, date, number...), with conditions and
validation enforced on every save, as for incidents. Rules, workflows and SLAs read them typed (`record.fields.size`).
Queue search and filters work on the columns every record has; filtering on an app field is a later addition, through
expression indexes the platform creates for fields the form marks `indexed: true` (core-run DDL on `tasks`, never
app SQL).

**Uninstalling and changing.** Removing the app keeps every record of its types (D10): the type row stays, marked
retired (`retired_at`), and is **hidden everywhere**: the queue and its type filter (`?type=` is a 400), `GET
/record-types` and so the New ticket dialog, the permissions catalog, and the records listed on configuration items
and articles; nobody may file one (`Provides` is false, and `app_anyone_may_file` says no). A record of it still opens
by its reference or id, with its history, and its type's lifecycle labels and form are still served (`Registry.Exists`)
so the page can show it. Reinstalling the app brings the type back with its numbering carried on. While records of a
type exist, its `prefix` cannot change (an install that tries is refused, naming the count); dropping a type from the
app, or renaming it (which is dropping one and adding another), retires it. `label`, `plural`,
`anyone_may_file` and `description` may change at any time.

**Not part of this:** typed columns or extension tables for app types (an app that needs heavy reporting on its fields
asks for core support), parent-child rules between app types beyond `parent_id`, a type being ordered only from the
catalog (`from_catalog` stays core), and a limit on types per instance (to be set when measured; 100 is the working
figure).

**Where it lives:** `internal/apps/recordtype.go` (the kind, `CoreRecordTypes`, `checkRecordTypes`), `checkTypeOwnership`
and `syncTaskTypes` in `install.go`, the registry's `types` and `retired`, migration 00042 (the columns). A sample app
for tests only is `backend/internal/apps/testdata/tyler` (Things, TYL1).
Tests: `internal/apps/recordtype_test.go`, `internal/api/app_record_types_test.go`, `web/e2e/record-types.spec.ts`.

### 9.7 Problems and their incidents (built)

The `problem-management` app (bundled, depends on core-itsm only) gives problems a lifecycle (New, Assess, Root cause
analysis, Fix in progress, Resolved, Closed, Cancelled; Start the fix needs the root cause, Resolve the fix too), a form
(category, root cause, workaround, **known error** as a flag that needs a workaround, fix) and a `problem-manager` role.

Core keeps the link (decided with the user 2026-11-17: **one problem per incident**, as in ServiceNow):
`incidents.problem_id` (00045; a generated `problem_type` column makes the foreign key prove the target is a problem;
no ON DELETE, so a linked problem cannot be deleted). It goes through the save path (`Patch.SetProblem`,
`CreateInput.ProblemID`, `problemFor`: incidents only, a problem the saver can see), is in history (`problem_id`,
shown as the problem's reference) and in expressions (`record.problem_id`, and `record.open_incidents` on a problem).
Linking, unlinking or a linked incident's status change notifies the problem's watchers. API: incidents carry
`problem_id`, `problem_reference`, `problem_title` (empty when the reader cannot see the problem); problems carry
`incidents` and `open_incidents`; `PATCH problem_id` (null unlinks); `POST /tickets` takes `problem_id` for a new
incident and `incidents` (at most 100) for a new problem, each linked as an edit of that incident in the same
transaction. Web: `ProblemLinks.svelte` (Problem section on incidents: link to an open problem, raise one, unlink;
Incidents section on problems).

What the link does is app content, decided with the user: **resolving a problem resolves its open incidents with a
note** (`rules/resolve-linked-incidents.yaml`: the fix as each resolution, a comment, the Resolve move) and marking a
known error comments the workaround on them (`rules/share-workaround.yaml`), both through `linked.update` (§6.4).

### 9.6 App sections in the navigation (decided, not built)

Decided with the user on 2026-11-17 from three mockups (accordion, icon rail with a panel, app switcher): **the
accordion**. Not built: build it only when asked. Two things are wanted, on top of row-level security: custom apps
must not be bundled in with the out-of-the-box ones, and each app is its own section in the navigation, as in
ServiceNow, without that product's clutter.

**The navigation.**

```
Search                      Ctrl K
+ New incident                         <- follows the section (below)

My work                          7     <- everything assigned to me, across apps
Approvals                        2

APPS
v Incidents                            <- the section you are in is open
    Mine                         3
    My groups                    6
    Unassigned                   4
    Due                          3
    Open                        23
    All
> Changes                              <- includes the change calendar
> Requests                             <- includes the catalog
> Knowledge
> CMDB
> Things                               <- a custom app, only for those who hold its role

PLATFORM
  Workflows, Runs, Admin
```

- **One line per app, one level deep.** Opening a section closes the one that was open. Its views are the queue's
  views (Mine, My groups, Unassigned, Due, Open, All) with their counts, limited to that app's record types.
- **Only apps you can use.** A section shows only to someone who holds a permission on that app's records, so most
  end users see nothing new.
- **Each out-of-the-box app is its own section** (Incidents, Changes, Requests, Knowledge, CMDB), by the same rule as
  custom apps, not one "Service Desk" section. `core-itsm` is plumbing and has none.
- **Sections are automatic.** An app that defines a record type (or, for a core type, provides its lifecycle) gets a
  section named by the type's plural; no menu to declare. The manifest may set a label, an order and an icon, and an
  operator may override them in the local layer. App-declared menu entries (saved filters, links) are a later addition.
- **"My work" stays cross-app** at the top, with Approvals: someone who works incidents and changes sees what is
  theirs in one place. Everything else is per app.
- **Platform tools are not apps**: Workflows, Runs and Admin sit in their own group at the bottom.
- Collapsed to icons, the navigation shows one icon per app. If an instance grows many apps, pinning and hiding apps
  per person is the follow-up (the rail-and-panel variant was the alternative for that case).

**The New button follows the section**, and the `c` shortcut does the same:

| Where | Button | Does |
|---|---|---|
| Incidents, Changes, a custom app | New incident, New change, New thing (the type's label) | The new-record dialog, starting on that type; the type can still be changed in it |
| Requests | Request something | Opens the catalog |
| Knowledge | New article | Opens the article editor |
| CMDB | New item | Opens the new item page |
| My work, Approvals, platform tools | New ticket | The dialog with its type choice, as today |

Someone who may read a section but not create in it gets New ticket with the types they may file, or no button when
there are none. An app with several record types starts on its first; the dialog offers the others.

**Keeping custom apps apart (visibility).** Today the built-in agent role holds `records:<verb>:*`, so a custom
app's records show to every agent the moment it is installed. Decided: **a custom app's record types are private by
default.** The wildcard covers the out-of-the-box types; an app's own types are seen only by people granted that
app's role (and admins). A type may opt out in its `record_type` definition when every agent should see it. This is
the per-type layer; group-scoped visibility within a type (records limited to the teams they are routed to) is the
second layer and a separate piece of work.

**Open: how an app chooses its icon.** The user wants to think about this before it is designed. The requirements:
apps must not all share one generic icon, and an app must not be able to ship an ugly one or a photograph shrunk to
icon size. Nothing is decided. Options to weigh:

1. *A curated set, chosen by name* (as catalog items do with `apps.CatalogIcons`): the app writes `icon: box`. Always
   consistent, nothing loaded from an app, but an app may not find one that fits; the set would have to be large.
2. *The app ships an SVG that the platform checks strictly*: one colour (drawn with the text colour), a fixed grid and
   stroke, a size and complexity limit, no raster images, scripts or external references. Distinct icons in one style,
   at the cost of a validator and of trusting authors to draw to the grid.
3. *A generated mark*: the app's initials on a tint derived from its name (what `Avatar` does for people), with an
   optional colour from a fixed palette. Never ugly and needs nothing from the author, but less recognisable than a
   drawn icon.
4. *A mix*: the curated set first, a generated mark when an app names none, and a checked SVG only for apps that need
   their own.

**Work when it is built:** the section list from the registry (apps, their types, labels, order, icon) served with
what the caller may use; per-section queue filtering by type and per-section counts; the accordion and the New button
in the web navigation; routes per section; the private-by-default rule in `app_can_record` and the Go checks, with a
migration of the built-in agent's grants; tests that an agent without the app's role sees neither the section nor its
records. Mockups: the accordion variant shown to the user on 2026-11-17.

## 10. Install, update, uninstall

**Install** (`servicelater app install ./my-app` or `my-app.swapp`):

1. **Validate** everything before writing: manifest schema, dependency resolution, YAML schemas, every
   expression type-checked, every workflow graph checked (reachable steps, no dangling `next`, acyclic, step
   parameters valid), and the declared permissions compared to what the definitions use.
2. **Show the plan**: definitions added, changed and removed, plus the permissions the app requests.
3. **Apply in one transaction**: write new definition versions, move `current` pointers, record the app. Any
   failure rolls back the whole install.
4. `NOTIFY app_changes` so every instance drops its cached copy and reloads on next use. No restart.

**Sync** (`servicelater app sync`, and automatically at startup) installs or upgrades the bundled apps with the instance
local directory applied (§4.5). Each app is handled on its own: a problem in one is reported and leaves that app as it
was, so a failed upgrade or a bad local file never stops the instance.

**A batch is one transaction.** `app install a b` installs both or neither, in dependency order, and `-dry-run` plans
the whole batch as if the earlier apps were installed.

**Update** diffs by checksum and writes new versions only for what changed. Removing a lifecycle state that
tickets currently occupy is **rejected** unless the update maps it to another state (`renamed_from` or an
explicit `migrate:` block), so no ticket is ever left in a state that no longer exists.

**Uninstall** removes the app's definitions and rules and stops its jobs. Config values and run history are kept
for a retention period.

**Other commands**: `app validate` (used in CI, no database needed), `app pack`, `app list`, and `app dev` (reload on
save, not built yet). `app install` takes several paths and installs dependencies first.

## 11. Security model

- Apps are installed by admins and are **trusted, but constrained**: the declared permissions limit what an app
  may touch, and every action is one of a fixed set of Go implementations.
- **Identity.** Before and after rules run as the *acting user*, under their RLS, so a rule can never see or write
  more than the person saving could. Async rules and workflows run as the app's **service principal**; a sync rule
  can opt in with `run_as: app` if it needs more, and that shows in the install plan.
- The service principal's permissions are exactly those in the manifest, approved at install, and enforced by both
  Go middleware and Postgres RLS. Writes are audited as "by app X on behalf of Y".
- **Creating and deleting records** needs explicit `create` and `delete` permissions per record type, shown in the
  install plan. Deletions are audited with a tombstone and stay visible in `action_runs`.
- **Outbound calls** (`http.request`, `webhook`) may only reach hosts declared in the manifest, with timeouts and
  response-size caps.
- **Secrets** are typed config values, stored encrypted (pgcrypto), never returned to clients, and available to
  steps only through `config`.
- **Loop protection**: events caused by an app's own writes carry a causation chain. A rule cannot re-trigger on
  its own changes, and chains are capped at a fixed depth.
- **Workflow limits**: maximum steps per execution, maximum `foreach` iterations, maximum execution age, and a
  concurrency cap per app, so one app cannot starve the queue.
- **Idempotency and retries**: jobs retry with backoff, so steps carry idempotency keys and built-in actions are
  safe to repeat.

### 11.1 What is built (slice 7)

- **Permissions under roles.** Code checks named permissions (`internal/authz`, `app_can` in SQL), never role names.
  A role is a set of them. A person has one base role (admin, agent, end_user) plus any roles granted to them or to
  a group they are in. Apps ship roles in `roles/*.yaml`:

  ```yaml
  kind: role
  name: cab-member
  label: CAB member
  permissions: [records:read:change, comments:internal, workflows:read_all]
  ```

  Admins grant them from the admin section. Role names are global, so two apps cannot define the same one.
  Escalation guards: changing who is in a group that holds a role, and making someone an admin, both need
  `roles:manage`.
- **Service principals.** Each installed app is an identity of its own (a `users` row with `app_name`, which cannot
  sign in). Its permissions are its manifest's `permissions:`, shown in the install plan; new ones are refused
  unless approved (`servicelater app install -approve-permissions`). Async rules and workflows run as it, on behalf
  of the person who set them off; an after rule can opt in with `run_as: app` (before rules cannot yet). What it
  writes records whom it acted for, and expressions still see that person as `user`.
- **`role('name')`** in workflow expressions resolves to everyone holding the role, like `group('name')`, so an
  approval step can ask `approvers: "role('cab-member')"`.
- **Record permissions are scoped by type** (migration 00022): `records:<verb>:<type>`, with `*` for every type
  (`records:update:*`). The verbs are fixed: `read` (see every record of the type, not only your own), `create`
  (file one), `update` (edit and move them, be assigned one) and `delete`. A requester always sees their own
  records, whatever the type. Filing for someone else needs `create` and `update` on the type. A type can be open
  to everyone (`task_types.anyone_may_file`, true for incidents), so filing one needs no permission.

  *Why by type and not by app:* a permission follows the data it protects. One app may own several types and
  several apps may touch one type, and app names are packaging that can be split or renamed. Adding a module adds
  a type and so catalog entries (generated from `task_types`), never a new check: Go asks
  `Set.CanRecord(verb, type)`, SQL `app_can_record(verb, type)`, the hub routes events by type, and the web asks
  `canRecord(user, verb, type)`. Abilities that are not one of these verbs (raising an emergency change, say)
  will be app-namespaced names (`change-management:emergency`) declared in the manifest; not built yet. Who
  approves is not a permission at all: it is the approval step's `approvers`.
- Not yet: app-namespaced permissions, a role editor in the UI (roles are YAML), `run_as: app` for before rules,
  and the outbound capabilities (`notify:email`, `http:`) that manifests may already declare.

### 11.2 Settings and secrets (built)

**Settings** (`settings`, migration 00029, `internal/settings`) are instance configuration admins change at run time:
the names, types, defaults and checks are Go (`settings.Email`), the table only stores values, and `settings:manage`
(admins) reads and writes them. Email is the first group: `email.enabled`, `host`, `port`, `tls` (starttls, tls, or
none only to localhost), `username`, `from_address`, `from_name`, `base_url`, `approvals`. API `GET|PUT
/settings/email`, `POST /settings/email/test`, `GET /settings/email/deliveries`; web Admin → Email. App-defined
config (the manifest's `config`) will use the same table.

**Secrets** (`secrets`, `internal/secrets`) are encrypted in Go before they reach the database: AES-256-GCM, a random
nonce per value, and the secret's name as additional data, so a ciphertext moved to another secret's row does not
decrypt. The key is never stored: it comes from `SL_SECRETS_KEY` (32 random bytes, base64; `servicelater secrets
gen-key`) or a file named by `SL_SECRETS_KEY_FILE`, and every api and worker server needs the same one. Each value
records its key's id (a hash prefix), so rotation is: set the new key, list the old one in `SL_SECRETS_OLD_KEYS` (or
`SL_SECRETS_OLD_KEYS_FILE`), run `servicelater secrets rotate`, restart, drop the old key. Without a key the server runs
and refuses to store a secret rather than storing it in the clear; a secret under a key it lacks is reported, never
treated as empty.

The app role has no access to `secrets` at all (RLS on, no policies, privileges revoked, like the credential tables):
admins set or clear one through SECURITY DEFINER `app_set_secret` / `app_clear_secret` (which check `settings:manage`)
and see only whether it is set, when and by whom (`app_secret_status`); only the system (the worker, and the api for a
test email) reads the ciphertext, decrypting it just before use. There is no API or CLI that returns a secret's value.
A key service (KMS, Vault) is not supported; the keyring is the seam where one would go.

## 12. Performance notes

- Expressions are compiled once per definition version and cached; the cache key is the definition checksum, so a
  reload never serves stale logic.
- A save evaluates each matching rule's expression filter first (microseconds) and applies actions only for rules
  that match. The sync budget (§6.3) caps the worst case at 250 ms per save.
- Sync rules add latency and hold a row lock, so the benchmark in slice 2 measures a save with zero, one and five
  rules, and a failing budget test blocks a release.
- Workflows never run in the request path, so they cannot affect API latency; they scale with the `worker` role.
- Hot reload via `NOTIFY` reaches every replica at once, with no coordination.

## 13. Open questions

1. ~~App-declared record types.~~ **Resolved by D14 and D17 (§9.5, built).** An app-defined type is a
   `task_types` row (its own prefix and numbering) plus `fields jsonb`. Apps declaring their own typed tables
   (runtime DDL, auto-applied RLS, queries alongside sqlc) stay deferred.
2. ~~Polymorphic comments.~~ **Resolved by D14.** Comments reference `tasks.id` with a real foreign key.
3. ~~Service principal and RLS.~~ **Resolved (§11.1).** One permission model for people and apps: RLS checks
   permissions, people get them from roles, app principals from their manifest.
4. **Lookups.** Resolved by the worked examples for v1: the `count` menu in §5 is enough. Revisit when real apps
   report a blocking rule it cannot express.
5. **Step library.** The worked examples exercised `notify.email`, `wait`, `condition`, `approval`,
   `record.update`, `record.query`, `foreach`, `subflow`, `comment.add` and `http.request`. `parallel`/`join`,
   `webhook`, `notify.sms`, `record.get` and `record.create` were not needed and can follow. Still open: how steps
   are versioned and deprecated across releases so an installed app keeps working.
6. **When to add scripting.** Proposed trigger: the same missing capability is requested repeatedly and cannot be
   met by a new step. The first form would be an async-only `script` step.
7. **Seed content vs user edits.** When an app ships knowledge articles or catalog items and a person edits one,
   does uninstall remove it? Proposal: app-shipped rows carry `owner_app`, and editing detaches them.
8. **Sync rule ordering.** Several before rules on one record run in a defined order (app dependency order, then
   name). Do we need an explicit `order:` field?
9. **Config and secrets UI.** Where admins enter per-app settings.
10. **Localisation.** Labels and verbs in definitions are single-language for now.
11. **Reference data.** Runtime-editable data such as freeze windows and business calendars cannot be app-defined
    tables in v1. Freeze windows are now a core table (`freeze_windows`, not a record type: they have no lifecycle,
    number or comments), managed with `freezes:manage` and counted by lookups (§5); what a freeze stops is app content.
    Business calendars are the same (`business_calendars`, `calendars:manage`, §6.6), and so are configuration items
    (`cis`, `cmdb:manage`, §8.2), whose classes and relationship types are app definitions.
12. **SLAs.** Built as a definition kind (§6.6). Still open: reporting, reset conditions and pause reasons.
13. **Comment events.** Rules on comments ("a reply reopens the ticket") need `record_type: comment` and access to
    the parent record.
14. ~~Bundled first-party apps.~~ **Resolved (D16).** They are embedded and installed at startup; local changes live
    outside the binary (§4.5).
15. **Signing and marketplace.** Out of scope for v1; worth deciding the archive format so signatures can be added.
16. **Editing definitions in the UI.** Partly resolved: the studio layer (§4.5) is the local layer kept in the
    database, and the workflow builder writes it. A form editor, and editors for lifecycle labels or a notification's
    wording, can write the same layer the same way.

## 14. Roadmap in small slices

Each slice is shippable and leaves the product working.

| Slice | Delivers | Needs first |
|---|---|---|
| **1. Skeleton and lifecycle** (done) | Manifest and validator; `apps` and `app_definitions` tables; `app install / uninstall / validate / pack`; the ticket lifecycle moves from Go into `incident-management`; the API serves it and the UI reads labels, verbs and categories from it; hot reload. | This doc agreed |
| **2. Expressions and a benchmark** (done) | CEL in transition guards, with install-time type checking and fail-closed evaluation; benchmark of the save path. Rule filters use the same engine when rules arrive in slice 5. | Slice 1 |
| **3. Record types** (done) | The `tasks` base and the `incidents` extension table (D14); the `task_types` registry with per-type numbering and references (INC42); comments referencing `tasks.id`; lifecycle inheritance from `task`; incident resolution notes. Extension tables for change, request and problem come with their apps, and rule inheritance with rules in slice 5. | Slice 1 |
| **4. Forms** (done, §8.1) | Field definitions, `fields jsonb`, form rendering. | Slices 2 and 3 |
| **5. Rules** (done: job queue and worker, before/after/async rules, `action_runs` audit, one shared save path, `record.update` and loop protection) | Before, after and async rules with the built-in actions; job queue and worker role; transactional outbox; `action_runs` audit; loop protection. | Slices 2 and 3 |
| **6. Workflow engine** (done: durable engine, `set` / `condition` / `wait` / `record.update` / `comment.add` / `end`, rule and manual triggers, run history API and viewer, load harness; the rest of the step library follows, see §7.6) | Durable execution, the v1 step library, workflows as YAML, run history API and a read-only run viewer. | Slice 5 |
| **6b. SLAs** (done, §6.6) | First-class SLA definitions with pause and resume, business calendars and breach actions. | Slice 6 |
| **7. Roles and permissions** (done, §11.1) | Custom RBAC and app service principals (open question 3). | Slice 5 |
| **8. Content kinds** | Catalog (done, §8.4), knowledge, notification templates (in-app done, §6.5), declarative list views and dashboards. | Slice 4 |
| **8d. Service catalog** (done, §8.4) | Catalog items as app definitions with questions and audiences, requests and their tasks, `task.create`, managers, the self-service portal, and an item editor writing the studio layer. | Slices 4, 6, 7 and 9 |
| **8c. Knowledge base** (done, §8.3) | Articles with review and publishing, versions, audiences, full-text search, suggestions while filing, links to records. | Slice 7 |
| **8b. CMDB** (done, §8.2) | Item classes and relationship types as app definitions, items and relationships as core data, impact, records linked to items, rules reading them (routing by support group), freezes scoped to items, approvals by owner, CSV import and export. Next: sync from other systems. | Slices 4 and 7 |
| **9. Visual builder** (done, §7.7) | The Svelte Flow builder editing the same workflow definitions, saved in the database-kept studio layer, with live highlighting of where runs are. | Slice 6 |

| **10. App-defined record types** (done, §9.5) | `record_type` definitions: an app's own types with their prefix and numbering, fields in `fields jsonb`, retired (not deleted) on uninstall. | Slices 3 and 4 |
| **11. App sections** (decided, §9.6, not built) | Each app as its own section in the navigation (accordion), a New button that follows the section, and custom apps' records private to their role holders. How an app picks its icon is still open. | Slice 10 |

Later: script steps (Starlark, Lua or WASM), app-defined tables and migrations, custom UI components, signing and
a marketplace.

## 15. Changes to the MVP doc

When this is agreed, update `ServiceLater-MVP.md`:
- §11.1: remove `scripts/`; `ui/` is declarative; `migrations/` is reserved; add `lifecycles/`, `rules/`,
  `notifications/` and `roles/`.
- §11.2: add `apiVersion`, `permissions` and `config` to the manifest.
- §4.4 and §11: workflows are interpreted by a durable Go engine using CEL and a fixed step library; the builder
  edits the same definition files (slices 6 and 9).
- §7 and §5: incident, change, request and problem are separate tables extending a shared `tasks` base (D11, D14).
