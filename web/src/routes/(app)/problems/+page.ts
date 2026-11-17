import { redirect } from '@sveltejs/kit';
import { canRecord } from '$lib/types';

// Problems are for those who may read them. Everyone else goes back to their tickets.
export const load = async ({ parent }) => {
	const { user } = await parent();
	if (!canRecord(user, 'read', 'problem')) redirect(307, '/tickets');
	return { user };
};
