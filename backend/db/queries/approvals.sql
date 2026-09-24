-- name: CreateApproval :exec
INSERT INTO workflow_approvals (execution_id, step_id, approver_id)
VALUES (sqlc.arg('execution_id'), sqlc.arg('step_id'), sqlc.arg('approver_id'))
ON CONFLICT (execution_id, step_id, approver_id) DO NOTHING;

-- name: ListApprovalsForStep :many
SELECT * FROM workflow_approvals WHERE execution_id = $1 AND step_id = $2 ORDER BY created_at;

-- Only a pending row matches, so deciding twice or deciding someone else's request finds nothing (RLS on top of this
-- already keeps it to the approver's own row).
-- name: DecideApproval :one
UPDATE workflow_approvals SET decision = sqlc.arg('decision'), comment = sqlc.arg('comment'), decided_at = now()
WHERE id = sqlc.arg('id') AND decision IS NULL
RETURNING *;

-- The approvals inbox: this person's undecided requests, with enough of the ticket to show in a list. The ticket's
-- reference and title come through app_approval_task_summary, not a plain join: RLS on tasks would otherwise hide
-- the very ticket this request is about, since being asked to approve something is often the reason someone who is
-- not staff, the requester or the assignee needs to see it at all.
-- name: ListMyPendingApprovals :many
SELECT wa.id, wa.execution_id, wa.step_id, wa.created_at,
       e.app, e.workflow, e.record_id,
       COALESCE(s.reference, '')::text AS reference,
       COALESCE(s.title, '')::text AS title
FROM workflow_approvals wa
JOIN workflow_executions e ON e.id = wa.execution_id
LEFT JOIN LATERAL app_approval_task_summary(e.record_id) s ON true
WHERE wa.approver_id = sqlc.arg('approver_id') AND wa.decision IS NULL
ORDER BY wa.created_at;
