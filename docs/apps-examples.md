# Worked examples: incident and change management

Status: draft. These are the definitions the first-party `incident-management` and `change-management` apps would
ship, written against `apps-platform.md`. They exist to test the design: writing real rules and workflows shows
which lookups, expression functions and workflow steps we actually need. §5 lists what the exercise found.

Conventions are those of the design doc: a value is a literal unless it starts with `=`, `when` and `until` are
always CEL, and before/after rules run in the save transaction while async rules and workflows run in workers.

## 0. Assumed record fields

Columns these examples rely on. Most do not exist in the schema yet; they are what the record-types slice adds. Under D14 they are spread across the shared `tasks` table (title, status, state_category, priority, requester, assignee, assignment_group, service, ci_id, parent_id, sla_due, timestamps) and each type's extension table, but expressions see one flat `record`. Built so far: `tasks` (with `type`, `number`, `reference`) and `incidents` (`resolution`); the rest arrive with their slices.

| Type | Fields |
|---|---|
| **incident** | `number`, `title`, `description`, `status`, `state_category`, `priority` (1-4), `impact` (1-3), `urgency` (1-3), `category`, `service`, `ci_id`, `requester`, `assignee`, `assignment_group`, `parent_id`, `problem_id`, `on_hold_reason`, `resolution`, `resolution_code`, `sla_due`, `created_at`, `updated_at` |
| **change** | `number`, `title`, `type` (standard, normal, emergency), `status`, `state_category`, `risk` (low, medium, high), `risk_score`, `cab_required`, `ci_id`, `service`, `requester`, `assignee`, `planned_start`, `planned_end`, `implementation_plan`, `backout_plan`, `test_plan`, `created_at` |
| **problem** | `number`, `title`, `description`, `status`, `state_category`, `service`, `assignment_group`, `root_cause`, `created_at` |
| **freeze_window** | `name`, `starts_at`, `ends_at` |

`state_category` is maintained by core from the lifecycle (open, waiting, done, closed) and indexed, so lookups and
the queue's "Open" filter never need to know status names. References such as `record.requester` and
`record.assignee` expand to a user with `name`, `email` and `manager`.

---

## 1. Incident management

### 1.1 Lifecycle

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
  - { from: new,         to: in_progress, verb: Start work }
  - { from: in_progress, to: on_hold,     verb: Put on hold
      when: 'size(record.on_hold_reason) > 0',
      error: Say why the incident is on hold. }
  - { from: on_hold,     to: in_progress, verb: Resume }
  - { from: in_progress, to: resolved,    verb: Resolve
      when: 'size(record.resolution) > 0 && size(record.resolution_code) > 0',
      error: Add resolution notes and a resolution code before resolving. }
  - { from: on_hold,     to: resolved,    verb: Resolve
      when: 'size(record.resolution) > 0 && size(record.resolution_code) > 0',
      error: Add resolution notes and a resolution code before resolving. }
  - { from: resolved,    to: in_progress, verb: Reopen }
  - { from: resolved,    to: closed,      verb: Close }
```

Principle used below: a check that belongs to one transition and needs only the record is a **guard** in the
lifecycle. A **rule** is for anything that needs lookups, spans several transitions, or has side effects.

### 1.2 Before rules

**I1. Priority from impact and urgency** (pure expression)

```yaml
kind: rule
name: priority-from-matrix
record_type: incident
timing: before
on: [create, update]
when: 'record.impact > 0 && record.urgency > 0 && (event == "create" || record.impact != old.impact || record.urgency != old.urgency)'
run:
  - action: set
    field: priority
    value: =[[1, 2, 3], [2, 3, 4], [3, 4, 4]][record.impact - 1][record.urgency - 1]
```

**I2. Resolution target from priority**

```yaml
kind: rule
name: set-sla-due
record_type: incident
timing: before
on: [create, update]
when: 'event == "create" || record.priority != old.priority'
run:
  - action: set
    field: sla_due
    value: =now + duration(record.priority == 1 ? "4h" : record.priority == 2 ? "8h" : record.priority == 3 ? "72h" : "120h")
