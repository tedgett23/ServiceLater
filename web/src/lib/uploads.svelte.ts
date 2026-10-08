/**
 * Files on their way to a record. An upload is in parts (the server says how many and how large): each part is PUT to
 * the URL the server gives for it, straight to the bucket or to the server, a few at once, and the upload is then
 * completed with every part's ETag. Uploads live here rather than in a component, so they carry on while people move
 * around the app (the New ticket dialog closes as soon as the ticket exists).
 */
import { api, ApiError } from '$lib/api';
import type { Attachment } from '$lib/types';

interface Started {
	attachment: Attachment;
	part_size: number;
	parts: number;
}
interface PartURL {
	number: number;
	url: string;
	size: number;
}

export interface Upload {
	/** Stable for the list; the attachment's id is known only once the server has it. */
	key: number;
	ticketId: string;
	name: string;
	size: number;
	sent: number;
	status: 'uploading' | 'failed';
	error?: string;
}

/** Parts in flight at once, part URLs asked for at a time, and tries per part. */
const PARALLEL = 3;
const BATCH = 10;
const TRIES = 3;

let next = 1;

class Uploads {
	items = $state<Upload[]>([]);
	private stops = new Map<number, () => void>();

	/** The uploads to a record, in flight or failed. */
	for(ticketId: string): Upload[] {
		return this.items.filter((u) => u.ticketId === ticketId);
	}

	/** Starts uploading files to a record; done is called with each finished attachment. */
	start(ticketId: string, files: Iterable<File>, done?: (a: Attachment) => void) {
		for (const file of files) {
			const u: Upload = { key: next++, ticketId, name: file.name, size: file.size, sent: 0, status: 'uploading' };
			this.items.push(u);
			void this.run(u.key, file, done);
		}
	}

	/** Stops an upload in flight (dropping what was sent), or clears a failed one. */
	cancel(key: number) {
		this.stops.get(key)?.();
		this.items = this.items.filter((u) => u.key !== key);
	}

	private update(key: number, change: Partial<Upload>) {
		const u = this.items.find((x) => x.key === key);
		if (u) Object.assign(u, change);
	}

	private async run(key: number, file: File, done?: (a: Attachment) => void) {
		const ticketId = this.items.find((u) => u.key === key)!.ticketId;
		const xhrs = new Set<XMLHttpRequest>();
		let stopped = false;
		let id = '';
		this.stops.set(key, () => {
			stopped = true;
			xhrs.forEach((x) => x.abort());
			if (id) void api(`/api/v1/attachments/${id}`, { method: 'DELETE' }).catch(() => {});
		});
		try {
			const s = await api<Started>(`/api/v1/tickets/${ticketId}/attachments`, {
				method: 'POST',
				body: { filename: file.name, size: file.size, content_type: file.type }
			});
			id = s.attachment.id;
			if (stopped) throw new Stopped();
			const sent = new Array<number>(s.parts).fill(0);
			const etags = new Array<string>(s.parts);
			const urls = new Map<number, Promise<PartURL>>();

			// URLs are asked for a batch at a time, just before they are needed, so none expires while it waits.
			const urlFor = (n: number): Promise<PartURL> => {
				if (!urls.has(n)) {
					const numbers = Array.from({ length: Math.min(BATCH, s.parts - n + 1) }, (_, i) => n + i).filter((m) => !urls.has(m));
					const batch = api<{ parts: PartURL[] }>(`/api/v1/attachments/${id}/parts`, { method: 'POST', body: { numbers } });
					for (const m of numbers) {
						const p = batch.then((b) => b.parts.find((x) => x.number === m)!);
						p.catch(() => {}); // awaited, and reported, only by the part that needs it
						urls.set(m, p);
					}
				}
				return urls.get(n)!;
			};

			const putPart = (n: number, part: PartURL) =>
				new Promise<string>((resolve, reject) => {
					const x = new XMLHttpRequest();
					xhrs.add(x);
					x.open('PUT', part.url);
					x.upload.onprogress = (e) => {
						sent[n - 1] = e.loaded;
						this.update(key, { sent: sent.reduce((a, b) => a + b, 0) });
					};
					x.onload = () => {
						xhrs.delete(x);
						const etag = x.getResponseHeader('ETag');
						if (x.status === 200 && etag) return resolve(etag);
						if (x.status === 200) return reject(new Final('Storage did not return the part’s ETag: the bucket’s CORS rules must expose it'));
						let msg = `Part ${n} was refused (${x.status})`;
						try {
							msg = (JSON.parse(x.responseText) as { error?: string }).error ?? msg;
						} catch {
							/* not JSON: a bucket's XML */
						}
						// Trying again helps only when the link expired (403), or storage was busy or failing.
						reject(x.status >= 400 && x.status < 500 && ![403, 408, 429].includes(x.status) ? new Final(msg) : new Error(msg));
					};
					x.onerror = () => {
						xhrs.delete(x);
						reject(new Error('The connection dropped'));
					};
					x.onabort = () => {
						xhrs.delete(x);
						reject(new Stopped());
					};
					const start = (n - 1) * s.part_size;
					x.send(file.slice(start, start + part.size)); // no type, so no Content-Type header for a bucket's CORS to allow
				});

			let cursor = 1;
			const worker = async () => {
				while (cursor <= s.parts) {
					const n = cursor++;
					for (let attempt = 1; ; attempt++) {
						try {
							if (attempt > 1) urls.delete(n); // a fresh URL, in case the old one expired
							etags[n - 1] = await putPart(n, await urlFor(n));
							break;
						} catch (e) {
							if (e instanceof Stopped || e instanceof Final || attempt >= TRIES) throw e;
							sent[n - 1] = 0;
						}
					}
				}
			};
			await Promise.all(Array.from({ length: Math.min(PARALLEL, s.parts) }, worker));
			if (stopped) throw new Stopped();
			const a = await api<Attachment>(`/api/v1/attachments/${id}/complete`, {
				method: 'POST',
				body: { parts: etags.map((etag, i) => ({ number: i + 1, etag })) }
			});
			this.items = this.items.filter((u) => u.key !== key);
			done?.(a);
		} catch (e) {
			if (stopped || e instanceof Stopped) return;
			xhrs.forEach((x) => x.abort());
			if (id) void api(`/api/v1/attachments/${id}`, { method: 'DELETE' }).catch(() => {});
			const msg = e instanceof ApiError || e instanceof Error ? e.message : 'The upload failed';
			this.update(key, { status: 'failed', error: msg });
		} finally {
			this.stops.delete(key);
		}
	}
}

class Stopped extends Error {}
/** A failure trying again cannot fix. */
class Final extends Error {}

export const uploads = new Uploads();
