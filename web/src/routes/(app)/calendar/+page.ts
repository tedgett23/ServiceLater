import { redirect } from '@sveltejs/kit';
import { canRecord } from '$lib/types';

// The calendar is for those who see changes. Everyone else goes back to their tickets.
export const load = async ({ parent }) => {
	const { user } = await parent();
	if (!canRecord(user, 'read', 'change')) redirect(307, '/tickets');
	return {};
};
