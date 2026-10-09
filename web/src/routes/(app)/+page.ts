import { redirect } from '@sveltejs/kit';
import { canRecordAny } from '$lib/types';

// Home: staff work the queue; everyone else starts at the portal, to report a problem or request something.
export const load = async ({ parent }) => {
	const { user } = await parent();
	redirect(307, canRecordAny(user, 'read') ? '/tickets' : '/catalog');
};