```

**I3. Critical incidents must name the affected service** (pure expression)

```yaml
kind: rule
name: critical-needs-service
record_type: incident
timing: before
on: [create, update]
when: 'record.priority == 1 && size(record.service) == 0'
run:
  - action: abort
    message: Critical incidents must name the affected service.
```

**I4. No duplicate tickets** (lookup: `count` with a range filter)

```yaml
kind: rule
name: reject-duplicates
record_type: incident
timing: before
on: [create]
lookups:
  recent_duplicates:
    count: incident
    where:
      requester: =record.requester.id
      title: =record.title
      state_category: open
      created_at: { gte: =now - duration("1h") }
when: 'lookups.recent_duplicates > 0'
run:
  - action: abort
    message: You already reported this in the last hour. Add a comment to your existing ticket instead.
```

**I5. A major incident cannot be resolved while children are open** (lookup: `count`)

```yaml
kind: rule
name: children-must-be-done
record_type: incident
timing: before
on: [update]
lookups:
  open_children:
    count: incident
    where: { parent_id: =record.id, state_category: open }
when: 'record.status == "resolved" && old.status != "resolved" && lookups.open_children > 0'
run:
  - action: abort
    message: Resolve the child incidents first, or use "Resolve with children".
```

**I10. An incident with children cannot be deleted** (before, on `delete`; `record` is the row being deleted)

```yaml
kind: rule
name: no-delete-with-children
record_type: incident
timing: before
on: [delete]
lookups:
  children:
    count: incident
    where: { parent_id: =record.id }
when: 'lookups.children > 0'
run:
  - action: abort
    message: This incident has child incidents. Unlink or resolve them before deleting it.
```

### 1.3 After and async rules

**I6. Start escalation for critical and high incidents** (after: queues a workflow in the transaction)

```yaml
kind: rule
name: escalate-urgent
record_type: incident
timing: after
on: [create]
when: 'record.priority <= 2'
run:
  - action: workflow.start
    workflow: critical-escalation
```

**I7. When an incident is resolved, follow up and cascade** (after)

```yaml
kind: rule
name: on-resolved
record_type: incident
timing: after
on: [update]
when: 'old.status != "resolved" && record.status == "resolved"'
run:
  - action: workflow.start
    workflow: close-after-silence
    with:
      record: =record
      quiet_for: =duration("72h")
      from_status: resolved
      to_status: closed
      template: resolution-survey
  - action: workflow.start
    workflow: cascade-to-children
    when: '!has(record.parent_id)'      # per-action condition: only parents cascade
```

**I8. Tell the new group when an incident is reassigned** (async)

```yaml
kind: rule
name: notify-new-group
record_type: incident
timing: async
on: [update]
when: 'event != "create" && record.assignment_group != old.assignment_group'
run:
  - action: notify.email
    to: =record.assignment_group.email
    template: incident-reassigned
```

**I11. Watch for recurring incidents on a service** (async; hands off to a workflow)

```yaml
kind: rule
name: watch-for-recurring-incidents
record_type: incident
timing: async
on: [create]
when: 'size(record.service) > 0 && !has(record.problem_id)'
run:
  - action: workflow.start
    workflow: raise-problem
```

**I9. A reply from the requester reopens a resolved incident** (after, on a comment)

```yaml
kind: rule
name: reply-reopens
record_type: comment                      # comments are events too (see finding F4)
timing: after
on: [create]
when: 'record.parent.type == "incident" && record.parent.status == "resolved" && record.author == record.parent.requester && !record.internal'
run:
  - action: record.update
    target: =record.parent
    set: { status: in_progress }
  - action: notify.email
    to: =record.parent.assignee.email
    template: requester-replied
```

### 1.4 Workflows

**W1. Critical escalation**: acknowledge within 15 minutes, or widen the circle.

```yaml
kind: workflow
name: critical-escalation
record_type: incident
start: notify_owner
steps:
  notify_owner:
    type: notify.email
    to: =has(record.assignee) ? record.assignee.email : record.assignment_group.email
    template: critical-ticket
    next: wait_ack
  wait_ack:
    type: wait
    until: 'record.status != "new"'
    timeout: 15m
    on_timeout: page_manager
    next: done
  page_manager:
    type: notify.email
    to: =record.assignee.manager.email
    template: critical-unacknowledged
    next: post_to_channel
  post_to_channel:
    type: http.request
    method: POST
    url: =config.slack_webhook
    body: ={"text": "INC" + string(record.number) + " (P" + string(record.priority) + ") is unacknowledged after 15 minutes"}
    next: wait_second
  wait_second:
    type: wait
    until: 'record.status != "new"'
    timeout: 30m
    on_timeout: raise_priority_note
    next: done
  raise_priority_note:
    type: comment.add
    body: No acknowledgement after 45 minutes. Flagged for the duty manager.
    internal: true
    next: done
  done:
    type: end
