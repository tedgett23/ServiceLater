import { api } from './api';
import type { Ticket, TicketPage, User } from './types';
import { canRecordAny } from './types';

export type Filter = 'mine' | 'groups' | 'unassigned' | 'due' | 'open' | 'all';

/** The views of the queue a person is offered: staff work several, everyone else sees their own open or all. */
export function queueViews(staff: boolean): { id: Filter; label: string }[] {
	return staff
		? [
				{ id: 'mine', label: 'Mine' },
				{ id: 'groups', label: 'My groups' },
				{ id: 'unassigned', label: 'Unassigned' },
				{ id: 'due', label: 'Due' },
				{ id: 'open', label: 'Open' },
				{ id: 'all', label: 'All' }
			]
		: [
				{ id: 'open', label: 'Open' },
				{ id: 'all', label: 'All' }
			];
}

const STORE_KEY = 'sl.filter';

function remembered(): Filter | null {
	try {
		const v = localStorage.getItem(STORE_KEY);
		return v === 'mine' || v === 'groups' || v === 'unassigned' || v === 'due' || v === 'open' || v === 'all' ? v : null;
	} catch {
		return null; // storage blocked: fine, use the default
	}
}

/** The ticket queue: one shared list that detail views also update, so edits show up instantly. */
class Queue {
	items = $state<Ticket[]>([]);
	filter = $state<Filter>('open');
	search = $state('');
	loading = $state(false);
	hasMore = $state(false);
	error = $state('');
	private started = false;
	private generation = 0;

	/** Pick the starting filter once per session: agents start on their own work, everyone else on open tickets. */
	start(user: User) {
		if (this.started) return;
		this.started = true;
		const saved = remembered();
		const staff = canRecordAny(user, 'read');
		const allowed: Filter[] = staff ? ['mine', 'groups', 'unassigned', 'due', 'open', 'all'] : ['open', 'all'];
		this.filter = saved && allowed.includes(saved) ? saved : staff ? 'mine' : 'open';
		void this.load();
	}

	setFilter(f: Filter) {
		this.filter = f;
		try {
			localStorage.setItem(STORE_KEY, f);
		} catch {
			/* ignore */
		}
		void this.load();
	}

	private query(offset: number) {
		const p = new URLSearchParams({ limit: '30', offset: String(offset) });
		if (this.filter === 'mine') {
			p.set('assignee', 'me');
			p.set('open', 'true');
		} else if (this.filter === 'groups') {
			p.set('assignment_group', 'mine');
			p.set('open', 'true');
		} else if (this.filter === 'unassigned') {
			p.set('assignee', 'none');
			p.set('open', 'true');
		} else if (this.filter === 'due') {
			// Open work by SLA: breached first, then the soonest due.
			p.set('open', 'true');
			p.set('sort', 'due');
		} else if (this.filter === 'open') {
			p.set('open', 'true');
		}
		if (this.search.trim()) p.set('q', this.search.trim());
		return `/api/v1/tickets?${p}`;
	}

	async load() {
		const gen = ++this.generation;
		this.loading = true;
		this.error = '';
		try {
			const page = await api<TicketPage>(this.query(0));
			if (gen !== this.generation) return; // a newer request superseded this one
			this.items = page.items;
			this.hasMore = page.has_more;
		} catch (e) {
			if (gen === this.generation) this.error = e instanceof Error ? e.message : 'Could not load tickets';
		} finally {
			if (gen === this.generation) this.loading = false;
		}
	}

	async more() {
		if (this.loading || !this.hasMore) return;
		const gen = this.generation;
		this.loading = true;
		try {
			const page = await api<TicketPage>(this.query(this.items.length));
			if (gen !== this.generation) return;
			const seen = new Set(this.items.map((t) => t.id));
			this.items = [...this.items, ...page.items.filter((t) => !seen.has(t.id))];
			this.hasMore = page.has_more;
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Could not load more tickets';
		} finally {
			if (gen === this.generation) this.loading = false;
		}
	}

	private refreshTimer: ReturnType<typeof setTimeout> | undefined;

	/** Refresh soon, once, however many change events arrive in a burst. */
	scheduleRefresh() {
		clearTimeout(this.refreshTimer);
		this.refreshTimer = setTimeout(() => void this.refresh(), 250);
	}

	/**
	 * Re-fetch the top of the list without the loading state or resetting scroll. Covers as many rows as are
	 * already loaded (up to 100), so live changes appear in place.
	 */
	async refresh() {
		if (!this.started) return;
		const gen = this.generation;
		const want = Math.min(100, Math.max(30, this.items.length));
		try {
			const p = new URLSearchParams(this.query(0).split('?')[1]);
			p.set('limit', String(want));
			const page = await api<TicketPage>(`/api/v1/tickets?${p}`);
			if (gen !== this.generation) return; // the filter changed meanwhile
			this.items = page.items;
			this.hasMore = page.has_more;
		} catch {
			/* a failed background refresh is not worth interrupting anyone for */
		}
	}

	/** Replace a ticket in place if listed. */
	upsert(t: Ticket) {
		const i = this.items.findIndex((x) => x.id === t.id);
		if (i >= 0) this.items[i] = withSlaSummary(t, this.items[i]);
	}

	patch(id: string, changes: Partial<Ticket>) {
		const i = this.items.findIndex((x) => x.id === id);
		if (i >= 0) this.items[i] = { ...this.items[i], ...changes };
	}

	prepend(t: Ticket) {
		this.items = [withSlaSummary(t), ...this.items.filter((x) => x.id !== t.id)];
	}
}

/**
 * A single ticket (from its page) carries its clocks, not the list's summary of them: work the summary out the way
 * the list query does (the soonest due time of the running clocks not yet breached, and whether any breached), so a
 * row keeps its SLA after an edit. Without clocks, keep what the row had.
 */
function withSlaSummary(t: Ticket, was?: Ticket): Ticket {
	if (!t.slas) return { ...t, sla_due_at: t.sla_due_at ?? was?.sla_due_at, sla_breached: t.sla_breached ?? was?.sla_breached };
	const due = t.slas
		.filter((c) => c.stage === 'running' && !c.breached_at && c.due_at)
		.map((c) => c.due_at!)
		.sort()[0];
	return { ...t, sla_due_at: due, sla_breached: t.slas.some((c) => !!c.breached_at) };
}

export const queue = new Queue();
