<script lang="ts">
	import { scaleLinear } from 'd3-scale';
	import { max } from 'd3-array';

	/**
	 * Clocks by something (assignment group, priority, type): one bar each, met then breached then still running, with
	 * the share met at the end and the counts written under it, so nothing depends on telling the colours apart. Hover
	 * or focus a row for its figures; view = 'table' shows the same as a table.
	 */
	interface Row {
		key: string;
		label: string;
		met: number;
		breached: number;
		running: number;
		met_pct: number | null;
	}
	let { rows, view = 'chart', label, of = 'Group' }: { rows: Row[]; view?: 'chart' | 'table'; label: string; of?: string } = $props();

	const parts = [
		{ key: 'met', label: 'Met', color: 'var(--series-1)' },
		{ key: 'breached', label: 'Breached', color: 'var(--breached)' },
		{ key: 'running', label: 'Still running', color: 'var(--running)' }
	] as const;

	let width = $state(0);
	const labelW = $derived(width < 420 ? 0 : 128);
	const endW = 42;
	const rowH = 44;
	const x = $derived(
		scaleLinear()
			.domain([0, max(rows, (r) => r.met + r.breached + r.running) || 1])
			.range([0, Math.max(1, width - labelW - endW)])
	);
	/** The bar's last piece has its far end rounded; the baseline end stays square. */
	function piece(x0: number, w: number, h: number, round: boolean) {
		const r = Math.min(4, w, h / 2);
		if (!round || r <= 0) return `M${x0},0h${w}v${h}h${-w}z`;
		return `M${x0},0h${w - r}a${r},${r} 0 0 1 ${r},${r}v${h - 2 * r}a${r},${r} 0 0 1 ${-r},${r}h${-(w - r)}z`;
	}
	function segments(r: Row) {
		const out: { d: string; color: string }[] = [];
		const present = parts.filter((p) => r[p.key] > 0);
		let at = labelW;
		present.forEach((p, i) => {
			const full = x(r[p.key]);
			const last = i === present.length - 1;
			const w = Math.max(1, full - (last ? 0 : 2)); // a 2px gap between pieces
			out.push({ d: piece(at, w, 18, last), color: p.color });
			at += full;
		});
		return { out, end: at };
	}
	let hover = $state<number | null>(null);
	const counts = (r: Row) => `${r.met} met · ${r.breached} breached · ${r.running} running`;
</script>

{#if view === 'table'}
	<div class="table-wrap">
		<table>
			<caption class="sr-only">{label}</caption>
			<thead>
				<tr><th scope="col">{of}</th><th scope="col">Met</th><th scope="col">Breached</th><th scope="col">Still running</th><th scope="col">Share met</th></tr>
			</thead>
			<tbody>
				{#each rows as r (r.key)}
					<tr><th scope="row">{r.label}</th><td>{r.met}</td><td>{r.breached}</td><td>{r.running}</td><td>{r.met_pct == null ? '–' : `${r.met_pct}%`}</td></tr>
				{/each}
			</tbody>
		</table>
	</div>
{:else}
	<div class="legend" aria-hidden="true">
		{#each parts as p (p.key)}<span><i style="background: {p.color}"></i>{p.label}</span>{/each}
	</div>
	<ul class="bars" bind:clientWidth={width} aria-label={label}>
		{#each rows as r, i (r.key)}
			{@const s = segments(r)}
			<li
				onpointerenter={() => (hover = i)}
				onpointerleave={() => (hover = null)}
				onfocus={() => (hover = i)}
				onblur={() => (hover = null)}
				tabindex="-1"
			>
				{#if labelW === 0}<span class="name">{r.label}</span>{/if}
				{#if width > 0}
					<svg {width} height={rowH - 18} aria-hidden="true">
						{#if labelW > 0}<text class="name" x={labelW - 8} y="13" text-anchor="end">{r.label}</text>{/if}
						{#each s.out as seg, k (k)}<path d={seg.d} fill={seg.color} />{/each}
						<text class="pct" x={s.end + 6} y="13">{r.met_pct == null ? '' : `${Math.round(r.met_pct)}%`}</text>
					</svg>
				{/if}
				<span class="counts" style="padding-left: {labelW}px">{counts(r)}</span>
				<span class="sr-only">{r.label}: {r.met_pct == null ? 'nothing ended' : `${r.met_pct}% met`}, {counts(r)}</span>
				{#if hover === i}
					<div class="tip" style="left: {Math.min(s.end + 8, width - 180)}px" aria-hidden="true">
						<b>{r.label}</b>
						<span><i style="background: var(--series-1)"></i>Met {r.met}</span>
						<span><i style="background: var(--breached)"></i>Breached {r.breached}</span>
						<span><i style="background: var(--running)"></i>Still running {r.running}</span>
					</div>
				{/if}
			</li>
		{:else}
			<li class="none">Nothing in this period.</li>
		{/each}
	</ul>
{/if}

<style>
	.legend {
		display: flex;
		flex-wrap: wrap;
		gap: 0.35rem 0.9rem;
		font-size: 0.8125rem;
		color: var(--ink-2);
		margin-bottom: 0.5rem;
	}
	.legend i,
	.tip i {
		display: inline-block;
		width: 0.6rem;
		height: 0.6rem;
		border-radius: 3px;
		margin-right: 0.35rem;
		vertical-align: -0.05rem;
	}
	.bars {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 0.35rem;
	}
	li {
		position: relative;
		display: grid;
		outline: none;
	}
	svg {
		display: block;
	}
	.name {
		font-size: 12px;
		fill: var(--ink);
		color: var(--ink);
	}
	span.name {
		font-size: 0.8125rem;
		margin-bottom: 0.15rem;
	}
	.pct {
		font-size: 11px;
		fill: var(--ink-2);
		font-variant-numeric: tabular-nums;
	}
	.counts {
		font-size: 0.71875rem;
		color: var(--ink-2);
		margin-top: -0.3rem;
	}
	.none {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.tip {
		position: absolute;
		top: -0.25rem;
		z-index: 1;
		display: grid;
		gap: 0.15rem;
		padding: 0.45rem 0.65rem;
		border: 1px solid var(--rule);
		border-radius: 8px;
		background: var(--raised);
		box-shadow: var(--shadow-pop);
		font-size: 0.75rem;
		white-space: nowrap;
		pointer-events: none;
		font-variant-numeric: tabular-nums;
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
		padding: 0.35rem 0.5rem;
		border-bottom: 1px solid var(--rule);
		white-space: nowrap;
	}
	thead th {
		color: var(--ink-2);
		font-weight: 600;
		font-size: 0.75rem;
	}
	tbody th {
		font-weight: 500;
	}
</style>
