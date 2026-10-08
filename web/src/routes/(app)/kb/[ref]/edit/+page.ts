import { redirect } from '@sveltejs/kit';
import { can, P } from '$lib/types';

export const load = async ({ parent }) => {
	const { user } = await parent();
	if (!can(user, P.knowledgeWrite) && !can(user, P.knowledgePublish)) redirect(307, '/kb');
	return { user };
};
