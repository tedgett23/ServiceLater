import { api } from './api';
import type { Approval } from './types';

/**
 * The signed-in person's own pending approvals: at most a handful for almost everyone, so this is one shared list
 * (not a paged queue like tickets), loaded once and refreshed after answering one or on request.
 */
class Approvals {
	items = $state<Approval[]>([]);
	loading = $state(false);
	error = $state('');
	private loaded = false;

	async load() {
		this.loading = true;
		this.error = '';
		try {
			const page = await api<{ items: Approval[] }>('/api/v1/approvals');
			this.items = page.items;
			this.loaded = true;
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Could not load approvals';
		} finally {
			this.loading = false;
		}
	}

	/** Loads once per session, so the header can cheaply ask "does this person have anything to approve". */
	async ensureLoaded() {
		if (!this.loaded) await this.load();
	}

	/** Answers one, optimistically: it leaves the list at once and comes back if the server refuses the decision. */
	async decide(id: string, decision: 'approved' | 'rejected', comment: string) {
		const before = this.items;
		this.items = this.items.filter((a) => a.id !== id);
		try {
			await api(`/api/v1/approvals/${id}/decide`, { method: 'POST', body: { decision, comment } });
		} catch (e) {
			this.items = before;
			throw e;
		}
	}
}

export const approvals = new Approvals();