```

**W2. Close after silence** (a reusable subflow, shipped in `core-itsm`; used by incidents and by change reviews)

```yaml
kind: workflow
name: close-after-silence
inputs: [record, quiet_for, from_status, to_status, template]
start: wait_for_reply
steps:
  wait_for_reply:
    type: wait
    until: 'record.status != inputs.from_status'      # someone reopened or moved it
    timeout: =inputs.quiet_for
    on_timeout: ask
    next: done
  ask:
    type: notify.email
    to: =record.requester.email
    template: =inputs.template
    next: wait_more
  wait_more:
    type: wait
    until: 'record.status != inputs.from_status'
    timeout: 96h
    on_timeout: close
    next: done
  close:
    type: record.update
    target: =record
    set: { status: =inputs.to_status }
    next: note
  note:
    type: comment.add
    body: Closed automatically because there was no reply.
    next: done
  done:
    type: end
```

**W3. Cascade a parent's resolution to its children** (`record.query`, `foreach`, and a subflow)

```yaml
kind: workflow
name: cascade-to-children
record_type: incident
start: find
steps:
  find:
    type: record.query
    record_type: incident
    where: { parent_id: =record.id, state_category: open }
    limit: 200
    next: each
  each:
    type: foreach
    items: =steps.find.output.items
    max: 200
    run: resolve-child
    with: { child: =item, parent: =record }
    next: done
  done:
    type: end
---
kind: workflow
name: resolve-child
inputs: [child, parent]
start: update
steps:
  update:
    type: record.update
    target: =inputs.child
    set:
      status: resolved
      resolution: ="Resolved with parent INC" + string(inputs.parent.number)
      resolution_code: parent_resolved
    next: done
  done:
    type: end
```

**W4. SLA warning at 80%** (hand-rolled; does not pause while on hold, see finding F7)

```yaml
kind: workflow
name: sla-warning
record_type: incident
start: wait_80
steps:
  wait_80:
    type: wait
    until_time: =record.created_at + scale(record.sla_due - record.created_at, 0.8)
    next: still_open
  still_open:
    type: condition
    branches:
      - when: 'record.state_category == "open"'
        next: warn
    else: done
  warn:
    type: notify.email
    to: =record.assignee.email
    template: sla-warning
    next: done
  done:
    type: end
```

**W7. Raise a problem for recurring incidents** (`record.query`, `set`, `record.create`, `foreach`)

Three or more unlinked incidents on one service in a week become a problem record, and the incidents are linked to it.
If an open problem already exists for the service, the incidents join that one instead of creating a duplicate.

```yaml
kind: workflow
name: raise-problem
record_type: incident
start: recent
steps:
  recent:
    type: record.query
    record_type: incident
    where:
      service: =record.service
      created_at: { gte: =now - duration("168h") }
      problem_id: null                       # not yet linked to a problem
    order_by: created_at desc
    limit: 50
    next: enough
  enough:
    type: condition
    branches:
      - when: 'steps.recent.output.count >= 3'
        next: existing
    else: done
  existing:
    type: record.query
    record_type: problem
    where: { service: =record.service, state_category: open }
    limit: 1
    next: have_problem
  have_problem:
    type: condition
    branches:
      - when: 'steps.existing.output.count > 0'
        next: use_existing
    else: create_problem
  use_existing:
    type: set
    vars: { problem: =steps.existing.output.items[0] }
    next: link
  create_problem:
    type: record.create
    record_type: problem
    set:
      title: ="Recurring incidents on " + record.service
      description: ="Raised automatically: " + string(steps.recent.output.count) + " incidents in the last 7 days."
      service: =record.service
    next: remember
  remember:
    type: set
    vars: { problem: =steps.create_problem.output.record }
    next: link
  link:
    type: foreach
    items: =steps.recent.output.items
    max: 50
    run: link-incident
    with: { incident: =item, problem: =vars.problem }
    on_item_error: continue
    next: done
  done:
    type: end
