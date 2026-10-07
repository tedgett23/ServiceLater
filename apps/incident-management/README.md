# incident-management

Incidents: their status flow, and the respond and resolve SLAs. (P0)

First-party ServiceLater app. Same structure and install path as any third-party app (see §11 of the MVP doc).

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
