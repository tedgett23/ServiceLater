import { api, hooks } from './api';

export type Environment = 'production' | 'uat' | 'development' | 'other';

export interface Maintenance {
	operation: string;
	status: string;
	since: string;
	by: string;
}

interface InstanceState {
	name: string;
	environment: Environment;
	maintenance: Maintenance | null;
}

export const environmentLabel: Record<Environment, string> = {
	production: 'Production',
	uat: 'UAT',
	development: 'Development',
	other: 'Test'
};

/**
 * What this instance is (its name and environment: anything but production shows a banner on every page) and whether
 * it is in maintenance (a restore waiting or running). While it is, every API call but GET /instance answers 503; the
 * layout shows a maintenance screen that checks every few seconds and, when the restore is over, goes to sign-in
 * (a restore ends every session).
 */
class Instance {
	name = $state('');
	environment = $state<Environment>('production');
	maintenance = $state<Maintenance | null>(null);
	private timer: ReturnType<typeof setTimeout> | null = null;

	async load() {
		try {
			this.take(await api<InstanceState>('/api/v1/instance'));
		} catch {
			/* the banner waits for the next load */
		}
	}

	private take(s: InstanceState) {
		const was = this.maintenance;
		this.name = s.name;
		this.environment = s.environment;
		this.maintenance = s.maintenance;
		if (s.maintenance) this.poll();
		else if (was) location.replace('/login');
	}

	/** Called when any request answers that the instance is in maintenance. */
	enter() {
		if (!this.maintenance) void this.load();
	}

	private poll() {
		if (this.timer) return;
		this.timer = setTimeout(async () => {
			this.timer = null;
			await this.load();
		}, 5000);
	}
}

export const instance = new Instance();
hooks.onMaintenance = () => instance.enter();
