import { api } from './api';
import type { Group, GroupDetail, Member } from './types';

/**
 * Groups, for the assignment group picker, and each group's members, for the assignee picker. Loaded on first use.
 * Groups change rarely and only by an admin, so there is no live update: the admin page refreshes this after a change.
 */
class Groups {
	list = $state<Group[]>([]);
	/** Members by group id, once asked for. */
	members = $state<Record<string, Member[]>>({});
	private requested = false;
	private pending = new Map<string, Promise<void>>();

	async ensure() {
		if (this.requested) return;
		this.requested = true;
		try {
			await this.reload();
		} catch {
			this.requested = false; // try again next time
		}
	}

	async reload() {
		this.list = (await api<{ items: Group[] }>('/api/v1/groups')).items;
	}

	/** Load a group's members unless they are already known. */
	ensureMembers(id: string): Promise<void> {
		if (this.members[id]) return Promise.resolve();
		let p = this.pending.get(id);
		if (!p) {
			p = api<GroupDetail>(`/api/v1/groups/${id}`)
				.then((g) => {
					this.members[id] = g.members;
				})
				.catch(() => {})
				.finally(() => this.pending.delete(id));
			this.pending.set(id, p);
		}
		return p;
	}

	/** Forget what is cached, after an admin changed something. */
	invalidate(id?: string) {
		if (id) delete this.members[id];
		else this.members = {};
	}
}

export const groups = new Groups();
