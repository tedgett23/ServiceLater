<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, download } from '$lib/api';
	import BreakdownBars from '$lib/charts/BreakdownBars.svelte';
	import TrendChart from '$lib/charts/TrendChart.svelte';
	import { span, when } from '$lib/format';
	import { realtime } from '$lib/realtime.svelte';
	import type { Group, SlaReport } from '$lib/types';

	/**
	 * The SLA report: how records met their SLAs in a period, in each SLA's own business hours, and what is at risk now.
	 * The filters live in the address, so a view can be shared. Everything is worked out by the server, as the reader.
	 */
	const periods = [
		{ id: '7d', label: '7 days' },
		{ id: '30d', label: '30 days' },
		{ id: '90d', label: '90 days' },
		{ id: '12m', label: '12 months' }
	];
	const filterNames = ['period', 'type', 'sla', 'group', 'priority'] as const;
	const query = $derived(
		new URLSearchParams(
			filterNames.flatMap((n) => {
				const v = page.url.searchParams.get(n);
				return v ? [[n, v]] : [];
			})
		).toString()
	);
	const period = $derived(page.url.searchParams.get('period') ?? '30d');

	let report = $state<SlaReport | null>(null);
	let groups = $state<Group[]>([]);
	let error = $state('');
	let views = $state<Record<string, 'chart' | 'table'>>({});
	const viewOf = (k: string) => views[k] ?? 'chart';
	const toggle = (k: string) => (views[k] = viewOf(k) === 'chart' ? 'table' : 'chart');

	async function load() {
		try {
			report = await api<SlaReport>(`/api/v1/reports/slas${query ? `?${query}` : ''}`);
			error = '';
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the report';
		}
	}
	$effect(() => {
		void query;
		void load();
	});
	$effect(() => {
		void api<{ items: Group[] }>('/api/v1/groups')
			.then((g) => (groups = g.items))
			.catch(() => {});
	});
	// Live: what is at risk moves as records change and as time passes.
	$effect(() => {
		let soon: ReturnType<typeof setTimeout> | undefined;
		const off = realtime.on((e) => {
			if (e.type === 'ticket.changed' || e.type === 'resync') {
				clearTimeout(soon);
				soon = setTimeout(load, 2000);
			}
		});
		const tick = setInterval(load, 60_000);
		return () => {
			off();
			clearTimeout(soon);
			clearInterval(tick);
		};
	});

	function set(name: (typeof filterNames)[number], value: string) {
		const p = new URLSearchParams(page.url.searchParams);
		if (value) p.set(name, value);
		else p.delete(name);
		void goto(`?${p}`, { replaceState: true, keepFocus: true, noScroll: true });
	}
	const filtered = $derived(filterNames.slice(1).filter((n) => page.url.searchParams.get(n)).length);

	const delta = $derived.by(() => {
		const s = report?.summary;
		if (s?.met_pct == null || s.prev_met_pct == null) return null;
		return Math.round((s.met_pct - s.prev_met_pct) * 10) / 10;
	});
	const periodLabel = $derived(periods.find((p) => p.id === period)?.label ?? period);
	const medians = $derived((report?.slas ?? []).filter((s) => s.median_ms != null).slice(0, 3));
	let exporting = $state(false);
	async function exportCSV() {
		exporting = true;
		try {
			await download(`/api/v1/reports/slas/export${query ? `?${query}` : ''}`, `sla-report-${period}.csv`);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not export';
		} finally {
			exporting = false;
		}
	}
</script>

<svelte:head><title>SLA report – ServiceLater</title></svelte:head>

