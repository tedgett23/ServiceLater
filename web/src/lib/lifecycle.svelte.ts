import { api } from './api';
import type { LifecycleData, RecordType } from './types';

/** "in_progress" becomes "In progress": the best label for a status the lifecycle does not know. */
function humanize(id: string) {
	const text = id.replaceAll('_', ' ');
	return text.charAt(0).toUpperCase() + text.slice(1);
}

/**
 * The record types in use, and the states of each one's lifecycle as the installed apps define them: their labels,
 * and their categories (which choose the shape of the status marker). What a person may do next is not here: the
 * server sends each ticket's moves, because a guard depends on the ticket and on who is acting.
 */
class Lifecycle {
	types = $state<RecordType[]>([]);
	byType = $state<Record<string, LifecycleData>>({});
	private loading: Promise<void> | null = null;

	/** Loads once; later calls wait for the same request. */
	ensure(): Promise<void> {
		this.loading ??= this.reload();
		return this.loading;
	}

	async reload() {
		try {
			const types = (await api<{ items: RecordType[] }>('/api/v1/record-types')).items;
			const loaded = await Promise.all(types.map((t) => api<LifecycleData>(`/api/v1/lifecycles/${t.type}`)));
			this.types = types;
			this.byType = Object.fromEntries(loaded.map((l) => [l.record_type, l]));
		} catch {
			/* keep what we have: labels fall back to the status id */
		}
	}

	/** The types this person may file, for the New ticket dialog. */
	get fileable() {
		return this.types.filter((t) => t.can_file);
	}

	label(id: string, type: string) {
		return this.byType[type]?.states.find((s) => s.id === id)?.label ?? humanize(id);
	}

	isInitial(id: string, type: string) {
		return this.byType[type]?.initial === id;
	}

	categoryOf(id: string, type: string) {
		return this.byType[type]?.states.find((s) => s.id === id)?.category;
	}

	/** What people call a type: "Change" for change. */
	typeLabel(type: string) {
		return this.types.find((t) => t.type === type)?.label ?? humanize(type);
	}
}

export const lifecycle = new Lifecycle();
