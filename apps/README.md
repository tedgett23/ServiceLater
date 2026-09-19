# apps/

First-party ServiceLater apps. Each folder is a complete app (`app.yaml`, `workflows/`, `forms/`, `catalog/`,
`knowledge/`, `scripts/`, `ui/`, `migrations/`) installed with the same CLI as third-party apps:

    servicelater app install ./apps/incident-management

Install order follows `dependencies:` in each `app.yaml` (everything depends on `core-itsm`).
The Workflows & Approvals module is the workflow engine (`backend/internal/workflow`) plus the builder UI
(`web/`), not an app.
