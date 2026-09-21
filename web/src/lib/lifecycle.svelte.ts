import { api } from './api';
import type { LifecycleData } from './types';

/** "in_progress" becomes "In progress": the best label for a status the lifecycle does not know. */
function humanize(id: string) {
	const text = id.replaceAll('_', ' ');
	return text.charAt(0).toUpperCase() + text.slice(1);
}

/**
 * The states of the ticket lifecycle as the installed app defines them: their labels, and their categories (which
 * choose the shape of the status marker). What a person may do next is not here: the server sends each ticket's
 * moves, because a guard depends on the ticket and on who is acting.
 */
class Lifecycle {
	data = $state<LifecycleData | null>(null);
	private loading: Promise<void> | null = null;

	/** Loads once; later calls wait for the same request. */
	ensure(): Promise<void> {
		this.loading ??= this.reload();
		return this.loading;
	}

	async reload() {
		try {
			this.data = await api<LifecycleData>('/api/v1/lifecycles/incident');
		} catch {
			/* keep what we have: labels fall back to the status id */
		}
	}

	label(id: string) {
		return this.data?.states.find((s) => s.id === id)?.label ?? humanize(id);
	}

	isInitial(id: string) {
		return this.data?.initial === id;
	}

	categoryOf(id: string) {
		return this.data?.states.find((s) => s.id === id)?.category;
	}
}

export const lifecycle = new Lifecycle();
