<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import Status from '$lib/components/Status.svelte';
	import { realtime } from '$lib/realtime.svelte';
	import { prefixOf, type CalendarEntry } from '$lib/types';

	type View = 'week' | 'month';

	const view = $derived<View>(page.url.searchParams.get('view') === 'month' ? 'month' : 'week');
	// The day the range is built around, in the browser's time zone; today when the address does not say.
	const anchor = $derived.by(() => {
		const raw = page.url.searchParams.get('date');
		const d = raw && /^\d{4}-\d{2}-\d{2}$/.test(raw) ? new Date(`${raw}T00:00`) : new Date();
		return Number.isNaN(d.getTime()) ? startOfDay(new Date()) : startOfDay(d);
	});
	const range = $derived.by(() => {
		if (view === 'month') {
			const from = new Date(anchor.getFullYear(), anchor.getMonth(), 1);
			return { from, to: new Date(anchor.getFullYear(), anchor.getMonth() + 1, 1) };
		}
		const from = new Date(anchor);
		from.setDate(from.getDate() - ((from.getDay() + 6) % 7)); // weeks start on Monday
		const to = new Date(from);
		to.setDate(to.getDate() + 7);
		return { from, to };
	});

	let items = $state<CalendarEntry[]>([]);
	let truncated = $state(false);
	let loading = $state(true);
	let error = $state('');

	function startOfDay(d: Date) {
		return new Date(d.getFullYear(), d.getMonth(), d.getDate());
	}
	const iso = (d: Date) =>
		`${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;

	let generation = 0;
	async function load() {
		const mine = ++generation;
		const { from, to } = range;
		error = '';
		try {
			const q = new URLSearchParams({ from: from.toISOString(), to: to.toISOString() });
			const res = await api<{ items: CalendarEntry[]; truncated: boolean }>(`/api/v1/calendar?${q}`);
			if (mine !== generation) return; // the range changed while this was loading
			items = res.items;
			truncated = res.truncated;
		} catch (e) {
			if (mine === generation) error = e instanceof Error ? e.message : 'Could not load the calendar';
		} finally {
			if (mine === generation) loading = false;
		}
	}

	$effect(() => {
		void range;
		loading = true;
		void load();
	});

	onMount(() =>
		realtime.on((e) => {
			// A change was planned, moved or rescheduled somewhere: look again.
			if (e.type === 'ticket.changed' || e.type === 'resync') void load();
		})
	);

	function go(next: { view?: View; date?: Date }) {
		const q = new URLSearchParams();
		const v = next.view ?? view;
		if (v === 'month') q.set('view', 'month');
		if (next.date) q.set('date', iso(next.date));
		void goto(`/calendar${q.size ? `?${q}` : ''}`, { keepFocus: true, noScroll: true });
	}
	function step(dir: -1 | 1) {
		const d = new Date(anchor);
		if (view === 'month') d.setMonth(d.getMonth() + dir, 1);
		else d.setDate(d.getDate() + 7 * dir);
		go({ date: d });
	}

	const title = $derived.by(() => {
		const { from, to } = range;
		if (view === 'month') return from.toLocaleDateString(undefined, { month: 'long', year: 'numeric' });
		const last = new Date(to.getTime() - 1);
		const sameMonth = from.getMonth() === last.getMonth();
		const a = from.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
		const b = last.toLocaleDateString(undefined, sameMonth ? { day: 'numeric' } : { month: 'short', day: 'numeric' });
		return `${a} – ${b}, ${last.getFullYear()}`;
	});

	// Grouped by the day each window starts on; one that started before the range is listed on its first day.
	const days = $derived.by(() => {
		const groups: { day: Date; entries: CalendarEntry[] }[] = [];
		for (const e of items) {
			let day = startOfDay(new Date(e.planned_start));
			if (day < range.from) day = range.from;
			const last = groups.at(-1);
			if (last && last.day.getTime() === day.getTime()) last.entries.push(e);
			else groups.push({ day, entries: [e] });
		}
		return groups;
	});
	const conflicts = $derived(items.filter((e) => e.overlaps.length > 0).length);

	const dayLabel = (d: Date) => d.toLocaleDateString(undefined, { weekday: 'long', month: 'short', day: 'numeric' });
	const time = (d: Date) => d.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' });
	/** "9:00 PM – Fri 1:00 AM": the end gets its day when it is on another one. */
	function windowText(e: CalendarEntry) {
		const s = new Date(e.planned_start);
		const end = new Date(e.planned_end);
		const sameDay = startOfDay(s).getTime() === startOfDay(end).getTime();
		// Within the week the weekday is enough; a longer window names the date it ends on.
		const days = (startOfDay(end).getTime() - startOfDay(s).getTime()) / 86_400_000;
		const on = days < 7 ? { weekday: 'short' } : { month: 'short', day: 'numeric' };
		const endText = sameDay ? time(end) : `${end.toLocaleDateString(undefined, on as Intl.DateTimeFormatOptions)} ${time(end)}`;
		return `${time(s)} – ${endText}`;
	}
	const kindLabel = { standard: 'Standard', normal: 'Normal', emergency: 'Emergency' } as const;
	const riskLabel = { '': '', low: 'Low risk', medium: 'Medium risk', high: 'High risk' } as const;
</script>

<svelte:head><title>Change calendar – ServiceLater</title></svelte:head>

<div class="page">
	<header class="top">
		<h1>Change calendar</h1>
		<div class="controls">
			<div class="seg" role="group" aria-label="Range">
				<button class="btn" aria-pressed={view === 'week'} onclick={() => go({ view: 'week', date: anchor })}>Week</button>
				<button class="btn" aria-pressed={view === 'month'} onclick={() => go({ view: 'month', date: anchor })}>Month</button>
			</div>
			<div class="nav">
				<button class="btn" onclick={() => step(-1)} aria-label="Previous {view}">
					<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
				</button>
				<button class="btn" onclick={() => go({})}>Today</button>
				<button class="btn" onclick={() => step(1)} aria-label="Next {view}">
					<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M2 1 6.5 6 2 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
				</button>
			</div>
		</div>
	</header>
	<p class="range" aria-live="polite">
		<strong>{title}</strong>
		{#if !loading && items.length}
			· {items.length} {items.length === 1 ? 'change' : 'changes'}{#if conflicts}{' '}· <span class="warn">{conflicts} overlapping</span>{/if}
		{/if}
	</p>

	{#if error}
		<p class="msg" role="alert">{error}. <button class="link" onclick={load}>Try again</button></p>
	{:else if loading && !items.length}
		<p class="msg">Loading…</p>
	{:else if !items.length}
		<p class="msg">Nothing is planned {view === 'week' ? 'this week' : 'this month'}.</p>
	{/if}
	{#if truncated}<p class="msg">Showing the first 500. Narrow the range to see the rest.</p>{/if}

	{#each days as d (d.day.getTime())}
		<section class="day" aria-label={dayLabel(d.day)}>
			<h2>{dayLabel(d.day)}</h2>
			<ul>
				{#each d.entries as e (e.id)}
					<li class:conflict={e.overlaps.length > 0} class:done={e.state_category === 'done' || e.state_category === 'closed'}>
						<span class="when">{windowText(e)}</span>
						<a class="what" href="/tickets/{e.reference}">
							<span class="ref"><span class="prefix">{prefixOf(e)}</span>{e.number}</span>
							<span class="title">{e.title}</span>
						</a>
						<span class="facts">
							<Status status={e.status} category={e.state_category} type={e.type} />
							<span>{kindLabel[e.change_type]}</span>
							{#if e.risk}<span>{riskLabel[e.risk]}</span>{/if}
							{#if e.assignee_name}<span>{e.assignee_name}</span>{/if}
						</span>
						{#if e.overlaps.length}
							<p class="overlap">Overlaps {e.overlaps.join(', ')}</p>
						{/if}
					</li>
				{/each}
			</ul>
		</section>
	{/each}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 56rem;
	}
	.top {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
	}
	h1 {
		font: 800 1.9rem/1.15 var(--font-display);
	}
	.controls,
	.nav,
	.seg {
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem;
	}
	.controls {
		gap: 0.75rem;
	}
	.seg .btn[aria-pressed='true'] {
		border-color: var(--blue);
		box-shadow: inset 0 0 0 1px var(--blue);
		font-weight: 700;
	}
	.range {
		margin: 0.5rem 0 1rem;
		color: var(--ink-2);
	}
	.range strong {
		color: var(--ink);
	}
	.warn {
		color: var(--ink);
		background: var(--marker-soft);
		padding: 0 0.3rem;
		font-weight: 600;
	}
	.day {
		margin-top: 1.25rem;
	}
	h2 {
		font: 700 1.2rem/1.2 var(--font-display);
		padding-bottom: 0.4rem;
		border-bottom: 1px solid var(--rule);
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	li {
		display: grid;
		grid-template-columns: 11rem minmax(0, 1fr);
		grid-template-areas:
			'when what'
			'.    facts'
			'.    overlap';
		column-gap: 1rem;
		row-gap: 0.25rem;
		padding: 0.7rem 0 0.7rem 0.6rem;
		border-bottom: 1px solid var(--rule);
		border-left: 3px solid transparent;
	}
	/* Yellow means look here: two changes in one window. */
	li.conflict {
		border-left-color: var(--marker);
		background: var(--marker-soft);
	}
	li.done .what {
		color: var(--ink-2);
	}
	.when {
		grid-area: when;
		font-variant-numeric: tabular-nums;
		font-weight: 600;
		font-size: 0.9rem;
		padding-top: 0.15rem;
	}
	.what {
		grid-area: what;
		display: flex;
		align-items: baseline;
		gap: 0.6rem;
		min-width: 0;
		color: var(--ink);
		text-decoration: none;
	}
	.what:hover .title {
		text-decoration: underline;
	}
	.ref {
		font: 700 1.05rem/1 var(--font-display);
		color: var(--ink-2);
		white-space: nowrap;
	}
	.prefix {
		font-size: 0.7em;
		letter-spacing: 0.06em;
		margin-right: 0.1em;
	}
	.title {
		font-weight: 600;
		overflow-wrap: anywhere;
	}
	.facts {
		grid-area: facts;
		display: flex;
		flex-wrap: wrap;
		gap: 0.25rem 0.9rem;
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.overlap {
		grid-area: overlap;
		font-size: 0.85rem;
		font-weight: 600;
	}
	.msg {
		padding: 0.5rem 0;
		color: var(--ink-2);
	}
	.link {
		border: 0;
		background: none;
		padding: 0;
		text-decoration: underline;
		cursor: pointer;
	}
	@media (max-width: 40rem) {
		li {
			grid-template-columns: minmax(0, 1fr);
			grid-template-areas: 'when' 'what' 'facts' 'overlap';
		}
	}
</style>
