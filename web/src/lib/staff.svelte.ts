import { api } from './api';
import type { User } from './types';

/** Agents and admins, for the assignee picker. Loaded once, on first use. */
class Staff {
	list = $state<User[]>([]);
	private requested = false;

	async ensure() {
		if (this.requested) return;
		this.requested = true;
		try {
			this.list = (await api<{ items: User[] }>('/api/v1/users/staff')).items;
		} catch {
			this.requested = false; // try again next time
		}
	}
}

export const staff = new Staff();