---
kind: workflow
name: link-incident
inputs: [incident, problem]
start: update
steps:
  update:
    type: record.update
    target: =inputs.incident
    set: { problem_id: =inputs.problem.id }
    next: done
  done:
    type: end
```

---

## 2. Change management

### 2.1 Lifecycle

```yaml
kind: lifecycle
record_type: change
initial: draft
states:
  draft:     { label: Draft,       category: open }
  assess:    { label: Assessment,  category: open }
  authorize: { label: Awaiting approval, category: waiting }
  scheduled: { label: Scheduled,   category: waiting }
  implement: { label: Implementing, category: open }
  review:    { label: Review,      category: done }
  closed:    { label: Closed,      category: closed }
  cancelled: { label: Cancelled,   category: closed }
transitions:
  - { from: draft,     to: assess,    verb: Submit for assessment
      when: 'size(record.implementation_plan) > 0',
      error: Describe the implementation plan first. }
  - { from: assess,    to: authorize, verb: Request approval
      when: 'size(record.backout_plan) > 0 && size(record.test_plan) > 0 && has(record.planned_start) && has(record.planned_end)',
      error: Add a backout plan, a test plan and planned start and end times. }
  - { from: authorize, to: scheduled, verb: Schedule }          # driven by the approval workflow
  - { from: authorize, to: draft,     verb: Send back }
  - { from: scheduled, to: authorize, verb: Request re-approval }
  - { from: scheduled, to: implement, verb: Start implementation }
  - { from: implement, to: review,    verb: Complete }
  - { from: review,    to: closed,    verb: Close }
  - { from: draft,     to: cancelled, verb: Cancel }
  - { from: assess,    to: cancelled, verb: Cancel }
  - { from: authorize, to: cancelled, verb: Cancel }
  - { from: scheduled, to: cancelled, verb: Cancel }
```

### 2.2 Before rules

**C1. Score the risk and decide whether CAB must approve** (pure expression; two `set`s, the second sees the first)

```yaml
kind: rule
name: risk-and-approval-path
record_type: change
timing: before
on: [create, update]
when: 'event == "create" || record.type != old.type || record.backout_plan != old.backout_plan || record.ci_id != old.ci_id'
run:
  - action: set
    field: risk_score
    value: =(record.type == "emergency" ? 40 : record.type == "normal" ? 20 : 0) + (size(record.backout_plan) == 0 ? 30 : 0) + (record.service == "payments" ? 20 : 0)
  - action: set
    field: risk
    value: =record.risk_score >= 60 ? "high" : record.risk_score >= 30 ? "medium" : "low"
  - action: set
    field: cab_required
    value: =record.type != "standard" && record.risk != "low"
```

**C2. Planned end must follow planned start** (pure expression)

```yaml
kind: rule
name: valid-window
record_type: change
timing: before
on: [create, update]
when: 'has(record.planned_start) && has(record.planned_end) && record.planned_end <= record.planned_start'
run:
  - action: abort
    message: The planned end has to be after the planned start.
```

**C3. Nothing may be scheduled inside a freeze window** (lookup: `count` with a two-field range)

```yaml
kind: rule
name: respect-freeze-windows
record_type: change
timing: before
on: [create, update]
lookups:
  freezes:
    count: freeze_window
    where:
      starts_at: { lte: =record.planned_end }
      ends_at:   { gte: =record.planned_start }
when: 'record.type != "emergency" && has(record.planned_start) && lookups.freezes > 0'
run:
  - action: abort
    message: The planned window overlaps a change freeze. Pick another time, or raise an emergency change.
```

**C4. No two changes on the same CI at once** (lookup: `count`, overlapping ranges plus `in`)

```yaml
kind: rule
name: no-overlapping-changes
record_type: change
timing: before
on: [create, update]
lookups:
  overlapping:
    count: change
    where:
      ci_id: =record.ci_id
      id: { ne: =record.id }
      status: { in: [scheduled, implement] }
      planned_start: { lt: =record.planned_end }
      planned_end:   { gt: =record.planned_start }
