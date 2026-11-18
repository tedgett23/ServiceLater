<script lang="ts">
	import { scaleLinear, scalePoint } from 'd3-scale';
	import { line } from 'd3-shape';
	import { min } from 'd3-array';

	/**
	 * The share met over time, one line per series (an SLA), on one percentage axis. Hovering (or focusing and using the
	 * arrow keys) shows one bucket's figures with the counts behind them; each line is named at its end, and the legend
	 * names them too, so colour is never the only way to tell them apart. view = 'table' shows the same as a table.
	 */
	let {
		series,
		points,
		bucket,
		height = 230,
		view = 'chart',
		label
	}: {
		series: { key: string; label: string }[];
		points: { start: string; values: { ended: number; met: number; met_pct: number | null }[] }[];
		bucket: string;
		height?: number;
		view?: 'chart' | 'table';
		/** What the chart shows, for screen readers. */
		label: string;
	} = $props();

	/** Fixed order: the first series is always series-1, whatever is filtered. At most four lines are drawn. */
	const colors = ['var(--series-1)', 'var(--series-2)', 'var(--series-3)', 'var(--series-4)'];
	const shown = $derived(series.slice(0, colors.length));

	let width = $state(0);
	const pad = { left: 40, right: 74, top: 10, bottom: 24 };

	function bucketLabel(iso: string, long = false) {
		const d = new Date(iso);
		if (bucket === 'month') return d.toLocaleDateString(undefined, { month: long ? 'long' : 'short', year: long ? 'numeric' : undefined, timeZone: 'UTC' });
		const day = d.toLocaleDateString(undefined, { day: 'numeric', month: 'short', timeZone: 'UTC' });
		return long && bucket === 'week' ? `Week of ${day}` : day;
	}

	const lowest = $derived(min(points.flatMap((p) => p.values.slice(0, colors.length).map((v) => v.met_pct ?? 100))) ?? 100);
	const y = $derived(
		scaleLinear()
			.domain([Math.max(0, Math.floor((lowest - 5) / 10) * 10), 100])
			.range([height - pad.bottom, pad.top])
	);
	const x = $derived(
		scalePoint<number>()
			.domain(points.map((_, i) => i))
			.range([pad.left, Math.max(pad.left + 1, width - pad.right)])
			.padding(0.1)
	);
	const ticks = $derived(y.ticks(4));
	/** Every nth bucket is labelled, so labels never crowd. */
	const every = $derived(Math.max(1, Math.ceil(points.length / Math.max(1, Math.floor((width - pad.left - pad.right) / 64)))));

	const paths = $derived(
		shown.map((_, s) =>
			line<{ i: number; v: number | null }>()
				.defined((d) => d.v !== null)
				.x((d) => x(d.i)!)
				.y((d) => y(d.v!))(points.map((p, i) => ({ i, v: p.values[s]?.met_pct ?? null })))
		)
	);

	/** Each line's name at its last point, nudged apart so two never overlap. */
	const ends = $derived.by(() => {
		const out = shown
			.map((s, k) => {
				let i = points.length - 1;
				while (i >= 0 && points[i].values[k]?.met_pct == null) i--;
				return i < 0 ? null : { k, label: s.label, x: x(i)!, y: y(points[i].values[k].met_pct!) };
			})
			.filter((e) => e !== null)
			.sort((a, b) => a.y - b.y);
		for (let i = 1; i < out.length; i++) if (out[i].y - out[i - 1].y < 14) out[i].y = out[i - 1].y + 14;
		return out;
	});

	let at = $state<number | null>(null);
	function pick(e: PointerEvent) {
		const box = (e.currentTarget as SVGElement).getBoundingClientRect();
		const px = e.clientX - box.left;
		let best = 0;
		points.forEach((_, i) => {
			if (Math.abs(x(i)! - px) < Math.abs(x(best)! - px)) best = i;
		});
		at = points.length ? best : null;
	}
	function key(e: KeyboardEvent) {
		if (!points.length) return;
		if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
			e.preventDefault();
			const step = e.key === 'ArrowRight' ? 1 : -1;
			at = Math.min(points.length - 1, Math.max(0, (at ?? (step > 0 ? -1 : points.length)) + step));
		} else if (e.key === 'Escape') at = null;
	}
	const pct = (v: number | null) => (v == null ? '–' : `${v}%`);
</script>

