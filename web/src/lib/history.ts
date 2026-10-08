import { when } from './format';
import type { HistoryEntry } from './types';

/** Who did it: a person, an app for a person, or ServiceLater itself (the SLA timer). */
export function who(e: HistoryEntry): string {
	if (e.actor_app) return e.on_behalf_of_name ? `${e.actor_app} for ${e.on_behalf_of_name}` : e.actor_app;
	return e.actor_name ?? 'ServiceLater';
}

const value = (e: HistoryEntry, text: string) => (e.kind === 'time' && text ? when(text) : text);

/**
 * What happened, as a sentence after the person's name: "changed Priority from Medium to High". Long text says only
 * that it changed; the before and after are shown apart (see `long`).
 */
export function what(e: HistoryEntry, subject = 'it'): string {
	const label = e.label;
	const from = value(e, e.old_text);
	const to = value(e, e.new_text);
	switch (e.action) {
		case 'created':
			return `created ${subject}`;
		case 'deleted':
			return `deleted ${subject}`;
		case 'linked':
			if (e.field === 'attachment') return `attached ${to}`;
			return `linked ${label.toLowerCase()} ${to}`;
		case 'unlinked':
			if (e.field === 'attachment') return `removed the attachment ${from}`;
			return `unlinked ${label.toLowerCase()} ${from}`;
	}
	if (e.kind === 'long') return from ? `changed ${label}` : `wrote ${label}`;
	if (!from) return `set ${label} to ${to}`;
	if (!to) return `cleared ${label} (was ${from})`;
	return `changed ${label} from ${from} to ${to}`;
}

/** Whether to offer the before and after of a long text. */
export const long = (e: HistoryEntry) => e.action === 'changed' && e.kind === 'long';
