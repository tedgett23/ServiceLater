<script lang="ts">
	import { arc, pie, type PieArcDatum } from 'd3-shape';
	import { COLORS, parts, whole, type Slice, type ChartSpec } from './resultChart';
	import type { QueryColumn } from '$lib/queries';
	let { columns, rows, spec, label, truncated }: { columns: QueryColumn[]; rows: unknown[][]; spec: ChartSpec; label: string; truncated: boolean } = $props();
	const data = $derived(parts(columns, rows, spec, truncated));
	const sectors = $derived('slices' in data ? pie<Slice>().sort(null).value(s => s.value)(data.slices) : []);
	const shape = $derived(arc<PieArcDatum<Slice>>().innerRadius(spec.type === 'doughnut' ? 68 : 0).outerRadius(102));
	const colors = [...COLORS, 'var(--marker)', 'var(--green)', 'var(--ink-2)', 'var(--blue)'];
	const percent = new Intl.NumberFormat(undefined, {style: 'percent', maximumFractionDigits: 1});
</script>

{#if 'problem' in data}
	<p role="status">{data.problem}</p>
{:else}
	<figure aria-label={label}>
		<svg viewBox="-108 -108 216 216" role="img" aria-label="{spec.type === 'pie' ? 'Pie' : 'Doughnut'} chart: {label}">
			{#each sectors as s, i}
				<path d={shape(s) ?? ''} fill={colors[i]} stroke="var(--surface)" stroke-width="2"><title>{s.data.label}: {whole(s.value)} ({percent.format(s.value / data.total)})</title></path>
			{/each}
			{#if spec.type === 'doughnut'}<text text-anchor="middle" y="-3">Total</text><text class="total" text-anchor="middle" y="19">{whole(data.total)}</text>{/if}
		</svg>
		<figcaption><strong>{label}</strong><p>Share of the result total. Use counts or amounts that add up.</p><ul>
			{#each data.slices as s, i}<li><i style:background={colors[i]}></i><span>{s.label}</span><b>{whole(s.value)}</b><small>{percent.format(s.value / data.total)}</small></li>{/each}
		</ul></figcaption>
	</figure>
{/if}

<style>
	figure { margin: 0; display: flex; flex-wrap: wrap; align-items: center; gap: 1rem 2rem; }
	svg { width: min(100%, 15rem); margin-inline: auto; }
	text { fill: var(--ink-2); font-size: 12px; } .total { fill: var(--ink); font-weight: 650; font-size: 18px; }
	figcaption { flex: 1 1 14rem; min-width: 0; font-size: .875rem; overflow-wrap: anywhere; }
	ul { padding: 0; list-style: none; } li { display: grid; grid-template-columns: .6rem 1fr auto 3.5rem; align-items: center; gap: .6rem; padding: .35rem 0; border-bottom: 1px solid var(--rule); }
	i { width: .6rem; height: .6rem; border-radius: 50%; } b, small { text-align: right; font-variant-numeric: tabular-nums; } small, p { color: var(--ink-2); }
</style>
