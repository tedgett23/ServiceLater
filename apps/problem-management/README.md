# problem-management

Problems: the causes behind incidents. First-party ServiceLater app, installed at startup. Same structure and install
path as any third-party app. Change anything here for one install in `local/`, never by editing `default/`.

## The process (`lifecycles/problem.yaml`)

New → Assess → Root cause analysis → Fix in progress → Resolved → Closed, and Cancelled until it is resolved.

- **Start the fix** needs the root cause written down.
- **Resolve** needs the root cause and the fix.

## Incidents and problems

An incident can be linked to one problem (core: `incidents.problem_id`, set from the incident's Problem field, or by
raising a problem from the incident). The problem lists its incidents.

- **Known error** (`forms/problem.yaml`): a problem with a workaround can be marked a known error. Marking it adds a
  comment with the workaround to each open linked incident (`rules/share-workaround.yaml`).
- **Resolving the problem resolves its open incidents** (`rules/resolve-linked-incidents.yaml`): the fix becomes each
  one's resolution, with a comment naming the problem. Each incident is saved on its own, through its own rules and
  guards; one that refuses stays open and the refusal is recorded.

Both use the `linked.update` rule action (docs/apps-platform.md §6), as this app's principal for the person who
changed the problem, so the manifest holds `records:update:incident`.

## Roles

- `problem-manager`: sees, files and edits problems, and sees and links incidents. Agents can already do all of it.
