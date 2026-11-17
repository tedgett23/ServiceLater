# change-management

Change requests with approval workflows, risk assessment and change calendar. (P0)

First-party ServiceLater app, installed at startup. Same structure and install path as any third-party app (see §11
of the MVP doc). Change anything here for one install in `local/`, never by editing `default/`.

## The process (`lifecycles/change.yaml`)

Draft → Assess → Authorize → Scheduled → Implement → Review → Closed, and Cancelled until work starts.

- **Submit** needs the implementation and back-out plans.
- **Request authorization** needs the risk, the impact and a planned window. A *normal* change then goes to the
  change advisory board and an *emergency* one to the emergency board (`rules/ask-the-board.yaml`).
- **Authorize** is automatic: only the board's decision makes it (`workflows/cab-approval.yaml`,
  `workflows/ecab-approval.yaml`). A rejection, or no decision in time, sends the change back to Assess with a
  comment saying why.
- A *standard* change is pre-approved: it is **scheduled** straight from Assess and never goes to a board.
- **Start implementing** waits for the planned window to open, and for any change freeze to end (see below).

| Board | Role | Rule | Times out after |
| --- | --- | --- | --- |
| Change advisory board | `cab-member` | majority | 5 days |
| Emergency board | `ecab-member` | any one | 4 hours |

## Change freezes

Freezes are declared, changed and lifted on the change calendar by anyone with `freezes:manage` (admins and change
managers). The table is core; what a freeze means is this app's content:

- `rules/respect-freeze-windows.yaml` refuses a planned window that overlaps a freeze, when the window or the kind
  changes. Edges may touch, and a change planned before the freeze was declared can still be edited.
- The Scheduled to Implement move in `lifecycles/change.yaml` is held back while a freeze is on.
- An *emergency* change is exempt from both. To make emergencies respect freezes too, override the rule's `when` and
  the guard in `local/`.

Both use a lookup (`count: freeze_window`, docs/apps-platform.md §5), so a freeze counts under the person's own access.

## Problems

A change can record the problem it fixes (its Problem field, or **Raise a change** on a problem). When it finishes
well (reaches Review), `rules/resolve-fixed-problem.yaml` comments on the problem and moves it to Resolved, if its
lifecycle allows (Fix in progress, root cause and fix written down); resolving it resolves its incidents.

## Roles

- `change-manager`: runs the change process: sees, files and edits changes (`records:*:change`), internal notes,
  workflows, and declares change freezes (`freezes:manage`).
- `cab-member` and `ecab-member`: sit on the boards. They see changes and are asked to approve them; approving needs
  no permission of its own. Grant them to people or to a group from the admin section.

## Notifications

The requester and the assignee hear when a change is scheduled (`change-scheduled`) and when it is sent back to
Assess (`change-sent-back`). Approvers are told by the approval inbox.

## The app's own permissions

Its workflows run as the app, for the person who requested authorization: `records:read:change`,
`records:update:change` (to move the change on) and `comments:internal` (for the note when nobody holds a board
role).
