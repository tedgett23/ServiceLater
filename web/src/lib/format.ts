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

/** A length of time for SLA clocks: 45m, 3h 20m, 2d 4h. */
export function span(ms: number): string {
	const m = Math.max(0, Math.round(ms / 60000));
	if (m < 60) return `${m}m`;
	const h = Math.floor(m / 60);
	if (h < 24) return m % 60 ? `${h}h ${m % 60}m` : `${h}h`;
	const d = Math.floor(h / 24);
	return h % 24 ? `${d}d ${h % 24}h` : `${d}d`;
}

/** A size in bytes the way people read it: 840 KB, 1.5 GB. */
export function bytes(n: number): string {
	if (n < 1024) return `${n} B`;
	const units = ['KB', 'MB', 'GB', 'TB'];
	let v = n / 1024;
	let i = 0;
	while (v >= 1024 && i < units.length - 1) {
		v /= 1024;
		i++;
	}
	return `${v >= 10 ? Math.round(v) : Math.round(v * 10) / 10} ${units[i]}`;
}
