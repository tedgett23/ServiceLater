/** Thin fetch wrapper: JSON in and out, same-origin cookies, one silent refresh on 401. */

export class ApiError extends Error {
	constructor(
		public status: number,
		message: string,
		/** The error response as the server sent it, for answers that say more than error (problems, say). */
		public body?: unknown
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
	// A file goes as it is (an import's CSV, say); anything else as JSON.
	if (body instanceof Blob) return fetch(path, { method, headers: { 'Content-Type': body.type || 'text/csv' }, body });
	return fetch(path, {
		method,
		headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
		body: body === undefined ? undefined : JSON.stringify(body)
	});
}

/** Sends, refreshing the session once if it has expired. */
async function sendFresh(path: string, method: string, body?: unknown) {
	let res = await send(path, method, body);
	if (res.status === 401 && !NO_REFRESH.includes(path)) {
		if (await refresh()) res = await send(path, method, body);
		if (res.status === 401) hooks.onSessionExpired();
	}
	return res;
}

/** Saves what a GET answers as a file (an export, say), under the name the server gives it. */
export async function download(path: string, fallbackName: string): Promise<void> {
	const res = await sendFresh(path, 'GET');
	if (!res.ok) {
		let message = res.statusText;
		try {
			message = ((await res.json()) as { error?: string }).error ?? message;
		} catch {
			/* not JSON */
		}
		throw new ApiError(res.status, message);
	}
	const name = /filename="([^"]+)"/.exec(res.headers.get('Content-Disposition') ?? '')?.[1] ?? fallbackName;
	const url = URL.createObjectURL(await res.blob());
	const a = document.createElement('a');
	a.href = url;
	a.download = name;
	a.click();
	setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/** Calls the API: body is sent as JSON, or as it is when it is a file (Blob). */
export async function api<T = void>(path: string, opts: { method?: string; body?: unknown } = {}): Promise<T> {
	const method = opts.method ?? 'GET';
	const res = await sendFresh(path, method, opts.body);
	if (!res.ok) {
		let message = res.statusText;
		let body: unknown;
		try {
			body = await res.json();
			message = (body as { error?: string }).error ?? message;
		} catch {
			/* not JSON */
		}
		throw new ApiError(res.status, message, body);
	}
	return res.status === 204 ? (undefined as T) : ((await res.json()) as T);
}
