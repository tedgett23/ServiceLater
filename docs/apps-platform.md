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
| D6 | v1 apps are **data only**: no app-defined tables or SQL migrations. Apps extend records through validated custom fields (`fields jsonb`). | Proposed |
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

**Where the local layer lives.** Three layers are merged, later ones winning:

1. `default/`: the shipped layer. Always present.
2. `local/` inside the app folder: for an app installed from a folder, so a developer or third party can keep their
   changes beside it. It is never packed into a `.swapp`.
3. The **instance local directory**, `SL_APPS_LOCAL_DIR/<app-name>/`, laid out like an app's `local/` folder. This
   is where changes to the embedded first-party apps go, because the binary cannot hold them. It belongs on a
   volume, so replacing the image or the binary leaves it alone.

**How they combine.** A local file at the same path as a shipped file is merged into it, not swapped for it:

- **Maps merge key by key**, recursively. A local file can change one label and say nothing else, so any change the
  upgrade makes to everything else still arrives.
- **Lists and scalars are replaced whole.** A local `transitions:` list replaces the shipped one.
- **`null` deletes a key**, which is how a local file removes a state.
- **New keys are added** after the shipped ones, so ordering stays stable.
- **A local file with no shipped counterpart adds a new definition**, and must be complete on its own.

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
| Forms | `form` (current field values), `user` |

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
- **Loop protection.** An update raises `update` events, which fire async rules, whose updates raise events. Two
  things cut that. Every queued run carries its **cause**: the chain of `app/rule` runs behind it and its depth. What a
  run saves is judged against it (`ruleruns.Cause.Blocks`): a rule may not be set off by a change made by **its own
  app** (an app's rules do not trigger each other, so two rules of one app cannot ping-pong), and a chain is cut at
  **depth 5** however many apps it crosses. Rules of different apps may set each other off. A blocked rule is not
  queued and is recorded as `skipped` with the reason, so a cut loop leaves a trace. A person's own save has no cause.
  Before and after rules cannot loop (they write nothing that raises events).
- **Layers:** a rule is a definition like any other, so `local/` can change one field of a shipped rule (`order`,
  `on_error`, an action) without copying it.

### 6.5 Notifications (built: in-app)

Who hears about what is app content, not code. It is split in two, so the wording and the policy change separately:

- **What it says** is a `notification` definition in `notifications/*.yaml`: a `title`, an optional `body`, and
  `channels` (`in_app` only; `email`, `sms` and `slack` are rejected as not supported yet). Placeholders come from a
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

**Still hard-coded, and must move to templates or config:**

- The `approval_pending` notification (title "Something needs your approval", link `/approvals`) is created by a
  database trigger and worded in Go (`internal/api/notifications.go`). It should become a template that the approval
  step names, so an app can word it and decide who else hears about it.
- Opening a ticket marks every notification about it read. Whether a kind clears on view, on action, or only when
  dismissed should be part of the template.
- The placeholder list and the fixed recipient kinds (`assignee`, `requester`, `assignment_group`) are code. Adding
  one is a platform change, not an app change.
- Per-person preferences (mute a kind, choose a channel) do not exist yet; they belong in config, not in each rule.

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

## 8. Declarative UI

Apps describe what to show; the core Svelte UI renders it (D4).

- **Forms**: fields (type, label, required, options), layout, and expression-driven show/hide and validation.
- **List views**: columns, default filters, and sort for a record type.
- **Dashboards**: tiles bound to saved queries.

There is no way for an app to ship executable UI code in v1. If we later want custom components, they would be
prebuilt web components loaded into named slots, but that is deliberately deferred.

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

1. ~~App-declared record types.~~ **Resolved by D14.** An app-defined type is a `tasks` row plus `fields jsonb`.
   Apps declaring their own typed tables (runtime DDL, auto-applied RLS, queries alongside sqlc) stay deferred.
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
    Business calendars are the same (`business_calendars`, `calendars:manage`, §6.6).
12. **SLAs.** Built as a definition kind (§6.6). Still open: reporting, reset conditions and pause reasons.
13. **Comment events.** Rules on comments ("a reply reopens the ticket") need `record_type: comment` and access to
    the parent record.
14. ~~Bundled first-party apps.~~ **Resolved (D16).** They are embedded and installed at startup; local changes live
    outside the binary (§4.5).
15. **Signing and marketplace.** Out of scope for v1; worth deciding the archive format so signatures can be added.

## 14. Roadmap in small slices

Each slice is shippable and leaves the product working.

| Slice | Delivers | Needs first |
|---|---|---|
| **1. Skeleton and lifecycle** (done) | Manifest and validator; `apps` and `app_definitions` tables; `app install / uninstall / validate / pack`; the ticket lifecycle moves from Go into `incident-management`; the API serves it and the UI reads labels, verbs and categories from it; hot reload. | This doc agreed |
| **2. Expressions and a benchmark** (done) | CEL in transition guards, with install-time type checking and fail-closed evaluation; benchmark of the save path. Rule filters use the same engine when rules arrive in slice 5. | Slice 1 |
| **3. Record types** (done) | The `tasks` base and the `incidents` extension table (D14); the `task_types` registry with per-type numbering and references (INC42); comments referencing `tasks.id`; lifecycle inheritance from `task`; incident resolution notes. Extension tables for change, request and problem come with their apps, and rule inheritance with rules in slice 5. | Slice 1 |
| **4. Forms** | Field definitions, `fields jsonb`, form rendering. | Slices 2 and 3 |
| **5. Rules** (done: job queue and worker, before/after/async rules, `action_runs` audit, one shared save path, `record.update` and loop protection) | Before, after and async rules with the built-in actions; job queue and worker role; transactional outbox; `action_runs` audit; loop protection. | Slices 2 and 3 |
| **6. Workflow engine** (done: durable engine, `set` / `condition` / `wait` / `record.update` / `comment.add` / `end`, rule and manual triggers, run history API and viewer, load harness; the rest of the step library follows, see §7.6) | Durable execution, the v1 step library, workflows as YAML, run history API and a read-only run viewer. | Slice 5 |
| **6b. SLAs** (done, §6.6) | First-class SLA definitions with pause and resume, business calendars and breach actions. | Slice 6 |
| **7. Roles and permissions** (done, §11.1) | Custom RBAC and app service principals (open question 3). | Slice 5 |
| **8. Content kinds** | Catalog, knowledge, notification templates (in-app done, §6.5), declarative list views and dashboards. | Slice 4 |
| **9. Visual builder** | The Svelte Flow builder editing the same workflow files, with live execution highlighting. | Slice 6 |

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
