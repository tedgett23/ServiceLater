import { api } from './api';
import type { User } from './types';

/** Who may be assigned a ticket of each type (records:update:<type>), for the assignee picker. Loaded once per type. */
class Staff {
	byType = $state<Record<string, User[]>>({});
	private requested = new Set<string>();

	async ensure(type: string) {
		if (this.requested.has(type)) return;
		this.requested.add(type);
		try {
			const items = (await api<{ items: User[] }>(`/api/v1/users/staff?type=${encodeURIComponent(type)}`)).items;
			this.byType = { ...this.byType, [type]: items };
		} catch {
			this.requested.delete(type); // try again next time
		}
	}

	of(type: string): User[] {
		return this.byType[type] ?? [];
	}
}

export const staff = new Staff();
