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
- **Start implementing** waits for the planned window to open.

| Board | Role | Rule | Times out after |
| --- | --- | --- | --- |
| Change advisory board | `cab-member` | majority | 5 days |
| Emergency board | `ecab-member` | any one | 4 hours |

## Roles

- `change-manager`: runs the change process: sees, files and edits changes (`records:*:change`), internal notes,
  workflows.
- `cab-member` and `ecab-member`: sit on the boards. They see changes and are asked to approve them; approving needs
  no permission of its own. Grant them to people or to a group from the admin section.

## Notifications

The requester and the assignee hear when a change is scheduled (`change-scheduled`) and when it is sent back to
Assess (`change-sent-back`). Approvers are told by the approval inbox.

## The app's own permissions

Its workflows run as the app, for the person who requested authorization: `records:read:change`,
`records:update:change` (to move the change on) and `comments:internal` (for the note when nobody holds a board
role).