<div class="page">
	<h1>SLA report</h1>
	<p class="lede">How records met their SLAs, each measured in its own working hours.</p>

	<div class="filters" role="group" aria-label="Filters">
		<div class="segmented" role="group" aria-label="Period">
			{#each periods as p (p.id)}
				<button type="button" aria-pressed={period === p.id} onclick={() => set('period', p.id === '30d' ? '' : p.id)}>{p.label}</button>
			{/each}
		</div>
		<select class="select" aria-label="Record type" value={page.url.searchParams.get('type') ?? ''} onchange={(e) => set('type', e.currentTarget.value)}>
			<option value="">All types</option>
			{#each report?.options.types ?? [] as t (t.key)}<option value={t.key}>{t.label}</option>{/each}
		</select>
		<select class="select" aria-label="SLA" value={page.url.searchParams.get('sla') ?? ''} onchange={(e) => set('sla', e.currentTarget.value)}>
			<option value="">All SLAs</option>
			{#each report?.options.slas ?? [] as s (s.key)}<option value={s.key}>{s.label}</option>{/each}
		</select>
		<select class="select" aria-label="Assignment group" value={page.url.searchParams.get('group') ?? ''} onchange={(e) => set('group', e.currentTarget.value)}>
			<option value="">All groups</option>
			<option value="none">No group</option>
			{#each groups as g (g.id)}<option value={g.id}>{g.name}</option>{/each}
		</select>
		<select class="select" aria-label="Priority" value={page.url.searchParams.get('priority') ?? ''} onchange={(e) => set('priority', e.currentTarget.value)}>
			<option value="">All priorities</option>
			{#each report?.options.priorities ?? [] as p (p.key)}<option value={p.key}>{p.label}</option>{/each}
		</select>
		{#if filtered}
			<button class="btn quiet" onclick={() => void goto(period === '30d' ? '?' : `?period=${period}`, { replaceState: true, keepFocus: true, noScroll: true })}
				>Clear filters</button
			>
		{/if}
		<span class="grow"></span>
		<button class="btn" onclick={exportCSV} disabled={exporting}>{exporting ? 'Exporting…' : 'Export CSV'}</button>
	</div>

	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if report}
		{@const s = report.summary}
		<section class="tiles" aria-label="Summary">
			<div class="tile">
				<h2>Met</h2>
				<p class="value">{s.met_pct == null ? '–' : `${s.met_pct}%`}</p>
				<p class="detail">
					{#if delta !== null}
						<span class={delta >= 0 ? 'up' : 'down'}>{delta >= 0 ? '▲' : '▼'} {Math.abs(delta)} pts</span> on the {periodLabel} before
					{:else}
						{s.ended ? `of ${s.ended} that ended` : 'Nothing ended yet'}
					{/if}
				</p>
			</div>
			<div class="tile">
				<h2>Breached</h2>
				<p class="value">{s.breached}</p>
				<p class="detail">of {s.ended} {s.ended === 1 ? 'clock' : 'clocks'} that ended</p>
			</div>
			<div class="tile">
				<h2>At risk now</h2>
				<p class="value">{s.at_risk}</p>
				<p class="detail">of {s.running} running, past 75% of their target</p>
			</div>
			{#each medians as m (m.key)}
				<div class="tile">
					<h2>{m.label}: median time</h2>
					<p class="value">{span(m.median_ms!)}</p>
					<p class="detail">working time · median target {span(m.median_target_ms ?? 0)}</p>
				</div>
			{/each}
		</section>
		{#if report.partial}<p class="hint">More clocks are running than the report reads at once: running counts are partial.</p>{/if}

		<div class="grid">
			<section class="card" aria-labelledby="trend-h">
				<header>
					<h2 id="trend-h">Met, by {report.bucket}</h2>
					<button class="link" onclick={() => toggle('trend')}>{viewOf('trend') === 'chart' ? 'Table' : 'Chart'}</button>
				</header>
				<p class="sub">Share of clocks that ended within their target</p>
				{#if report.series.length}
					<TrendChart series={report.series} points={report.trend} bucket={report.bucket} view={viewOf('trend')} label="Share met by {report.bucket}, per SLA" />
				{:else}
					<p class="none">No clocks ended in this period.</p>
				{/if}
			</section>
			<section class="card" aria-labelledby="groups-h">
				<header>
					<h2 id="groups-h">By assignment group</h2>
					<button class="link" onclick={() => toggle('groups')}>{viewOf('groups') === 'chart' ? 'Table' : 'Chart'}</button>
				</header>
				<p class="sub">Clocks that ended in the period, and those running now</p>
				<BreakdownBars rows={report.groups} view={viewOf('groups')} label="Clocks by assignment group" of="Group" />
			</section>

			<section class="card" aria-labelledby="risk-h">
				<header><h2 id="risk-h">At risk now</h2></header>
				<p class="sub">Running clocks closest to their target, live</p>
				{#if report.at_risk.length}
					<div class="table-wrap">
						<table>
							<thead><tr><th scope="col">Record</th><th scope="col">SLA</th><th scope="col">Group</th><th scope="col">Used</th><th scope="col">Due</th></tr></thead>
							<tbody>
								{#each report.at_risk as c (c.reference + c.sla)}
									<tr>
										<td class="rec"><a href={c.link}><span class="ref">{c.reference}</span> {c.title}</a></td>
										<td>{c.sla}</td>
										<td>{c.group}</td>
										<td class="used" title="{span(c.used_ms)} of {span(c.target_ms)} in {c.calendar}">
											<span class="meter" aria-hidden="true"><span class:close={c.used_pct >= 90} style="width: {Math.min(100, c.used_pct)}%"></span></span>{c.used_pct}%
										</td>
										<td>{c.due_at ? when(c.due_at) : '–'}</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				{:else}
					<p class="none">Nothing is past 75% of its target.</p>
				{/if}
			</section>
			<section class="card" aria-labelledby="breach-h">
				<header><h2 id="breach-h">Recent breaches</h2></header>
				<p class="sub">How far past their target, in working time</p>
				{#if report.breaches.length}
					<div class="table-wrap">
						<table>
							<thead><tr><th scope="col">Record</th><th scope="col">SLA</th><th scope="col">Group</th><th scope="col">Over by</th></tr></thead>
							<tbody>
								{#each report.breaches as c (c.reference + c.sla)}
									<tr>
										<td class="rec"><a href={c.link}><span class="ref">{c.reference}</span> {c.title}</a></td>
										<td>{c.sla}</td>
										<td>{c.group}</td>
										<td title="Breached {c.breached_at ? when(c.breached_at) : ''}, counted in {c.calendar}">{span(c.over_ms)}</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				{:else}
					<p class="none">No breaches in this period.</p>
				{/if}
			</section>

			<section class="card" aria-labelledby="prio-h">
				<header>
					<h2 id="prio-h">By priority</h2>
					<button class="link" onclick={() => toggle('prio')}>{viewOf('prio') === 'chart' ? 'Table' : 'Chart'}</button>
				</header>
				<BreakdownBars rows={report.priorities} view={viewOf('prio')} label="Clocks by priority" of="Priority" />
			</section>
			{#if report.types.length > 1}
				<section class="card" aria-labelledby="type-h">
					<header>
						<h2 id="type-h">By type</h2>
						<button class="link" onclick={() => toggle('type')}>{viewOf('type') === 'chart' ? 'Table' : 'Chart'}</button>
					</header>
					<BreakdownBars rows={report.types} view={viewOf('type')} label="Clocks by record type" of="Type" />
				</section>
			{/if}
		</div>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.15rem 0 0.9rem;
		color: var(--ink-2);
	}
	.filters {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.5rem;
		margin-bottom: 1rem;
	}
	.filters .select {
		width: auto;
		max-width: 14rem;
	}
	.grow {
		flex: 1;
	}
	.tiles {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr));
		gap: 0.75rem;
		margin-bottom: 0.75rem;
	}
	.tile,
	.card {
		background: var(--ground);
		border: 1px solid var(--rule);
		border-radius: 10px;
		min-width: 0;
	}
	.tile {
		padding: 0.75rem 0.9rem;
	}
	.tile h2 {
		font-size: 0.8125rem;
		font-weight: 600;
		color: var(--ink-2);
	}
	.value {
		font: 650 1.625rem/1.2 var(--font-display);
		margin: 0.25rem 0 0.1rem;
		font-variant-numeric: tabular-nums;
	}
	.detail {
		font-size: 0.75rem;
		color: var(--ink-2);
	}
	.up {
		color: var(--green);
		font-weight: 600;
	}
	.down {
		color: var(--red);
		font-weight: 600;
	}
	.grid {
		display: grid;
		grid-template-columns: minmax(0, 1.35fr) minmax(0, 1fr);
		gap: 0.75rem;
	}
	.card {
		padding: 0.85rem 1rem;
	}
	.card header {
		display: flex;
		justify-content: space-between;
		align-items: baseline;
		gap: 0.5rem;
	}
	.card h2 {
		font: 650 0.9375rem/1.3 var(--font-display);
	}
	.sub {
		font-size: 0.78rem;
		color: var(--ink-2);
		margin: 0.1rem 0 0.6rem;
	}
	.link {
		background: none;
		border: 0;
		padding: 0;
		color: var(--blue);
		font-weight: 600;
		font-size: 0.78rem;
		cursor: pointer;
	}
	.none,
	.hint {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.hint {
		margin-bottom: 0.75rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
		margin-bottom: 0.75rem;
	}
	.table-wrap {
		overflow-x: auto;
	}
	table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.8125rem;
		font-variant-numeric: tabular-nums;
	}
	th,
	td {
		text-align: left;
		padding: 0.45rem 0.4rem;
		border-bottom: 1px solid var(--rule);
		white-space: nowrap;
	}
	th {
		color: var(--ink-2);
		font-weight: 600;
		font-size: 0.75rem;
	}
	.rec {
		white-space: normal;
		min-width: 12rem;
	}
	.rec a {
		color: var(--ink);
		text-decoration: none;
	}
	.rec a:hover {
		text-decoration: underline;
	}
	.ref {
		font: 600 0.6875rem var(--font-mono);
		background: var(--marker);
		color: var(--on-marker);
		padding: 0.05rem 0.3rem;
		border-radius: 4px;
		margin-right: 0.25rem;
	}
	.used {
		display: flex;
		align-items: center;
		gap: 0.4rem;
	}
	.meter {
		display: inline-block;
		width: 4.5rem;
		height: 6px;
		border-radius: 3px;
		background: var(--hover);
		overflow: hidden;
	}
	.meter span {
		display: block;
		height: 100%;
		border-radius: 3px;
		background: var(--series-1);
	}
	.meter span.close {
		background: var(--breached);
	}
	@media (max-width: 64rem) {
		.grid {
			grid-template-columns: minmax(0, 1fr);
		}
	}
	@media (max-width: 40rem) {
		.page {
			padding: 0.85rem 1rem 2rem;
		}
		.filters .select {
			flex: 1 1 9rem;
			max-width: none;
		}
		.tiles {
			grid-template-columns: 1fr 1fr;
		}
	}
</style>
