import { redirect } from '@sveltejs/kit';
import { session } from '$lib/session.svelte';

export const load = async ({ url }) => {
	const user = await session.ensure();
	if (!user) {
		const next = url.pathname + url.search;
		redirect(307, next === '/' ? '/login' : `/login?next=${encodeURIComponent(next)}`);
	}
	return { user };
};
