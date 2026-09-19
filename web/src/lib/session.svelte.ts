import { goto } from '$app/navigation';
import { api, hooks } from './api';
import type { User } from './types';

class Session {
	user = $state<User | null>(null);
	private loaded = false;

	/** Resolves the current user once, then serves the cached value. */
	async ensure(): Promise<User | null> {
		if (this.user || this.loaded) return this.user;
		try {
			this.user = await api<User>('/api/v1/auth/me');
		} catch {
			this.user = null;
		}
		this.loaded = true;
		return this.user;
	}

	async login(email: string, password: string) {
		this.user = await api<User>('/api/v1/auth/login', { method: 'POST', body: { email, password } });
		this.loaded = true;
	}

	async logout() {
		try {
			await api('/api/v1/auth/logout', { method: 'POST' });
		} finally {
			this.clear();
			await goto('/login');
		}
	}

	clear() {
		this.user = null;
		this.loaded = true;
	}
}

export const session = new Session();

hooks.onSessionExpired = () => {
	if (!session.user) return;
	session.clear();
	void goto('/login?expired=1');
};
