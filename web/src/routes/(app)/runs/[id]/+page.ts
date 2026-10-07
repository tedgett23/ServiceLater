import { redirect } from '@sveltejs/kit';
import { can, P } from '$lib/types';

// Workflow runs are for staff. Everyone else goes back to their tickets.
export const load = async ({ parent }) => {
	const { user } = await parent();
	if (!can(user, P.workflowsReadAll)) redirect(307, '/tickets');
	return {};
};
