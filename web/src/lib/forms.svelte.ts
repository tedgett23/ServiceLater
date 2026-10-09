import { api } from './api';
import type { FormDef } from './types';

/**
 * The form of each record type, as the installed apps define it: which fields there are, which go on the form for
 * filing one and in which sections on the record. Whether a field is shown, required or writable right now is the
 * server's to say (a ticket's field_states, or POST /forms/{type}/states while filing); the UI keeps no copy of the
 * rules.
 */
class Forms {
	/** By type, or type/item for a request ordered from a catalog item, whose questions are its form. */
	byType = $state<Record<string, FormDef>>({});
	private loading = new Map<string, Promise<void>>();

	/** Loads a form once; later calls wait for the same request. */
	ensure(type: string, item = ''): Promise<void> {
		const key = item ? `${type}/${item}` : type;
		let p = this.loading.get(key);
		if (!p) {
			p = this.load(key, type, item);
			this.loading.set(key, p);
		}
		return p;
	}

	private async load(key: string, type: string, item: string) {
		try {
			const f = await api<FormDef>(`/api/v1/forms/${type}${item ? `?${new URLSearchParams({ item })}` : ''}`);
			this.byType = { ...this.byType, [key]: f };
		} catch {
			this.loading.delete(key); // try again next time
		}
	}

	/** An app changed: load again every form already shown. */
	async reload() {
		const keys = Object.keys(this.byType);
		this.loading.clear();
		await Promise.all(keys.map((k) => this.ensure(...(k.split('/') as [string, string?]))));
	}

	of(type: string, item = ''): FormDef | undefined {
		return this.byType[item ? `${type}/${item}` : type];
	}
}

export const forms = new Forms();