when: 'has(record.ci_id) && has(record.planned_start) && lookups.overlapping > 0'
run:
  - action: abort
    message: Another change is already planned on this item during that window.
```

**C5. Moving a scheduled change needs approval again** (a `set` that triggers a lifecycle transition)

```yaml
kind: rule
name: reschedule-needs-reapproval
record_type: change
timing: before
on: [update]
when: 'old.status == "scheduled" && record.status == "scheduled" && record.cab_required && (record.planned_start != old.planned_start || record.planned_end != old.planned_end)'
run:
  - action: set
    field: status
    value: authorize                  # a literal: no leading =
```

### 2.3 After and async rules

**C6. Start the approval workflow when approval is requested** (after)

```yaml
kind: rule
name: start-approval
record_type: change
timing: after
on: [update]
when: 'old.status != "authorize" && record.status == "authorize"'
run:
  - action: workflow.start
    workflow: change-approval
```

**C7. Remind the implementer, and chase overdue changes** (after)

```yaml
kind: rule
name: start-implementation-watch
record_type: change
timing: after
on: [update]
when: 'old.status != "scheduled" && record.status == "scheduled"'
run:
  - action: workflow.start
    workflow: implementation-watch
```

**C8. Review and close after silence** (after; reuses the shared subflow)

```yaml
kind: rule
name: review-then-close
record_type: change
timing: after
on: [update]
when: 'old.status != "review" && record.status == "review"'
run:
  - action: workflow.start
    workflow: close-after-silence
    with:
      record: =record
      quiet_for: =duration("120h")
      from_status: review
      to_status: closed
      template: change-review-request
```

### 2.4 Workflows

**W5. Change approval**: manager first for normal changes, CAB when required, a fast emergency path.

```yaml
kind: workflow
name: change-approval
record_type: change
start: route
steps:
  route:
    type: condition
    branches:
      - when: 'record.type == "emergency"'
        next: ecab
      - when: 'record.type == "standard"'
        next: approved
    else: manager
  manager:
    type: approval
    approvers: =[record.requester.manager]
    rule: any
    remind_after: 48h                 # the step nudges pending approvers itself
    timeout: 96h
    on_timeout: rejected              # no answer in four days counts as a no
    next: after_manager
  after_manager:
    type: condition
    branches:
      - when: 'steps.manager.output.decision == "rejected"'
        next: rejected
      - when: 'record.cab_required'
        next: cab
    else: approved
  cab:
    type: approval
    approvers: =group("change-advisory-board")
    rule: majority
    timeout: 72h
    on_timeout: rejected
    next: after_cab
  ecab:
    type: approval
    approvers: =group("emergency-cab")
    rule: any
    timeout: 4h
    on_timeout: rejected
    next: after_cab
  after_cab:
    type: condition
    branches:
      - when: 'steps.cab.output.decision == "approved" || steps.ecab.output.decision == "approved"'
        next: approved
    else: rejected
  approved:
    type: record.update
    target: =record
    set: { status: scheduled }
    next: tell_approved
  tell_approved:
    type: notify.email
    to: =record.requester.email
    template: change-approved
    next: done
  rejected:
    type: record.update
    target: =record
    set: { status: draft }
    next: explain
  explain:
    type: comment.add
    body: =has(steps.cab.output.comments) ? steps.cab.output.comments : "The change was not approved."
    next: done
  done:
    type: end
```

**W6. Implementation watch**: remind before the window, flag when it overruns.

```yaml
kind: workflow
name: implementation-watch
record_type: change
start: wait_until_window
steps:
  wait_until_window:
    type: wait
    until_time: =record.planned_start - duration("1h")
    next: remind
  remind:
    type: notify.email
    to: =record.assignee.email
    template: change-starts-soon
    next: wait_until_end
  wait_until_end:
    type: wait
    until: 'record.status != "scheduled" && record.status != "implement"'
    until_time: =record.planned_end
    on_time: overrun_check
    next: done
  overrun_check:
    type: condition
    branches:
      - when: 'record.status == "implement"'
        next: flag
    else: done
  flag:
    type: notify.email
    to: =record.assignee.manager.email
    template: change-overrun
    next: done
  done:
    type: end
