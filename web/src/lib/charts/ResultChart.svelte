<script lang="ts">
	import { scaleLinear, scalePoint, scaleTime, scaleUtc } from 'd3-scale';
	import { line } from 'd3-shape';
	import { extent, max, min } from 'd3-array';
	import type { QueryColumn } from '$lib/queries';
	import { categories, COLORS, points as toPoints, short, whole, type ChartSpec } from './resultChart';

	/**
	 * A query's result drawn as the spec says: a line over its X, bars (grouped or stacked) by its X, or one figure.
	 * Marks follow the chart rules (thin bars with a rounded data end, 2px lines, 2px surface gaps between stacked
	 * pieces, ringed markers); colours are series-1..4 in the order the columns are given, never cycled; with two or
	 * more series a legend names them, and lines are also named at their ends. Hover, or focus and the arrow keys,
	 * reads exact values. The Results tab is the same data as a table.
	 */
	let { columns, rows, spec, label }: { columns: QueryColumn[]; rows: unknown[][]; spec: ChartSpec; label: string } = $props();

	const series = $derived(spec.y.map((key, i) => ({ key, color: COLORS[i] })));
	let width = $state(0);
	let active = $state<number | null>(null);

	// ---- the figure ----
	const figure = $derived.by(() => {
		if (spec.type !== 'figure') return null;
		const i = columns.findIndex((c) => c.name === spec.y[0]);
		const v = rows[0]?.[i];
		return { value: typeof v === 'number' || typeof v === 'string' ? Number(v) : null };
	});

	// ---- the line ----
	const height = 300;
	const narrow = $derived(width < 520);
	const pad = $derived({ left: 52, right: narrow ? 16 : 104, top: 12, bottom: 30 });
	const line_ = $derived(spec.type === 'line' ? toPoints(columns, rows, spec) : null);
	const pts = $derived(line_?.points ?? []);
	const xs = $derived.by(() => {
		const r: [number, number] = [pad.left, Math.max(pad.left + 1, width - pad.right)];
		if (!line_ || line_.kind === 'text') {
			const s = scalePoint<number>().domain(pts.map((_, i) => i)).range(r).padding(0.3);
			return { at: (i: number) => s(i) ?? r[0], ticks: [] as { at: number; text: string }[] };
		}
		const dom = extent(pts, (p) => p.x as number) as [number, number];
		if (dom[0] === dom[1]) dom[1] = dom[0] + 1;
		const n = Math.max(2, Math.floor((r[1] - r[0]) / 96));
		if (line_.kind === 'time') {
			const s = (line_.utc ? scaleUtc() : scaleTime()).domain(dom.map((d) => new Date(d))).range(r);
			// One format for every tick, from the span: times within two days, days within a year, months beyond.
			const span = dom[1] - dom[0];
			const tz = line_.utc ? 'UTC' : undefined;
			const opts: Intl.DateTimeFormatOptions =
				span < 2 * 86400000 ? { hour: 'numeric', minute: '2-digit', timeZone: tz } : span < 300 * 86400000 ? { day: 'numeric', month: 'short', timeZone: tz } : { month: 'short', year: 'numeric', timeZone: tz };
			const fmt = new Intl.DateTimeFormat(undefined, opts);
			return { at: (i: number) => s(new Date(pts[i].x as number)), ticks: s.ticks(n).map((t) => ({ at: s(t), text: fmt.format(t) })) };
		}
		const s = scaleLinear().domain(dom).range(r).nice();
		return { at: (i: number) => s(pts[i].x as number), ticks: s.ticks(n).map((t) => ({ at: s(t), text: short(t) })) };
	});
	const textTicks = $derived.by(() => {
		if (line_?.kind !== 'text') return [];
		const every = Math.max(1, Math.ceil(pts.length / Math.max(1, Math.floor((width - pad.left - pad.right) / 80))));
		return pts.flatMap((p, i) => (i % every === 0 ? [{ at: xs.at(i), text: p.label }] : []));
	});
	const ys = $derived.by(() => {
		const vals = pts.flatMap((p) => p.values.filter((v): v is number => v !== null));
		let lo = min(vals) ?? 0;
		let hi = max(vals) ?? 1;
		if (lo >= 0 && lo < hi * 0.4) lo = 0;
		if (lo === hi) [lo, hi] = [lo - 1, hi + 1];
		return scaleLinear().domain([lo, hi]).range([height - pad.bottom, pad.top]).nice();
	});
	const paths = $derived(
		series.map((_, s) =>
			line<number>()
				.defined((i) => pts[i].values[s] !== null)
				.x((i) => xs.at(i))
				.y((i) => ys(pts[i].values[s] ?? 0))(pts.map((_, i) => i)) ?? ''
		)
	);
	/** Each line named at its last point, nudged apart so two never overlap. */
	const ends = $derived.by(() => {
		if (narrow || series.length < 1) return [];
		const out = series.flatMap((sr, s) => {
			for (let i = pts.length - 1; i >= 0; i--) {
				const v = pts[i].values[s];
				if (v !== null) return [{ key: sr.key, color: sr.color, x: xs.at(i), dot: ys(v), y: ys(v) }];
			}
			return [];
		});
		out.sort((a, b) => a.y - b.y);
		for (let i = 1; i < out.length; i++) if (out[i].y - out[i - 1].y < 15) out[i].y = out[i - 1].y + 15;
		return out;
	});
	const positions = $derived(pts.map((_, i) => xs.at(i)));
	function nearest(px: number): number | null {
		if (!positions.length) return null;
		let best = 0;
		for (let i = 1; i < positions.length; i++) if (Math.abs(positions[i] - px) < Math.abs(positions[best] - px)) best = i;
		return best;
	}
	function onLineMove(e: PointerEvent) {
		const box = (e.currentTarget as SVGElement).getBoundingClientRect();
		active = nearest(e.clientX - box.left);
	}

	// ---- the bars ----
	const cats = $derived(spec.type === 'bars' || spec.type === 'stacked' ? categories(columns, rows, spec) : []);
	const stacked = $derived(spec.type === 'stacked');
	const labelsAbove = $derived(width < 480);
	const barH = $derived(stacked || series.length === 1 ? 20 : Math.max(8, Math.min(16, Math.floor(36 / series.length))));
	const bx = $derived.by(() => {
		let lo = 0;
		let hi = 0;
		for (const c of cats) {
			if (stacked) {
				hi = Math.max(hi, c.values.reduce<number>((sum, v) => sum + Math.max(0, v ?? 0), 0));
			} else {
				for (const v of c.values) if (v !== null) [lo, hi] = [Math.min(lo, v), Math.max(hi, v)];
			}
		}
		if (lo === hi) hi = lo + 1;
		return scaleLinear().domain([lo, hi]).range([0, 100]).nice();
	});
	const bticks = $derived(bx.ticks(width < 480 ? 3 : 5));
	const showEnds = $derived((stacked || series.length === 1) && cats.length <= 30);
	/** One bar (or a stacked piece) as left/width in percent, and which end is the data end (rounded). */
	function bar(from: number, to: number) {
		const a = bx(Math.min(from, to));
		const b = bx(Math.max(from, to));
		return { left: a, width: Math.max(0, b - a), negative: to < from };
	}
	/** Where a row's longest mark ends, in percent: its tooltip goes just past it, or over the bars when they are long. */
	const endOf = (c: { values: (number | null)[] }) =>
		stacked ? bx(totalOf(c)) : Math.max(...c.values.map((v) => (v === null ? bx(0) : Math.max(bx(v), bx(0)))));
	const totalOf = (c: { values: (number | null)[] }) => c.values.reduce<number>((s, v) => s + Math.max(0, v ?? 0), 0);

	function onKey(e: KeyboardEvent, count: number, horizontal: boolean) {
		const next = horizontal ? 'ArrowRight' : 'ArrowDown';
		const prev = horizontal ? 'ArrowLeft' : 'ArrowUp';
		if (![next, prev, 'Home', 'End', 'Escape'].includes(e.key) || !count) return;
		e.preventDefault();
		if (e.key === 'Escape') active = null;
		else if (e.key === 'Home') active = 0;
		else if (e.key === 'End') active = count - 1;
		else active = Math.min(count - 1, Math.max(0, (active ?? (e.key === next ? -1 : count)) + (e.key === next ? 1 : -1)));
	}
