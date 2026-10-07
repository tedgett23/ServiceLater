import { api } from './api';
import type { Notification } from './types';

/**
 * The signed-in person's own notifications: like approvals, a handful at most for almost everyone, so this is one
 * shared list loaded once and refreshed on a realtime hint rather than a paged queue.
 */
class Notifications {
	items = $state<Notification[]>([]);
	unreadCount = $state(0);
	loading = $state(false);
	error = $state('');
	private loaded = false;

	async load() {
		this.loading = true;
		this.error = '';
		try {
			const page = await api<{ items: Notification[]; unread_count: number }>('/api/v1/notifications');
			this.items = page.items;
			this.unreadCount = page.unread_count;
			this.loaded = true;
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Could not load notifications';
		} finally {
			this.loading = false;
		}
	}

	/** Loads once per session, so the header can cheaply ask "does this person have anything new". */
	async ensureLoaded() {
		if (!this.loaded) await this.load();
	}

	/** Marks one read, optimistically: it stops counting toward the badge at once and comes back if the server
	 * refuses (already read, or not this person's). */
	async markRead(id: string) {
		const item = this.items.find((n) => n.id === id);
		if (!item || item.read_at) return;
		const before = this.unreadCount;
		item.read_at = new Date().toISOString();
		this.unreadCount = Math.max(0, this.unreadCount - 1);
		try {
			await api(`/api/v1/notifications/${id}/read`, { method: 'POST' });
		} catch {
			item.read_at = undefined;
			this.unreadCount = before;
		}
	}

	/** Opening a ticket answers what this person was told about it. Asks the server only when there is something
	 * unread about it here, so opening tickets costs nothing otherwise. */
	async readTask(taskId: string) {
		const unread = this.items.filter((n) => n.task_id === taskId && !n.read_at);
		if (unread.length === 0) return;
		const before = this.unreadCount;
		const now = new Date().toISOString();
		for (const n of unread) n.read_at = now;
		this.unreadCount = Math.max(0, this.unreadCount - unread.length);
		try {
			await api('/api/v1/notifications/read', { method: 'POST', body: { task_id: taskId } });
		} catch {
			for (const n of unread) n.read_at = undefined;
			this.unreadCount = before;
		}
	}
}

export const notifications = new Notifications();
