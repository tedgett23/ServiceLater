import { redirect } from '@sveltejs/kit';
import { isStaff } from '$lib/types';

// Workflow runs are for staff. Everyone else goes back to their tickets.
export const load = async ({ parent }) => {
	const { user } = await parent();
	if (!isStaff(user)) redirect(307, '/tickets');
	return {};
};
