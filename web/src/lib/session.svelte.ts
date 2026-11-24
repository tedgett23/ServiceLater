import { api, hooks } from './api';
import type { User } from './types';

// Cookies are shared across tabs. Let other workspaces discard their stores too, including ones restored by Back.
const sessionKey = 'sl.session.change';
function signalChange() {
	try {
		localStorage.setItem(sessionKey, String(Date.now()));
	} catch { /* storage may be disabled */ }
}

export function watchSession() {
	const changed = (e: StorageEvent) => { if (e.key === sessionKey) location.reload(); };
	const restored = (e: PageTransitionEvent) => { if (e.persisted) location.reload(); };
	window.addEventListener('storage', changed);
	window.addEventListener('pageshow', restored);
	return () => {
		window.removeEventListener('storage', changed);
		window.removeEventListener('pageshow', restored);
	};
}

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
		await api<User>('/api/v1/auth/login', { method: 'POST', body: { email, password } });
		signalChange();
	}

	async logout() {
		try {
			await api('/api/v1/auth/logout', { method: 'POST' });
		} finally {
			this.clear();
			signalChange();
			// A new document discards all account-owned stores, subscriptions and pending callbacks together.
			window.location.replace('/login');
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
	window.location.replace('/login?expired=1');
};
