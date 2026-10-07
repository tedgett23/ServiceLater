# change-management

Change requests with approval workflows, risk assessment and change calendar. (P0)

First-party ServiceLater app. Same structure and install path as any third-party app (see §11 of the MVP doc).

## Roles

- `change-manager`: runs the change process (sees and edits tickets, internal notes, workflows).
- `cab-member`: sits on the change advisory board. Grant it to people or to a group, and ask the board with an
  approval step: `approvers: "role('cab-member')"`.

There is no change record type yet, so both cover every ticket for now; they narrow to changes when it arrives.
