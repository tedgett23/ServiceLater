# service-catalog

Self-service requests: the catalog people order from, requests and the tasks that fulfil them (docs/apps-platform.md
§8.4). First-party ServiceLater app, installed by default. Same structure and install path as any third-party app.

- `lifecycles/`: requests (submitted, awaiting approval, in progress, fulfilled, closed, rejected, cancelled) and tasks.
- `catalog/`: the items. Add your own here in `local/catalog/`, or change a shipped one by key (its questions, who may
  order it, its workflow).
- `workflows/`: how each item is fulfilled: approvals (the requester's manager, then others), then a task for each
  team involved.
- `roles/software-approver.yaml`: who approves administrator access to software.

The workflows file tasks for these teams: Procurement, Desktop Support, Service Desk, HR and Facilities. Create those
groups, or rename them in `local/workflows/`. A team that does not exist gets its tasks unrouted (they show in the
Unassigned queue) until you do. Set each person's manager in Admin → Users.
