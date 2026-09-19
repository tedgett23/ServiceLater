import type { Status } from './types';

export const statusLabel: Record<Status, string> = {
	new: 'New',
	in_progress: 'In progress',
	on_hold: 'On hold',
	resolved: 'Resolved',
	closed: 'Closed'
};

export const priorityLabel = { 1: 'Critical', 2: 'High', 3: 'Medium', 4: 'Low' } as const;

/** Mirrors the server's state machine (backend/internal/api/tickets.go). The server stays the authority. */
export const nextStatuses: Record<Status, Status[]> = {
	new: ['in_progress', 'on_hold', 'resolved', 'closed'],
	in_progress: ['on_hold', 'resolved'],
	on_hold: ['in_progress', 'resolved'],
	resolved: ['in_progress', 'closed'],
	closed: []
};

/** The action verb for moving a ticket from one status to another. */
export function transitionVerb(from: Status, to: Status): string {
	if (to === 'in_progress') return from === 'new' ? 'Start work' : from === 'on_hold' ? 'Resume' : 'Reopen';
	if (to === 'on_hold') return 'Put on hold';
	if (to === 'resolved') return 'Resolve';
	return 'Close';
}

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
