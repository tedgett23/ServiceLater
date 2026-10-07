import { redirect } from '@sveltejs/kit';
import { can, P } from '$lib/types';

// The first admin page this person may use.
export const load = async ({ parent }) => {
	const { user } = await parent();
	if (can(user, P.usersManage)) redirect(307, '/admin/users');
	if (can(user, P.groupsManage)) redirect(307, '/admin/groups');
	redirect(307, '/admin/roles');
};
