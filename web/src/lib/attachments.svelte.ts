/** Opening attached files, and the previews of pictures, shared by the Attachments section and the Activity feed. */
import { api } from '$lib/api';
import type { Attachment } from '$lib/types';

/** Picture previews by attachment id: a short-lived link, fetched once per file ('' while it is being fetched). */
export const previews = $state<Record<string, string>>({});

/** Fetches a picture's preview link, once. */
export function preview(a: Attachment) {
	if (!a.inline || a.id in previews) return;
	previews[a.id] = '';
	api<{ url: string }>(`/api/v1/attachments/${a.id}/download?inline=true`)
		.then((r) => (previews[a.id] = r.url))
		.catch(() => {});
}

/** Opens a file: a picture in a new tab, anything else saved (the server says Content-Disposition: attachment). */
export async function openAttachment(a: Attachment) {
	const r = await api<{ url: string }>(`/api/v1/attachments/${a.id}/download?inline=${a.inline}`);
	if (a.inline) {
		window.open(r.url, '_blank', 'noopener');
		return;
	}
	const link = document.createElement('a');
	link.href = r.url;
	link.rel = 'noopener';
	link.click();
}
