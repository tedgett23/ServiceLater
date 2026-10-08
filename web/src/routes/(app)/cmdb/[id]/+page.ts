import { redirect } from '@sveltejs/kit';
import { can, P } from '$lib/types';

export const load = async ({ parent }) => {
	const { user } = await parent();
	if (!can(user, P.cmdbRead)) redirect(307, '/tickets');
	return { user };
};
