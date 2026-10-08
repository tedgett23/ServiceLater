import { redirect } from '@sveltejs/kit';
import { can, P } from '$lib/types';

// The CMDB is for those who may read it. Everyone else goes back to their tickets.
export const load = async ({ parent }) => {
	const { user } = await parent();
	if (!can(user, P.cmdbRead)) redirect(307, '/tickets');
	return { user };
};
