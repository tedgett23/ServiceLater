import { error } from '@sveltejs/kit';
import { api, ApiError } from '$lib/api';
import type { Comment, Ticket } from '$lib/types';

// Runs on hover-preload as well as navigation, so opening a ticket usually feels instant.
export const load = async ({ params }) => {
	const loadedAt = Date.now(); // a change after this may not be in what is fetched (see realtime.changedSince)
	try {
		const [ticket, comments] = await Promise.all([
			api<Ticket>(`/api/v1/tickets/${params.id}`),
			api<{ items: Comment[] }>(`/api/v1/tickets/${params.id}/comments`)
		]);
		return { ticket, comments: comments.items, loadedAt };
	} catch (e) {
		if (e instanceof ApiError && e.status === 404) error(404, 'Ticket not found');
		throw e;
	}
};
