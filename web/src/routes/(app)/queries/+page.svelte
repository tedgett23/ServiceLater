<script lang="ts">
	import { onMount, tick } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { when } from '$lib/format';
	import {
		cellText,
		loadRuns,
		loadSchema,
		problemOf,
		removeView,
		runQuery,
		saveView,
		toCSV,
		type BuiltinView,
		type QueryProblem,
		type QueryResult,
		type QueryRun,
		type SavedView
	} from '$lib/queries';
	import type { SchemaView, SqlView } from '$lib/sqlEditor';
	import { paramCount } from '$lib/sqlParams';
	import { session } from '$lib/session.svelte';
	import ResultChart from '$lib/charts/ResultChart.svelte';
	import { fits, guessChart, isNumeric, MAX_SERIES, type ChartSpec, type ChartType } from '$lib/charts/resultChart';

	/**
	 * The SQL workbench (docs/query.md): the tables at the left (built-in views and saved views, with their columns and
	 * what each holds), the editor, and the results or the person's past queries under it. A query runs as the person,
	 * over what they may see. "Save as view" keeps the query under a name, private or shared, for other queries and
	 * GET/QUERY /api/v1/views/{name}/rows. The draft is kept in this browser.
	 */
	const DRAFT = `queries.draft.${session.user!.id}`;
	const starter = "SELECT reference, title, status, priority\nFROM records\nWHERE state_category IN ('open', 'waiting')\nORDER BY priority, created_at\nLIMIT 50";

	let text = $state(readDraft());
	let params = $state<string[]>([]);
	let builtins = $state<BuiltinView[]>([]);
	let saved = $state<SavedView[]>([]);
	let filter = $state('');
	// records starts open beside the editor; on a phone the list sits under it, so everything starts folded.
	let open = $state<Record<string, boolean>>({ records: wide() });
	let described = $state<{ view: string; column: string; text: string } | null>(null);
	let result = $state<QueryResult | null>(null);
	let problem = $state<QueryProblem | null>(null);
	let running = $state(false);
	let tab = $state<'results' | 'chart' | 'history'>('results');
	/** How the result is drawn on the Chart tab: picked to fit the columns, changed by hand, kept with a saved view. */
	let chart = $state<ChartSpec | null>(null);
	/** The columns of the last result, so a chart is kept while they stay (re-running, a new WHERE) and picked afresh
	 * when they change; null after opening a view, whose chart its first run keeps. */
	let lastShape: string | null = null;
	let runs = $state<QueryRun[]>([]);
	let loadError = $state('');
	/** The saved view being edited, when one was opened: Save then changes it. */
	let editing = $state<SavedView | null>(null);

	let host: HTMLDivElement;
	let editor: SqlView | null = null;
	let dialog: HTMLDialogElement;
	let form = $state({ name: '', description: '', shared: false, keepChart: false });
	let saveProblem = $state('');
	let saving = $state(false);
	let notice = $state('');

	const needed = $derived(paramCount(text));
	$effect(() => {
		if (params.length !== needed) params = Array.from({ length: needed }, (_, i) => params[i] ?? '');
	});

	function wide(): boolean {
		try {
			return window.matchMedia('(min-width: 40.01rem)').matches;
		} catch {
			return true;
		}
	}

	function readDraft(): string {
		try {
			return localStorage.getItem(DRAFT) ?? starter;
		} catch {
			return starter;
		}
	}
	function keepDraft(t: string) {
		try {
			localStorage.setItem(DRAFT, t);
		} catch {
			/* private window: the draft just is not kept */
		}
	}

	const schemaViews = (): SchemaView[] => [
		...saved.map((v) => ({ name: v.name, description: v.description, columns: v.columns })),
		...builtins
	];

	async function refreshSchema() {
		try {
			const s = await loadSchema();
			builtins = s.views;
			saved = s.saved_views;
			editor?.setSchema(schemaViews());
			loadError = '';
		} catch (e) {
			loadError = problemOf(e).error;
		}
	}

	onMount(() => {
		let gone = false;
		void refreshSchema().then(() => {
			const name = page.url.searchParams.get('view');
			const v = name ? saved.find((x) => x.name === name) : undefined;
			if (v) openView(v);
		});
		import('$lib/sqlEditor').then((cm) => {
			if (gone) return;
			editor = cm.createSqlEditor(host, text, schemaViews(), 'SQL query', (t) => {
				text = t;
				keepDraft(t);
				if (problem) {
					problem = null;
					editor?.setProblem(null);
				}
			}, run);
		});
		return () => {
			gone = true;
			editor?.destroy();
		};
	});

	async function run() {
		if (running || !text.trim()) return;
		running = true;
		problem = null;
		editor?.setProblem(null);
		if (tab === 'history') tab = 'results';
		try {
			result = await runQuery(text, params);
			const shape = result.columns.map((c) => `${c.name}:${c.type}`).join(',');
			const changed = lastShape !== null && lastShape !== shape;
			if (!chart || changed || !fits(chart, result.columns)) chart = guessChart(result.columns, result.rows.length);
			lastShape = shape;
		} catch (e) {
			result = null;
			problem = problemOf(e);
			editor?.setProblem({ line: problem.line, column: problem.column, message: problem.error });
		} finally {
			running = false;
		}
		void loadRuns().then((r) => (runs = r)).catch(() => {});
	}

	async function showHistory() {
		tab = 'history';
		try {
			runs = await loadRuns();
		} catch (e) {
			loadError = problemOf(e).error;
		}
	}

	function useText(t: string) {
		text = t;
		editor?.setText(t);
		keepDraft(t);
	}

	function openView(v: SavedView) {
		editing = v.mine ? v : null;
		useText(v.sql);
		chart = v.chart ? { ...v.chart, y: [...v.chart.y] } : null;
		lastShape = null;
		tab = v.chart ? 'chart' : 'results';
		result = null;
		form = { name: v.mine ? v.name : '', description: v.description, shared: v.shared, keepChart: !!v.chart };
		void goto(v.mine ? `?view=${encodeURIComponent(v.name)}` : '?', { replaceState: true, keepFocus: true, noScroll: true });
		notice = v.mine ? '' : `${v.name} is ${v.owner_name}'s view: save a change under a name of your own.`;
	}

	function newQuery() {
		editing = null;
		form = { name: '', description: '', shared: false, keepChart: false };
		notice = '';
		void goto('?', { replaceState: true, keepFocus: true, noScroll: true });
		editor?.focus();
	}

	async function openSave() {
		saveProblem = '';
		form.keepChart = !!chart && (tab === 'chart' || !!editing?.chart);
		dialog.showModal();
		await tick();
		dialog.querySelector<HTMLInputElement>(editing ? '#sv-description' : '#sv-name')?.focus();
	}

	async function submitSave(e: SubmitEvent) {
		e.preventDefault();
		saving = true;
		saveProblem = '';
		try {
			const v = await saveView(form.name.trim(), {
				sql: text,
				description: form.description,
				shared: form.shared,
				chart: form.keepChart && chart ? chart : null
			});
			dialog.close();
			await refreshSchema();
			editing = saved.find((x) => x.name === v.name) ?? v;
			notice = `Saved as ${v.name}${v.shared ? ', shared with everyone who runs queries' : ''}.`;
			open = { ...open, [v.name]: true };
			void goto(`?view=${encodeURIComponent(v.name)}`, { replaceState: true, keepFocus: true, noScroll: true });
		} catch (err) {
			const p = problemOf(err);
			saveProblem = p.line ? `Line ${p.line}, column ${p.column}: ${p.error}` : p.error;
			if (p.line) editor?.setProblem({ line: p.line, column: p.column, message: p.error });
		} finally {
			saving = false;
		}
	}

	async function remove(v: SavedView) {
		if (!confirm(`Remove the view ${v.name}? Queries that name it will stop working.`)) return;
		try {
			await removeView(v.name);
			if (editing?.name === v.name) newQuery();
			notice = `Removed ${v.name}.`;
			await refreshSchema();
		} catch (e) {
			notice = problemOf(e).error;
		}
	}

	function goToProblem() {
		editor?.focus();
	}

	function download() {
		if (!result) return;
		const url = URL.createObjectURL(new Blob(['﻿' + toCSV(result)], { type: 'text/csv' }));
		const a = document.createElement('a');
		a.href = url;
		a.download = `${editing?.name ?? 'query'}.csv`;
		a.click();
		setTimeout(() => URL.revokeObjectURL(url), 1000);
	}

	const matches = (v: { name: string; description: string; columns: { name: string }[] }) => {
		const q = filter.trim().toLowerCase();
		return !q || v.name.includes(q) || v.description.toLowerCase().includes(q) || v.columns.some((c) => c.name.includes(q));
	};
	const mine = $derived(saved.filter((v) => v.mine && matches(v)));
	const sharedViews = $derived(saved.filter((v) => !v.mine && matches(v)));
	const shownBuiltins = $derived(builtins.filter(matches));

	/** A cell that is a record's or article's reference opens it. */
	function linkOf(column: string, v: unknown): string | null {
		if (column !== 'reference' || typeof v !== 'string') return null;
		if (/^KB\d+$/.test(v)) return `/kb/${v}`;
		return /^[A-Z]{2,5}\d+$/.test(v) ? `/tickets/${v}` : null;
	}
	const chartTypes: { id: ChartType; label: string }[] = [
		{ id: 'line', label: 'Line' },
		{ id: 'bars', label: 'Bars' },
		{ id: 'stacked', label: 'Stacked' },
		{ id: 'figure', label: 'Figure' }
	];
	const numericColumns = $derived(result ? result.columns.filter(isNumeric) : []);
	function setType(t: ChartType) {
		if (!result || !chart) return;
		const y = t === 'figure' ? chart.y.slice(0, 1) : chart.y;
		const x = t === 'figure' ? undefined : chart.x && result.columns.some((c) => c.name === chart!.x) ? chart.x : result.columns.find((c) => !isNumeric(c))?.name ?? result.columns[0]?.name;
		chart = { type: t, x, y: y.length ? y : numericColumns.slice(0, 1).map((c) => c.name) };
	}
	function toggleY(name: string, on: boolean) {
		if (!chart) return;
		const order = numericColumns.map((c) => c.name);
		const y = on ? [...chart.y, name] : chart.y.filter((n) => n !== name);
		// Columns keep the result's order, so a column keeps its colour as others come and go.
		chart = { ...chart, y: order.filter((n) => y.includes(n)) };
	}
	const chartLabel = $derived(
		chart ? (chart.type === 'figure' ? chart.y[0] : `${chart.y.join(', ')} by ${chart.x}`) : ''
	);
	const outcomeLabel = { ok: 'OK', refused: 'Refused', failed: 'Failed', timeout: 'Timed out' } as const;
