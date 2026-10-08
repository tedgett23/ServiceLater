import { redirect } from '@sveltejs/kit';
import { can, P } from '$lib/types';

// Workflows are for staff who follow them, and for those who edit them.
export const load = async ({ parent }) => {
	const { user } = await parent();
	if (!can(user, P.workflowsReadAll) && !can(user, P.workflowsEdit)) redirect(307, '/tickets');
	return { user };
};
