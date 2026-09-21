export const priorityLabel = { 1: 'Critical', 2: 'High', 3: 'Medium', 4: 'Low' } as const;

/** Compact age for dense lists: 5m, 3h, 2d, then a short date. */
export function age(iso: string, now = Date.now()): string {
	const s = Math.max(0, (now - new Date(iso).getTime()) / 1000);
	if (s < 60) return 'now';
	if (s < 3600) return `${Math.floor(s / 60)}m`;
	if (s < 86400) return `${Math.floor(s / 3600)}h`;
	if (s < 86400 * 14) return `${Math.floor(s / 86400)}d`;
	return new Date(iso).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

export function when(iso: string): string {
	return new Date(iso).toLocaleString(undefined, {
		month: 'short',
		day: 'numeric',
		hour: 'numeric',
		minute: '2-digit'
	});
}
