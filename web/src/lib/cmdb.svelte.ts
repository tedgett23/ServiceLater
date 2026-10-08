import { api } from './api';
import type { CIClass, CIStatus, CMDBModel } from './types';

/**
 * What configuration items can be: the classes, relationship types and statuses, as the installed apps define them.
 * Loaded on first use and again when an app changes (definitions.changed).
 */
class CMDB {
	model = $state<CMDBModel | null>(null);
	private loading: Promise<void> | null = null;

	ensure(): Promise<void> {
		if (!this.loading) {
			this.loading = api<CMDBModel>('/api/v1/cmdb/model')
				.then((m) => {
					this.model = m;
				})
				.catch(() => {
					this.loading = null; // try again next time
				});
		}
		return this.loading;
	}

	/** An app changed: load again if it was ever loaded. */
	async reload() {
		if (!this.model) return;
		this.loading = null;
		await this.ensure();
	}

	classOf(name: string): CIClass | undefined {
		return this.model?.classes.find((c) => c.name === name);
	}

	/**
	 * The classes in tree order, each after the one it extends, with how deep it is: for pickers that show the
	 * hierarchy.
	 */
	tree(): { cls: CIClass; depth: number }[] {
		const classes = this.model?.classes ?? [];
		const known = new Set(classes.map((c) => c.name));
		const out: { cls: CIClass; depth: number }[] = [];
		const walk = (parent: string | undefined, depth: number) => {
			for (const c of classes) {
				const p = c.extends && known.has(c.extends) ? c.extends : undefined;
				if (p === parent) {
					out.push({ cls: c, depth });
					walk(c.name, depth + 1);
				}
			}
		};
		walk(undefined, 0);
		return out;
	}

	statusLabel(s: CIStatus): string {
		return this.model?.statuses.find((x) => x.value === s)?.label ?? s;
	}
}

export const cmdb = new CMDB();
