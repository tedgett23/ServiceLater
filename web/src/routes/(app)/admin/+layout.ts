import { redirect } from '@sveltejs/kit';

// Managing people and teams is an admin's job. Everyone else goes back to their tickets.
export const load = async ({ parent }) => {
	const { user } = await parent();
	if (user.role !== 'admin') redirect(307, '/tickets');
	return { user };
};
