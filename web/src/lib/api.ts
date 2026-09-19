/** Thin fetch wrapper: JSON in and out, same-origin cookies, one silent refresh on 401. */

export class ApiError extends Error {
	constructor(
		public status: number,
		message: string
	) {
		super(message);
	}
}

/** Set by the session module: called when the session cannot be recovered. */
export const hooks = { onSessionExpired: () => {} };

const NO_REFRESH = ['/api/v1/auth/login', '/api/v1/auth/refresh', '/api/v1/auth/logout'];

let refreshing: Promise<boolean> | null = null;

function refresh(): Promise<boolean> {
	// Single flight: parallel 401s share one refresh, so the rotated token isn't replayed.
	refreshing ??= fetch('/api/v1/auth/refresh', { method: 'POST' })
		.then((r) => r.ok)
		.catch(() => false)
		.finally(() => {
			refreshing = null;
		});
	return refreshing;
}

function send(path: string, method: string, body?: unknown) {
	return fetch(path, {
		method,
		headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
		body: body === undefined ? undefined : JSON.stringify(body)
	});
}

export async function api<T = void>(path: string, opts: { method?: string; body?: unknown } = {}): Promise<T> {
	const method = opts.method ?? 'GET';
	let res = await send(path, method, opts.body);
	if (res.status === 401 && !NO_REFRESH.includes(path)) {
		if (await refresh()) res = await send(path, method, opts.body);
		if (res.status === 401) hooks.onSessionExpired();
	}
	if (!res.ok) {
		let message = res.statusText;
		try {
			message = (await res.json()).error ?? message;
		} catch {
			/* not JSON */
		}
		throw new ApiError(res.status, message);
	}
	return res.status === 204 ? (undefined as T) : ((await res.json()) as T);
}
