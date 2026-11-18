import { redirect } from '@sveltejs/kit';
import { can, P } from '$lib/types';

// Reports are for those who may read them (agents and admins built in).
export const load = async ({ parent }) => {
	const { user } = await parent();
	if (!can(user, P.reportsRead)) redirect(307, '/');
	return { user };
};
