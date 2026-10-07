# core-itsm

Shared foundations: base roles, teams, notification templates and common form fields.

First-party ServiceLater app. Same structure and install path as any third-party app (see §11 of the MVP doc).

## Notifications

Two rules tell people about tickets in the app, for every record type (`record_type: task`):

- `notify-assignee`: the new assignee, with `ticket-assigned`.
- `notify-group`: the members of the assignment group, with `ticket-routed`, when a ticket lands there with nobody
  assigned.

Change the wording in `local/notifications/`, or who is told and when in `local/rules/` (or remove a rule with
`null`). Whatever a rule asks, the platform never tells someone about their own change, anyone deactivated, or
anyone who cannot see the ticket.
