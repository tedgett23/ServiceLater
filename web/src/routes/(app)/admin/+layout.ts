import { redirect } from '@sveltejs/kit';
import { canAdmin } from '$lib/types';

// Managing people, teams and roles. Anyone without a permission for any of it goes back to their tickets; each page
// checks its own.
export const load = async ({ parent }) => {
	const { user } = await parent();
	if (!canAdmin(user)) redirect(307, '/tickets');
	return { user };
};
