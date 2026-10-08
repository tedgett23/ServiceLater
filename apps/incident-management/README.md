# incident-management

Incidents: their status flow, their form, and the respond and resolve SLAs. (P0)

First-party ServiceLater app. Same structure and install path as any third-party app (see §11 of the MVP doc).

## Form

`forms/incident.yaml` (docs/apps-platform.md §8.1) adds four fields and lays out the form for filing and the
record:

| Field | What it is |
|---|---|
| Category | hardware, software, network, access and accounts, or something else |
| Subcategory | offered once there is a category (not for something else), with only that category's options |
| Impact | many people or a whole service, a team, or one person |
| Urgency | work has stopped, is slowed, or can wait |

Once impact and urgency are both set, the `priority-from-impact` rule works the priority out (impact + urgency - 1,
at most 4) and the form makes priority read-only for people; clearing either gives it back. Nothing is required, so
records filed through the API without these fields are fine.

To add a field, relabel one, make one required or move it, write `forms/incident.yaml` in this instance's local layer
(`SL_APPS_LOCAL_DIR/incident-management/forms/`). Fields merge by name; `layout.create` and `layout.detail` replace
the shipped lists, so give them in full.

## SLAs

Two clocks start on every incident (docs/apps-platform.md §6.6). Both count working hours of the `business-hours`
calendar (Monday to Friday, 09:00 to 17:00 UTC until an administrator changes it in Admin > Calendars), and stand
still while the incident is on hold.

| SLA | Met when | Critical | High | Medium | Low |
|---|---|---|---|---|---|
| Respond | someone is assigned (or it is finished first) | 30m | 1h | 4h | 8h |
| Resolve | it is resolved or closed | 4h | 8h | 24h | 40h |

At 75% of the target the assignment group (respond) or the assignee (resolve) is warned; on a breach they are told,
and for resolve the assignment group as well. A change of priority changes the target and keeps the time used; a
reopened incident's resolve clock picks up where it stopped.

To change targets, the calendar or who is told, override `slas/respond.yaml` or `slas/resolve.yaml` in this
instance's local layer (`SL_APPS_LOCAL_DIR/incident-management/slas/`). The wording is in `notifications/`.

The app's principal may read incidents (`records:read:incident`), so it can tell people about a breach that no
person's change set off.