</script>

<div class="rc" bind:clientWidth={width}>
	{#if series.length >= 2 && spec.type !== 'figure'}
		<ul class="legend" aria-label="Series">
			{#each series as s (s.key)}
				<li><span class="key" style:background={s.color}></span>{s.key}</li>
			{/each}
		</ul>
	{/if}

	{#if spec.type === 'figure' && figure}
		<figure class="figure" aria-label="{spec.y[0]}: {whole(figure.value)}">
			<figcaption>{spec.y[0]}</figcaption>
			<span class="value">{whole(figure.value)}</span>
			{#if rows.length > 1}<span class="note">From the first of {rows.length.toLocaleString()} rows</span>{/if}
		</figure>
	{:else if spec.type === 'line'}
		{#if !pts.length}
			<p class="note">No rows have a value for {spec.x} to draw.</p>
		{:else}
			<!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_noninteractive_element_interactions -->
			<div
				class="plot"
				role="group"
				aria-label="{label}. Use the arrow keys to read each point."
				tabindex="0"
				onkeydown={(e) => onKey(e, pts.length, true)}
				onblur={() => (active = null)}
			>
				<svg {height} width="100%" role="img" aria-label={label} onpointermove={onLineMove} onpointerleave={() => (active = null)}>
					{#each ys.ticks(5) as t (t)}
						<line class="grid" x1={pad.left} x2={width - pad.right} y1={ys(t)} y2={ys(t)} />
						<text class="tick" x={pad.left - 8} y={ys(t)} dy="0.32em" text-anchor="end">{short(t)}</text>
					{/each}
					{#each line_?.kind === 'text' ? textTicks : xs.ticks as t, i (i)}
						<text class="tick" x={t.at} y={height - 8} text-anchor="middle">{t.text}</text>
					{/each}
					{#each paths as d, s (series[s].key)}
						<path class="line" {d} stroke={series[s].color} />
					{/each}
					{#if pts.length === 1}
						{#each series as sr, s (sr.key)}
							{#if pts[0].values[s] !== null}<circle class="dot" cx={xs.at(0)} cy={ys(pts[0].values[s] ?? 0)} r="4" fill={sr.color} />{/if}
						{/each}
					{/if}
					{#each ends as e (e.key)}
						<circle class="dot" cx={e.x} cy={e.dot} r="4" fill={e.color} />
						<text class="end" x={width - pad.right + 10} y={e.y} dy="0.32em">{e.key}</text>
					{/each}
					{#if active !== null && pts[active]}
						<line class="cross" x1={xs.at(active)} x2={xs.at(active)} y1={pad.top} y2={height - pad.bottom} />
						{#each series as sr, s (sr.key)}
							{#if pts[active].values[s] !== null}
								<circle class="dot" cx={xs.at(active)} cy={ys(pts[active].values[s] ?? 0)} r="4" fill={sr.color} />
							{/if}
						{/each}
					{/if}
				</svg>
				{#if active !== null && pts[active]}
					{@const left = Math.min(Math.max(xs.at(active) + 12, 0), Math.max(0, width - 220))}
					<div class="tip" style:left="{left}px" role="status">
						<strong class="tx">{pts[active].label}</strong>
						{#each series as sr, s (sr.key)}
							<span class="row"><span class="stroke" style:background={sr.color}></span><b>{whole(pts[active].values[s])}</b> {sr.key}</span>
						{/each}
					</div>
				{/if}
			</div>
		{/if}
	{:else if cats.length}
		<!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_noninteractive_element_interactions -->
		<div
			class="bars"
			class:above={labelsAbove}
			role="group"
			aria-label="{label}. Use the arrow keys to read each bar."
			tabindex="0"
			onkeydown={(e) => onKey(e, cats.length, false)}
			onblur={() => (active = null)}
			onpointerleave={() => (active = null)}
			style:--bar="{barH}px"
		>
			{#each cats as c, ci (ci)}
				<div class="brow" class:on={active === ci} onpointerenter={() => (active = ci)} role="presentation">
					<span class="blabel" title={c.label}>{c.label}</span>
					<div class="track">
						{#if !labelsAbove}
							{#each bticks as t (t)}<span class="vgrid" style:left="{bx(t)}%"></span>{/each}
						{/if}
						<div class="marks">
							{#if stacked}
								{@const total = totalOf(c)}
								<div class="bar stack">
									{#each series as sr, s (sr.key)}
										{@const before = c.values.slice(0, s).reduce<number>((sum, v) => sum + Math.max(0, v ?? 0), 0)}
										{@const b = bar(before, before + Math.max(0, c.values[s] ?? 0))}
										{#if b.width > 0}
											<span
												class="piece"
												class:last={c.values.slice(s + 1).every((v) => !v || v <= 0)}
												style:left="{b.left}%"
												style:width="calc({b.width}% - 2px)"
												style:background={sr.color}
											></span>
										{/if}
									{/each}
									{#if showEnds}<span class="endv" style:left="{bx(total)}%">{short(total)}</span>{/if}
								</div>
							{:else}
								{#each series as sr, s (sr.key)}
									{@const v = c.values[s]}
									<div class="bar">
										{#if v !== null && v !== 0}
											{@const b = bar(0, v)}
											<span class="piece last" class:neg={b.negative} style:left="{b.left}%" style:width="{b.width}%" style:background={sr.color}></span>
											{#if showEnds}<span class="endv" class:neg={b.negative} style:left="{b.negative ? b.left : b.left + b.width}%">{short(v)}</span>{/if}
										{/if}
									</div>
								{/each}
							{/if}
						</div>
						{#if active === ci}
							{@const end = endOf(c)}
							<div class="tip bartip" role="status" style:left={end < 55 ? `calc(${end}% + 3rem)` : '0.5rem'}>
								<strong class="tx">{c.label}</strong>
								{#each series as sr, s (sr.key)}
									<span class="row"><span class="stroke" style:background={sr.color}></span><b>{whole(c.values[s])}</b> {sr.key}</span>
								{/each}
								{#if stacked && series.length > 1}<span class="row total"><b>{whole(totalOf(c))}</b> in all</span>{/if}
								{#if c.folded}<span class="row total">The sum of {c.folded.toLocaleString()} rows</span>{/if}
							</div>
						{/if}
					</div>
				</div>
			{/each}
			<div class="brow axis" aria-hidden="true">
				<span class="blabel"></span>
				<div class="track ticks">
					{#each bticks as t (t)}<span class="tick" style:left="{bx(t)}%">{short(t)}</span>{/each}
				</div>
			</div>
		</div>
	{:else}
		<p class="note">No rows to draw.</p>
	{/if}
</div>

<style>
	.rc {
		position: relative;
		display: grid;
		gap: 0.75rem;
	}
	.legend {
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem 1.1rem;
		margin: 0;
		padding: 0;
		list-style: none;
		font-size: 0.8125rem;
		color: var(--ink);
	}
	.legend li {
		display: flex;
		align-items: center;
		gap: 0.4rem;
	}
	.key {
		width: 14px;
		height: 3px;
		border-radius: 2px;
	}
	.note {
		margin: 0;
		color: var(--ink-2);
		font-size: 0.875rem;
	}

	.figure {
		margin: 0;
		display: grid;
		gap: 0.25rem;
		padding: 1.25rem 0.25rem;
	}
	.figure figcaption {
		font-size: 0.875rem;
		color: var(--ink-2);
	}
	.figure .value {
		font-size: 3.5rem;
		font-weight: 650;
		letter-spacing: -0.02em;
		line-height: 1.1;
		color: var(--ink);
	}

	.plot,
	.bars {
		position: relative;
		border-radius: var(--radius);
	}
	.plot:focus-visible,
	.bars:focus-visible {
		outline: 2px solid var(--focus);
		outline-offset: 4px;
	}
	svg {
		display: block;
		overflow: visible;
	}
	.grid {
		stroke: var(--grid);
		stroke-width: 1;
	}
	.tick {
		fill: var(--ink-2);
		font-size: 11px;
		font-variant-numeric: tabular-nums;
	}
	.line {
		fill: none;
		stroke-width: 2;
		stroke-linejoin: round;
		stroke-linecap: round;
	}
	.dot {
		stroke: var(--ground);
		stroke-width: 2;
	}
	.end {
		fill: var(--ink);
		font-size: 12px;
	}
	.cross {
		stroke: var(--ink-2);
		stroke-width: 1;
	}

	.tip {
		position: absolute;
		top: 0;
		z-index: 2;
		display: grid;
		gap: 0.2rem;
		min-width: 9rem;
		max-width: 16rem;
		padding: 0.55rem 0.7rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		box-shadow: var(--shadow-pop);
		font-size: 0.8125rem;
		pointer-events: none;
	}
	.tip .tx {
		font-weight: 600;
		overflow-wrap: anywhere;
	}
	.tip .row {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		color: var(--ink-2);
	}
	.tip .row b {
		color: var(--ink);
		font-variant-numeric: tabular-nums;
	}
	.tip .stroke {
		flex: none;
		width: 12px;
		height: 3px;
		border-radius: 2px;
	}

	.bars {
		display: grid;
		gap: 0;
	}
	.brow {
		position: relative;
		display: grid;
		grid-template-columns: minmax(6rem, 28%) minmax(0, 1fr);
		align-items: center;
		gap: 0.75rem;
		padding: 5px 0;
	}
	.brow.on .blabel {
		color: var(--ink);
		font-weight: 600;
	}
	.blabel {
		font-size: 0.8125rem;
		color: var(--ink);
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
		text-align: right;
	}
	.track {
		position: relative;
		margin-right: 3.25rem;
	}
	.vgrid {
		position: absolute;
		top: -5px;
		bottom: -5px;
		width: 1px;
		background: var(--grid);
	}
	.marks {
		position: relative;
		display: grid;
		gap: 2px;
	}
	.bar {
		position: relative;
		height: var(--bar);
	}
	.piece {
		position: absolute;
		top: 0;
		bottom: 0;
		min-width: 1px;
	}
	.piece.last {
		border-radius: 0 4px 4px 0;
	}
	.piece.last.neg {
		border-radius: 4px 0 0 4px;
	}
	.endv {
		position: absolute;
		top: 50%;
		transform: translate(6px, -50%);
		font-size: 0.75rem;
		color: var(--ink-2);
		white-space: nowrap;
		font-variant-numeric: tabular-nums;
	}
	.endv.neg {
		transform: translate(calc(-100% - 6px), -50%);
	}
	.bartip {
		top: 50%;
		transform: translateY(-50%);
	}
	.axis .ticks {
		height: 1rem;
	}
	.axis .tick {
		position: absolute;
		transform: translateX(-50%);
		font-size: 11px;
		color: var(--ink-2);
		fill: none;
	}
	.bars.above .brow {
		grid-template-columns: minmax(0, 1fr);
		gap: 0.2rem;
	}
	.bars.above .blabel {
		text-align: left;
	}
		.bars.above .axis .blabel {
		display: none;
	}
</style>