{#if view === 'table'}
	<div class="table-wrap">
		<table>
			<caption class="sr-only">{label}</caption>
			<thead>
				<tr>
					<th scope="col">{bucket === 'month' ? 'Month' : bucket === 'week' ? 'Week of' : 'Day'}</th>
					{#each series as s (s.key)}<th scope="col">{s.label} met</th>{/each}
				</tr>
			</thead>
			<tbody>
				{#each points as p (p.start)}
					<tr>
						<th scope="row">{bucketLabel(p.start)}</th>
						{#each p.values as v, k (k)}<td>{pct(v.met_pct)} <span class="muted">({v.met} of {v.ended})</span></td>{/each}
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{:else}
	{#if shown.length > 1}
		<div class="legend" aria-hidden="true">
			{#each shown as s, k (s.key)}<span><i style="background: {colors[k]}"></i>{s.label}</span>{/each}
		</div>
	{/if}
	<div class="plot" bind:clientWidth={width} style="height: {height}px">
		{#if width > 0}
			<!-- Focusable on purpose: the arrow keys step through the buckets, as hovering does. -->
			<!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_noninteractive_element_interactions -->
			<svg
				{width}
				{height}
				role="img"
				aria-label="{label}. Use the arrow keys for each {bucket}, or the table view."
				tabindex="0"
				onpointermove={pick}
				onpointerleave={() => (at = null)}
				onkeydown={key}
				onblur={() => (at = null)}
			>
				{#each ticks as t (t)}
					<line class="grid" x1={pad.left} x2={width - pad.right} y1={y(t)} y2={y(t)} />
					<text class="axis" x={pad.left - 6} y={y(t) + 4} text-anchor="end">{t}%</text>
				{/each}
				{#each points as p, i (p.start)}
					{#if i % every === 0}<text class="axis" x={x(i)} y={height - 6} text-anchor="middle">{bucketLabel(p.start)}</text>{/if}
				{/each}
				{#if at !== null}<line class="cross" x1={x(at)} x2={x(at)} y1={pad.top} y2={height - pad.bottom} />{/if}
				{#each paths as d, k (k)}
					{#if d}<path {d} fill="none" stroke={colors[k]} stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />{/if}
				{/each}
				{#if at !== null}
					{#each shown as _, k (k)}
						{@const v = points[at].values[k]?.met_pct}
						{#if v != null}<circle cx={x(at)} cy={y(v)} r="4" fill={colors[k]} stroke="var(--ground)" stroke-width="2" />{/if}
					{/each}
				{/if}
				{#each ends as e (e.k)}
					<text class="end" x={e.x + 8} y={e.y + 4}>{e.label}</text>
				{/each}
			</svg>
			{#if at !== null}
				{@const left = x(at)! > width / 2}
				<div class="tip" style="{left ? 'right' : 'left'}: {left ? width - x(at)! + 12 : x(at)! + 12}px; top: {pad.top}px" aria-hidden="true">
					<b>{bucketLabel(points[at].start, true)}</b>
					{#each shown as s, k (s.key)}
						{@const v = points[at].values[k]}
						<span><i style="background: {colors[k]}"></i>{s.label} {pct(v.met_pct)} <span class="muted">({v.met} of {v.ended})</span></span>
					{/each}
				</div>
			{/if}
		{/if}
	</div>
	{#if series.length > shown.length}
		<p class="more">Showing the first {shown.length} SLAs: choose one above to see the others.</p>
	{/if}
{/if}

<style>
	.legend {
		display: flex;
		flex-wrap: wrap;
		gap: 0.35rem 0.9rem;
		font-size: 0.8125rem;
		color: var(--ink-2);
		margin-bottom: 0.35rem;
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
	.plot {
		position: relative;
		width: 100%;
	}
	svg {
		display: block;
		overflow: visible;
		outline-offset: 2px;
	}
	svg:focus-visible {
		outline: 2px solid var(--blue);
		border-radius: 4px;
	}
	.grid {
		stroke: var(--grid);
	}
	.cross {
		stroke: var(--ink-2);
		stroke-dasharray: 2 3;
	}
	.axis {
		font-size: 11px;
		fill: var(--ink-2);
		font-variant-numeric: tabular-nums;
	}
	.end {
		font-size: 11.5px;
		font-weight: 600;
		fill: var(--ink);
	}
	.tip {
		position: absolute;
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
	.muted {
		color: var(--ink-2);
	}
	.more {
		margin-top: 0.35rem;
		font-size: 0.75rem;
		color: var(--ink-2);
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
