import { api, ApiError } from './api';

/** Events are hints that carry ids only; handlers refetch through the REST API. */
export type RealtimeEvent =
	| { type: 'ticket.changed'; id: string; number: number }
	| { type: 'comment.added'; ticket_id: string }
	| { type: 'notification.changed' }
	| { type: 'freeze.changed'; id: string }
	| { type: 'ci.changed'; id: string } // a configuration item or its relationships changed
	| { type: 'email.changed' } // an email was queued, sent or failed (to those who manage settings)
	| { type: 'run.changed'; id: string; app: string; workflow: string } // a workflow run started, moved or ended
	| { type: 'resync' }
	| { type: 'definitions.changed' }; // an app was installed, updated or removed

type Handler = (e: RealtimeEvent) => void;

/**
 * One WebSocket per tab, with reconnect and backoff. Before every reconnect it calls /auth/me, which silently
 * refreshes an expired access token (the server closes sockets when the token expires) and tells us if the
 * session is really gone. After a reconnect it emits `resync`, because anything sent while offline is lost.
 */
class Realtime {
	/** True while the connection is down after having been asked to run. */
	down = $state(false);

	private ws: WebSocket | null = null;
	private handlers = new Set<Handler>();
	private running = false;
	private everOpened = false;
	private attempt = 0;
	private timer: ReturnType<typeof setTimeout> | undefined;

	start() {
		if (this.running) return;
		this.running = true;
		void this.connect();
	}

	stop() {
		this.running = false;
		this.down = false;
		this.everOpened = false;
		clearTimeout(this.timer);
		this.ws?.close();
		this.ws = null;
	}

	on(h: Handler) {
		this.handlers.add(h);
		return () => this.handlers.delete(h);
	}

	private emit(e: RealtimeEvent) {
		for (const h of this.handlers) h(e);
	}

	private async connect() {
		if (!this.running) return;
		try {
			await api('/api/v1/auth/me'); // refreshes the access token if needed
		} catch (e) {
			if (e instanceof ApiError && e.status === 401) return this.stop(); // signed out: the app redirects
			return this.retry(); // offline or server down
		}
		if (!this.running) return;

		const proto = location.protocol === 'https:' ? 'wss' : 'ws';
		const ws = new WebSocket(`${proto}://${location.host}/ws/v1/subscribe`);
		this.ws = ws;

		ws.onopen = () => {
			this.attempt = 0;
			this.down = false;
			if (this.everOpened) this.emit({ type: 'resync' });
			this.everOpened = true;
		};
		ws.onmessage = (m) => {
			try {
				this.emit(JSON.parse(m.data));
			} catch {
				/* ignore malformed frames */
			}
		};
		ws.onclose = () => {
			if (this.ws !== ws) return; // superseded or stopped on purpose
			this.ws = null;
			this.down = true;
			this.retry();
		};
	}

	private retry() {
		if (!this.running) return;
		this.down = true;
		// 0.5s, 1s, 2s ... capped at 20s, with jitter so a server restart isn't a thundering herd.
		const delay = Math.min(20000, 500 * 2 ** this.attempt++) * (0.75 + Math.random() * 0.5);
		this.timer = setTimeout(() => void this.connect(), delay);
	}
}

export const realtime = new Realtime();
