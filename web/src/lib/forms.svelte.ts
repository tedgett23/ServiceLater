import { api } from './api';
import type { FormDef } from './types';

/**
 * The form of each record type, as the installed apps define it: which fields there are, which go on the form for
 * filing one and in which sections on the record. Whether a field is shown, required or writable right now is the
 * server's to say (a ticket's field_states, or POST /forms/{type}/states while filing); the UI keeps no copy of the
 * rules.
 */
class Forms {
	byType = $state<Record<string, FormDef>>({});
	private loading = new Map<string, Promise<void>>();

	/** Loads a type's form once; later calls wait for the same request. */
	ensure(type: string): Promise<void> {
		let p = this.loading.get(type);
		if (!p) {
			p = this.load(type);
			this.loading.set(type, p);
		}
		return p;
	}

	private async load(type: string) {
		try {
			const f = await api<FormDef>(`/api/v1/forms/${type}`);
			this.byType = { ...this.byType, [type]: f };
		} catch {
			this.loading.delete(type); // try again next time
		}
	}

	/** An app changed: load again every form already shown. */
	async reload() {
		const types = Object.keys(this.byType);
		this.loading.clear();
		await Promise.all(types.map((t) => this.ensure(t)));
	}

	of(type: string): FormDef | undefined {
		return this.byType[type];
	}
}

export const forms = new Forms();
