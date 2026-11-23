import { redirect } from '@sveltejs/kit';
import { can, P } from '$lib/types';

// The SQL workbench is for those who may run queries (admins, and core-itsm's analyst role).
export const load = async ({ parent }) => {
	const { user } = await parent();
	if (!can(user, P.queryRun)) redirect(307, '/');
	return { user };
};