</script>

<svelte:head><title>Queries · ServiceLater</title></svelte:head>

<div class="bench">
	<section class="tables" aria-label="Tables">
		<label class="find">
			<span>Find a table or column</span>
			<input type="search" bind:value={filter} placeholder="records, breached_at…" />
		</label>
		{#if loadError}<p class="error" role="alert">{loadError}</p>{/if}

		{#snippet viewItem(v: { name: string; description: string; columns: { name: string; type: string; description?: string }[] }, sv?: SavedView)}
			<li class="view">
				<div class="vhead">
					<button
						type="button"
						class="vname"
						aria-expanded={!!open[v.name] || !!filter.trim()}
						onclick={() => (open = { ...open, [v.name]: !open[v.name] })}
					>
						<span>{v.name}</span>
						<span class="count">{v.columns.length}</span>
					</button>
					<button type="button" class="mini" title="Put {v.name} in the query" aria-label="Insert {v.name}" onclick={() => editor?.insert(v.name)}>+</button>
				</div>
				{#if open[v.name] || filter.trim()}
					{#if v.description}<p class="vdesc">{v.description}</p>{/if}
					{#if sv}
						<p class="vmeta">
							{sv.shared ? 'Shared' : 'Private'} · {sv.mine ? 'yours' : sv.owner_name}
							<button type="button" class="link" onclick={() => openView(sv)}>{sv.mine ? 'Edit' : 'Open'}</button>
							{#if sv.mine}<button type="button" class="link danger" onclick={() => remove(sv)}>Remove</button>{/if}
						</p>
					{/if}
					<ul class="cols">
						{#each v.columns as c (c.name)}
							<li>
								<button
									type="button"
									class:on={described?.view === v.name && described?.column === c.name}
									onclick={() => {
										editor?.insert(c.name);
										described = c.description ? { view: v.name, column: c.name, text: c.description } : null;
									}}
									title={c.description || c.type}
								>
									<span>{c.name}</span><span class="type">{c.type}</span>
								</button>
							</li>
						{/each}
					</ul>
					{#if described?.view === v.name}
						<p class="coldesc"><b>{described.column}</b>: {described.text}</p>
					{/if}
				{/if}
			</li>
		{/snippet}

		{#if mine.length}
			<h2>Your views</h2>
			<ul class="list">
				{#each mine as v (v.name)}{@render viewItem(v, v)}{/each}
			</ul>
		{/if}
		{#if sharedViews.length}
			<h2>Shared views</h2>
			<ul class="list">
				{#each sharedViews as v (v.name)}{@render viewItem(v, v)}{/each}
			</ul>
		{/if}
		<h2>Built-in</h2>
		<ul class="list">
			{#each shownBuiltins as v (v.name)}{@render viewItem(v)}{/each}
		</ul>
	</section>

	<div class="work">
		<header class="bar">
			<h1>Queries</h1>
			{#if editing}
				<p class="editing">Editing view <code>{editing.name}</code>{editing.shared ? ' · shared' : ''}</p>
				<button type="button" class="btn" onclick={newQuery}>New query</button>
			{/if}
			<span class="grow"></span>
			<button type="button" class="btn" onclick={openSave} disabled={!text.trim()}>{editing ? 'Save view' : 'Save as view'}</button>
			<button type="button" class="btn primary run" onclick={run} disabled={running || !text.trim()} aria-keyshortcuts="Control+Enter Meta+Enter">
				{running ? 'Running…' : 'Run'}<span class="kbd" aria-hidden="true">Ctrl ↵</span>
			</button>
		</header>
		{#if notice}<p class="notice" role="status">{notice}</p>{/if}

		<div class="editor" bind:this={host}></div>

		{#if params.length}
			<div class="params" role="group" aria-label="Parameters">
				{#each params as _, i (i)}
					<label class="field">
						<span>${i + 1}</span>
						<input bind:value={params[i]} placeholder="value" />
					</label>
				{/each}
			</div>
		{/if}

		{#if problem}
			<div class="problem" role="alert">
				<strong>{problem.timeout ? 'Too slow' : 'Not run'}: {problem.error}</strong>
				{#if problem.line}
					<span>Line {problem.line}, column {problem.column}. <button type="button" class="link" onclick={goToProblem}>Go to it</button></span>
				{/if}
			</div>
		{/if}

		<div class="tools">
			<div class="segmented" role="group" aria-label="Show">
				<button type="button" aria-pressed={tab === 'results'} onclick={() => (tab = 'results')}>Results</button>
				<button type="button" aria-pressed={tab === 'chart'} onclick={() => (tab = 'chart')}>Chart</button>
				<button type="button" aria-pressed={tab === 'history'} onclick={showHistory}>History</button>
			</div>
			{#if tab !== 'history' && result}
				<span class="status">
					{result.rows.length.toLocaleString()} {result.rows.length === 1 ? 'row' : 'rows'}{result.truncated ? ' (more not shown)' : ''} · {result.duration_ms} ms
				</span>
				<span class="grow"></span>
				<button type="button" class="btn" onclick={download}>Download CSV</button>
			{/if}
		</div>

		{#if tab === 'results'}
			{#if result}
				{#if result.truncated}
					<p class="note">Only the first {result.rows.length.toLocaleString()} rows are shown. Narrow the query, or read a saved view page by page through the API.</p>
				{/if}
				<div class="grid" role="region" aria-label="Results" tabindex="-1">
					<table>
						<thead>
							<tr>
								{#each result.columns as c, i (i)}
									<th class:num={isNumeric(c)} scope="col">{c.name} <span class="type">{c.type}</span></th>
								{/each}
							</tr>
						</thead>
						<tbody>
							{#each result.rows as row, r (r)}
								<tr>
									{#each row as v, i (i)}
										{@const link = linkOf(result.columns[i].name, v)}
										<td class:num={isNumeric(result.columns[i])} class:null={v === null}>
											{#if link}<a class="ref" href={link}>{v}</a>{:else}{cellText(v)}{/if}
										</td>
									{/each}
								</tr>
							{:else}
								<tr><td class="empty" colspan={result.columns.length || 1}>No rows.</td></tr>
							{/each}
						</tbody>
					</table>
				</div>
			{:else if !problem}
				<p class="hint">Write a query over the tables at the left and run it (Ctrl or ⌘ + Enter). You see only what you may see in ServiceLater, and nothing you run changes anything.</p>
			{/if}
		{:else if tab === 'chart'}
			{#if result && chart}
				<div class="chartctl" role="group" aria-label="Chart">
					<div class="segmented" role="group" aria-label="Chart type">
						{#each chartTypes as t (t.id)}
							<button type="button" aria-pressed={chart.type === t.id} onclick={() => setType(t.id)}>{t.label}</button>
						{/each}
					</div>
					{#if chart.type !== 'figure'}
						<label class="pick">
							<span>{chart.type === 'line' ? 'Along the bottom' : 'One bar per'}</span>
							<select class="select" value={chart.x} onchange={(e) => (chart = { ...chart!, x: e.currentTarget.value })}>
								{#each result.columns as c (c.name)}<option value={c.name}>{c.name}</option>{/each}
							</select>
						</label>
						<fieldset class="ys">
							<legend>Measure</legend>
							{#each numericColumns as c (c.name)}
								<label>
									<input
										type="checkbox"
										checked={chart.y.includes(c.name)}
										disabled={!chart.y.includes(c.name) && chart.y.length >= MAX_SERIES}
										onchange={(e) => toggleY(c.name, e.currentTarget.checked)}
									/>
									{c.name}
								</label>
							{/each}
						</fieldset>
					{:else}
						<label class="pick">
							<span>Show</span>
							<select class="select" value={chart.y[0]} onchange={(e) => (chart = { ...chart!, y: [e.currentTarget.value] })}>
								{#each numericColumns as c (c.name)}<option value={c.name}>{c.name}</option>{/each}
							</select>
						</label>
					{/if}
				</div>
				{#if chart.y.length}
					<div class="chartbox">
						<ResultChart columns={result.columns} rows={result.rows} spec={chart} label={chartLabel} />
					</div>
				{:else}
					<p class="hint">Pick at least one column to measure.</p>
				{/if}
			{:else if result}
				<p class="hint">Nothing here to chart: a chart measures a number, and this result has no number column.</p>
			{:else if !problem}
				<p class="hint">Run the query to draw it{chart ? `: it is kept as ${chart.type === 'figure' ? 'a figure' : `a ${chart.type === 'line' ? 'line' : chart.type === 'bars' ? 'bar' : 'stacked bar'} chart`}` : ''}.</p>
			{/if}
		{:else}
			<ul class="runs" aria-label="Your recent queries">
				{#each runs as q (q.id)}
					<li>
						<button type="button" onclick={() => { useText(q.sql); tab = 'results'; }}>
							<span class="outcome {q.outcome}">{outcomeLabel[q.outcome]}</span>
							<code>{q.sql.replace(/\s+/g, ' ')}</code>
							<span class="when">{when(q.started_at)}</span>
							<span class="rows">{q.outcome === 'ok' ? `${q.row_count} rows` : q.error}</span>
						</button>
					</li>
				{:else}
					<li class="empty">Nothing run yet.</li>
				{/each}
			</ul>
		{/if}
	</div>
</div>

<dialog bind:this={dialog} class="save" aria-labelledby="sv-title">
	<form onsubmit={submitSave}>
		<h2 id="sv-title">{editing ? `Save view ${editing.name}` : 'Save as view'}</h2>
		<p class="lead">
			Other queries can then read it like a table, and the API serves its rows. Whoever reads it sees only their own
			rows.
		</p>
		<label class="field">
			<span>Name</span>
			<input id="sv-name" bind:value={form.name} required pattern="[a-z][a-z0-9_]{'{'}1,62{'}'}" readonly={!!editing}
				placeholder="open_incidents" autocomplete="off" spellcheck="false" />
		</label>
		<p class="help">Lower-case letters, digits and underscores, starting with a letter.</p>
		<label class="field">
			<span>Description</span>
			<textarea id="sv-description" bind:value={form.description} rows="3" maxlength="2000" placeholder="What it shows, for whoever finds it"></textarea>
		</label>
		<label class="check">
			<input type="checkbox" bind:checked={form.shared} />
			<span>Share with everyone who runs queries</span>
		</label>
		{#if chart}
			<label class="check">
				<input type="checkbox" bind:checked={form.keepChart} />
				<span>Keep the chart with it ({chartTypes.find((t) => t.id === chart?.type)?.label.toLowerCase()} of {chart.y.join(', ')})</span>
			</label>
		{/if}
		{#if saveProblem}<p class="error" role="alert">{saveProblem}</p>{/if}
		<div class="actions">
			<button type="button" class="btn" onclick={() => dialog.close()}>Cancel</button>
			<button type="submit" class="btn primary" disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
		</div>
	</form>
</dialog>

<style>
	.bench {
		display: flex;
		flex-wrap: wrap;
		min-height: 100%;
	}
	.tables {
		flex: 0 0 16.5rem;
		box-sizing: border-box;
		padding: 1rem;
		background: var(--surface);
		border-right: 1px solid var(--rule);
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
		max-height: 100vh;
		overflow-y: auto;
		position: sticky;
		top: 0;
	}
	.find {
		display: grid;
		gap: 0.35rem;
		font-size: 0.75rem;
		color: var(--ink-2);
	}
	.find input {
		min-height: 2.125rem;
		padding: 0 0.6rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		font-size: 0.875rem;
	}
	.tables h2 {
		margin: 0.75rem 0 0.1rem;
		font-size: 0.75rem;
		font-weight: 600;
		color: var(--ink-2);
	}
	.list,
	.cols,
	.runs {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.vhead {
		display: flex;
		gap: 2px;
	}
	.vname {
		flex: 1;
		display: flex;
		justify-content: space-between;
		align-items: center;
		min-height: 2rem;
		padding: 0 0.5rem;
		border: 0;
		border-radius: 6px;
		background: none;
		text-align: left;
		cursor: pointer;
		font-family: var(--font-mono);
		font-size: 0.8125rem;
	}
	.vname[aria-expanded='true'] {
		background: var(--hover);
		font-weight: 600;
	}
	.vname:hover,
	.mini:hover,
	.cols button:hover {
		background: var(--hover);
	}
	.count,
	.type {
		font-family: var(--font-body);
		font-weight: 400;
		font-size: 0.6875rem;
		color: var(--ink-2);
	}
	.mini {
		width: 2rem;
		border: 0;
		border-radius: 6px;
		background: none;
		color: var(--ink-2);
		cursor: pointer;
	}
	.vdesc,
	.vmeta,
	.coldesc {
		margin: 0.2rem 0.5rem 0.4rem;
		font-size: 0.75rem;
		line-height: 1.4;
		color: var(--ink-2);
	}
	.vmeta {
		display: flex;
		flex-wrap: wrap;
		gap: 0.25rem 0.6rem;
		align-items: center;
	}
	.coldesc {
		padding: 0.5rem;
		border: 1px solid var(--rule);
		border-radius: 6px;
		background: var(--raised);
	}
	.coldesc b {
		color: var(--ink);
	}
	.cols button {
		display: flex;
		justify-content: space-between;
		gap: 0.5rem;
		width: 100%;
		padding: 0.2rem 0.5rem 0.2rem 1rem;
		border: 0;
		border-radius: 4px;
		background: none;
		text-align: left;
		cursor: pointer;
		font-family: var(--font-mono);
		font-size: 0.75rem;
	}
	.cols button.on {
		background: var(--marker-soft);
	}
	.link {
		padding: 0;
		border: 0;
		background: none;
		color: var(--blue);
		cursor: pointer;
		font-size: inherit;
		text-decoration: underline;
	}
	.link.danger {
		color: var(--red);
	}

	.work {
		flex: 999 1 35rem;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
		padding: 1rem 1.5rem 1.5rem;
		box-sizing: border-box;
	}
	.bar {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.6rem;
	}
	h1 {
		margin: 0;
		font-size: 1.25rem;
		font-weight: 650;
		letter-spacing: -0.015em;
	}
	.editing {
		margin: 0;
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.grow {
		flex: 1;
	}
	.kbd {
		font-size: 0.75rem;
		font-weight: 500;
		opacity: 0.75;
	}
	.notice,
	.note,
	.hint {
		margin: 0;
		font-size: 0.875rem;
		color: var(--ink-2);
	}
	.params {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem;
	}
	.params .field {
		width: 12rem;
	}
	.problem {
		display: grid;
		gap: 0.25rem;
		padding: 0.75rem 0.9rem;
		border: 1px solid color-mix(in srgb, var(--red) 40%, var(--rule));
		border-radius: var(--radius);
		background: color-mix(in srgb, var(--red) 8%, var(--ground));
		font-size: 0.875rem;
	}
	.problem span {
		color: var(--ink-2);
	}
	.tools {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.75rem;
	}
	.status {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.grid {
		overflow: auto;
		max-height: 60vh;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
	}
	table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.8125rem;
	}
	th {
		position: sticky;
		top: 0;
		background: var(--surface);
		text-align: left;
		font-weight: 600;
		white-space: nowrap;
	}
	th,
	td {
		padding: 0.5rem 0.85rem;
		border-bottom: 1px solid var(--rule);
		vertical-align: top;
	}
	td {
		min-width: 7rem;
		max-width: 28rem;
		overflow-wrap: anywhere;
	}
	td.num {
		min-width: 0;
	}
	.num {
		text-align: right;
		font-variant-numeric: tabular-nums;
	}
	td.null {
		color: var(--ink-2);
		font-style: italic;
	}
	.ref {
		font-family: var(--font-mono);
		padding: 0 0.3rem;
		border-radius: 4px;
		background: var(--marker);
		color: var(--on-marker);
		text-decoration: none;
	}
	.empty {
		color: var(--ink-2);
		padding: 0.75rem;
	}
	.runs {
		border: 1px solid var(--rule);
		border-radius: var(--radius);
	}
	.runs li + li {
		border-top: 1px solid var(--rule);
	}
	.runs button {
		display: grid;
		grid-template-columns: 6rem minmax(0, 1fr) 8rem minmax(0, 14rem);
		gap: 0.75rem;
		align-items: center;
		width: 100%;
		padding: 0.55rem 0.85rem;
		border: 0;
		background: none;
		text-align: left;
		cursor: pointer;
		font-size: 0.8125rem;
	}
	.runs button:hover {
		background: var(--hover);
	}
	.runs code {
		font-family: var(--font-mono);
		font-size: 0.75rem;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.when,
	.rows {
		color: var(--ink-2);
		font-size: 0.75rem;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.outcome.ok {
		color: var(--green);
	}
	.outcome.refused,
	.outcome.failed {
		color: var(--red);
	}
	.outcome.timeout {
		color: var(--marker-ink);
	}
	.error {
		margin: 0;
		color: var(--red);
		font-size: 0.875rem;
	}

	.chartctl {
		display: flex;
		flex-wrap: wrap;
		align-items: flex-end;
		gap: 0.75rem 1.25rem;
	}
	.pick {
		display: grid;
		gap: 0.3rem;
		font-size: 0.75rem;
		color: var(--ink-2);
	}
	.pick .select {
		min-width: 10rem;
		color: var(--ink);
		font-size: 0.875rem;
	}
	.ys {
		display: flex;
		flex-wrap: wrap;
		gap: 0.35rem 0.9rem;
		margin: 0;
		padding: 0;
		border: 0;
		font-size: 0.875rem;
	}
	.ys legend {
		float: left;
		width: 100%;
		margin-bottom: 0.3rem;
		font-size: 0.75rem;
		color: var(--ink-2);
	}
	.ys label {
		display: flex;
		align-items: center;
		gap: 0.35rem;
		font-family: var(--font-mono);
		font-size: 0.8125rem;
	}
	.chartbox {
		padding: 1rem 1.25rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
	}
	.save {
		width: min(32rem, calc(100vw - 2rem));
		padding: 0;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--ground);
		color: var(--ink);
		box-shadow: var(--shadow-pop);
	}
	.save::backdrop {
		background: rgb(10 12 20 / 0.35);
	}
	.save form {
		display: grid;
		gap: 0.75rem;
		padding: 1.25rem;
	}
	.save h2 {
		margin: 0;
		font-size: 1.05rem;
		font-weight: 650;
	}
	.lead,
	.help {
		margin: 0;
		font-size: 0.8125rem;
		color: var(--ink-2);
	}
	.help {
		margin-top: -0.4rem;
	}
	.check {
		display: flex;
		gap: 0.5rem;
		align-items: center;
		font-size: 0.875rem;
	}
	.actions {
		display: flex;
		justify-content: flex-end;
		gap: 0.5rem;
	}

	@media (max-width: 40rem) {
		.tables {
			order: 2;
			flex-basis: 100%;
			position: static;
			max-height: none;
			border-right: 0;
			border-top: 1px solid var(--rule);
		}
		.work {
			padding: 0.75rem 1rem 1rem;
		}
		.runs button {
			grid-template-columns: 5rem minmax(0, 1fr);
		}
		.runs .when,
		.runs .rows {
			display: none;
		}
	}
</style>