```

**W8. Delete stale drafts** (a scheduled workflow; `record.query`, `foreach`, `record.delete`)

Runs every night and removes changes that were never submitted. The deletion is audited with a tombstone.

```yaml
kind: workflow
name: purge-stale-drafts
trigger: { schedule: "0 3 * * *" }          # every day at 03:00
start: find
steps:
  find:
    type: record.query
    record_type: change
    where: { status: draft, created_at: { lt: =now - duration("2160h") } }     # older than 90 days
    limit: 200
    next: remove
  remove:
    type: foreach
    items: =steps.find.output.items
    max: 200
    do:                                    # an inline single step, instead of a one-line subflow (finding F18)
      type: record.delete
      target: =item
    on_item_error: continue
    next: done
  done:
    type: end
```

---

## 3. What each example needed

| Example | Tier | Needs |
|---|---|---|
| I1, I2, I3, C1, C2 | before rule | Expressions only (`set`, `abort`). Nothing else. |
| I4 | before rule | Lookup `count` with equality and a range on `created_at`. |
| I5 | before rule | Lookup `count` on the same type. |
| C3 | before rule | Lookup `count` with a two-field range against another type. |
| C4 | before rule | Lookup `count` with `ne`, `in` and two ranges on the same type. |
| C5 | before rule | `set` on `status`, checked afterwards by the lifecycle. |
| I6, I7, C6, C7, C8 | after rule | `workflow.start` (with inputs, and a per-action `when`). |
| I8 | async rule | `notify.email` to a reference (`assignment_group.email`). |
| I10 | before rule, on delete | Lookup `count`; the `delete` event with `record` as the row being removed. |
| I11 | async rule | `workflow.start`. |
| I9 | after rule | Comment events, and `record.update` on the comment's parent. |
| W1 | workflow | `notify.email`, `wait until` with timeout, `http.request`, `comment.add`. |
| W2 | subflow | `wait`, `notify.email`, `record.update`, `comment.add`, subflow inputs. |
| W3 | workflow | `record.query`, `foreach`, subflow with inputs. |
| W7 | workflow | `record.query` (twice), `condition`, `set` (workflow variables), `record.create` (its output feeds later steps), `foreach` with `on_item_error`, subflow. |
| W8 | workflow | A schedule trigger, `record.query`, `foreach`, `record.delete`. |
| W4 | workflow | `wait until_time`, `condition`, a `scale()` duration helper. |
| W5 | workflow | `condition`, `approval` (any, majority; timeout; output `decision` and `comments`), `record.update`. |
| W6 | workflow | `wait` with `until_time`, `condition`, `notify.email`. |

## 4. Coverage

- **Lookups.** All four blocking cases that needed data (I4, I5, C3, C4) fit one menu: **`count`** over one
  record type, **AND-only** filters, operators `eq ne lt lte gt gte in`, filter values may be expressions. `get`
  and `exists` were not needed (`count > 0` covers `exists`). Nothing needed a join, an OR, a sum, or free text.
- **Workflow steps used**: `notify.email`, `wait` (`until`, `until_time`, `timeout`), `condition`, `approval`,
  `set`, `record.query`, `record.create`, `record.update`, `record.delete`, `foreach`, `subflow` (via
  `run`/`workflow.start`), `comment.add`, `http.request`, `end`.
- **Steps no example needed**: `parallel`/`join`, `webhook`, `notify.sms`, `record.get`. Reasonable to defer all of
  these from the first cut of the engine.
- **Triggers used**: a rule's `workflow.start`, and a schedule (W8). Manual and webhook triggers are not exercised.

## 5. Findings

These are the places where writing real definitions disagreed with the design doc. Each has a proposed resolution.

| # | Finding | Proposed resolution |
|---|---|---|
| F1 | Rules need to know whether a save is a create, and `old` cannot be null: comparing with `null` is rejected by the validator (for some types it silently never matches). | `event` is a variable (`create`, `update` or `delete`), and `old` is an empty record on create. Optional fields are tested with `has(record.field)`. Implemented for lifecycle guards in slice 2; `event` arrives with rules in slice 5. |
| F2 | C1 sets three fields in order and each depends on the last. | Actions run in order, and later expressions see earlier `set`s. Document it. |
| F3 | Examples use `record.assignee.email` and `record.requester.manager.email`, i.e. **expanded references**. | The context expands references up to two levels. Because expressions are analysable, the platform prefetches only the paths an expression actually reads, so a sync rule costs no more queries than it uses. |
| F4 | I9 (a customer reply reopens a ticket) needs **comment events** with access to the parent record. | Allow `record_type: comment` rules. Provide `record.parent`. This is the same polymorphic parent the comments table needs anyway. |
| F5 | After rules cannot loop, so cascading to many records (W3) must be a workflow. | Intended. Confirms `foreach` and subflows are v1 requirements. |
| F6 | `freeze_window` is runtime-editable reference data. A data-only app cannot create a table for it. | For v1 the change module's freeze windows are a **core record type**. This is a concrete case for open question 1 (generic custom records). |
| F7 | W4 is a hand-rolled SLA and is wrong: it does not pause while the incident is on hold, and it ignores business hours. | SLA is a **first-class concept** (`kind: sla` with start, stop and pause conditions, and a business calendar), not something built from waits. Add it to the roadmap. |
| F8 | The examples need CEL helpers beyond the standard library: `scale(duration, double)`, `group("name")`, `has()`, and date arithmetic. | Ship a small documented function library on top of CEL. `group()` is a lookup and is only allowed where the design permits lookups. |
| F9 | `state_category` appears in most lookups and workflows. | Core keeps a denormalised, indexed `state_category` column, updated when the status changes. It also fixes the queue's hardcoded "Open" filter. |
| F10 | Guards (in a lifecycle) and rules overlap. | Convention used above: a guard is a pure single-transition check on the record; a rule is for lookups, several transitions, or side effects. Say so in the design doc. |
| F11 | `approval` needs a defined output (`decision`, `comments`, who decided), rules (`any`, `all`, `majority`), and built-in reminders (`remind_after`) so a workflow does not need a nudge step. | Specify in the step library. `group()` as an approver source is a core feature (teams exist). |
| F12 | I7 needed a per-action `when`. | Allow `when` on any action in a rule's `run` list. |
| F13 | W6's `wait` combines `until`, `until_time` and `on_time`. | Split for clarity: `wait` has either a `duration`, an `until_time`, or an `until` condition, each with an optional `timeout`. Revise the step spec. |
| F14 | Workflow expressions used `inputs.*` and, in `foreach`, `item`. A subflow's `record` input needs to become the workflow's `record`. | Workflow context adds `inputs` and `item`. A declared input named `record` binds the workflow's `record`. |
| F15 | I8 (reassignment notice) and I6 (urgent escalation) use only common fields, and the change app needs the same. I7 does not generalise, because it depends on the incident-specific `resolved` status. | Accepted as D14. Under it, I8 and I6 become rules on `record_type: task` and cover every type (I6 with an acknowledgement condition based on `has(record.assignee)` instead of the `new` status). I7 stays on `incident`. |
| F16 | W7 creates a record inside a workflow that can be retried. | `record.create` carries an idempotency key derived from the execution and step, so a retry never creates a second problem. Specified in the design doc. |
| F17 | W8 deletes records. | `record.delete` needs an explicit `delete` permission, is audited with a tombstone, and is blockable by a before-delete rule (I10). Specified in the design doc. Also needs realtime delete events (none exist yet). |
| F18 | W8 and W3 use `foreach` to run a single step per item, which forces a one-line subflow. | Allow `foreach` to take an inline `do:` step for the single-step case (used in W8), and add bulk steps (`record.update_many`, `record.delete_many`) later for large sets. |
| F19 | Filters need "is empty" (`problem_id: null`) and I10 needs the delete context. | A `null` filter value means IS NULL. On `delete`, `record` is the row being removed and `old` is null. |
| F20 | Workflow triggers other than a rule appeared (W8's schedule). | The `trigger:` block on a workflow supports `schedule` (cron) in v1. Manual and webhook triggers follow. |
