import { redirect } from '@sveltejs/kit';
import { lifecycle } from '$lib/lifecycle.svelte';
import { session } from '$lib/session.svelte';

export const load = async ({ url }) => {
	const user = await session.ensure();
	if (!user) {
		const next = url.pathname + url.search;
		redirect(307, next === '/' ? '/login' : `/login?next=${encodeURIComponent(next)}`);
	}
	await lifecycle.ensure(); // labels and buttons come from the installed lifecycle
	return { user };
};
