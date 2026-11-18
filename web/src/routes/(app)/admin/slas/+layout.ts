import { redirect } from '@sveltejs/kit';
import { can, P } from '$lib/types';

export const load = async ({ parent }) => {
	const { user } = await parent();
	if (!can(user, P.slasManage)) redirect(307, '/admin');
	return { user };
};
