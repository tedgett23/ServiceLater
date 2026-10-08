<script lang="ts">
	import { onMount } from 'svelte';
	import { api, ApiError, download } from '$lib/api';
	import { cmdb } from '$lib/cmdb.svelte';
	import type { FormField } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	/** What an import does, or would do, with one row (POST /cmdb/csv/...). */
	interface ImportRow {
		row: number;
		name: string;
		class?: string;
		action: 'create' | 'update' | 'unchanged' | 'error';
		id?: string;
		changes?: string[];
		problem?: string;
	}
	interface ImportReport {
		applied: boolean;
		counts: Record<ImportRow['action'], number>;
		rows: ImportRow[];
	}

	// What is imported: one class's items, or relationships between items ('' = relationships).
	let what = $state('');
	let file = $state<File | null>(null);
	let report = $state<ImportReport | null>(null);
	let done = $state<ImportReport | null>(null);
	let skip = $state(false);
	let showAll = $state(false);
	let busy = $state(false);
	let error = $state('');
	let input = $state<HTMLInputElement>();

	const klass = $derived(what ? cmdb.classOf(what) : undefined);
	// The classes extending it share its file, with a class column saying which a row is.
	const family = $derived(klass ? [klass, ...(cmdb.model?.classes ?? []).filter((c) => c.ancestors.includes(klass.name))] : []);
	// Built here: Svelte drops a leading space inside {#if}.
	const familyNote = $derived(
		klass && family.length > 1 ? ` This file also covers the classes that extend ${klass.label}: ${family.slice(1).map((c) => c.label).join(', ')}.` : ''
	);
	/** The fields only some of the family have, and which classes have them. */
	const extraFields = $derived.by(() => {
		if (!klass) return [];
		const own = new Set(klass.fields.map((f) => f.name));
		const out = new Map<string, { field: FormField; classes: string[] }>();
		for (const c of family.slice(1)) {
			for (const f of c.fields) {
				if (own.has(f.name)) continue;
				const e = out.get(f.name) ?? { field: f, classes: [] };
				e.classes.push(c.label);
				out.set(f.name, e);
			}
		}
		return [...out.values()];
	});
	const path = $derived(what ? `/api/v1/cmdb/csv/items/${what}` : '/api/v1/cmdb/csv/relationships');
	const toDo = $derived(report ? report.counts.create + report.counts.update : 0);
	const shown = $derived(report ? report.rows.filter((r) => showAll || r.action !== 'unchanged') : []);
	const SHOW = 300;

	onMount(() => {
		void cmdb.ensure().then(() => {
			if (!what && cmdb.model?.classes.length) what = cmdb.tree()[0]?.cls.name ?? '';
		});
	});

	// Choosing something else starts again.
	function reset() {
		report = null;
		file = null;
		skip = false;
		error = '';
		if (input) input.value = '';
	}

	async function fetchFile(template: boolean) {
		error = '';
		try {
			await download(`${path}${template ? '?template=true' : ''}`, `${what || 'relationships'}.csv`);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not download the file';
		}
	}

	async function send(apply: boolean) {
		if (!file || busy) return;
		busy = true;
		error = '';
		const q = new URLSearchParams();
		if (apply) q.set('apply', 'true');
		if (apply && skip) q.set('skip_errors', 'true');
		try {
			const r = await api<ImportReport>(`${path}?${q}`, { method: 'POST', body: file });
			if (apply) {
				done = r;
				ui.announce = 'Imported';
				reset();
			} else {
				report = r;
				done = null;
			}
		} catch (e) {
			if (e instanceof ApiError && e.status === 422 && (e.body as ImportReport)?.rows) {
				report = e.body as ImportReport; // refused for its problems: show them
			} else {
				error = e instanceof Error ? e.message : 'Could not read the file';
				report = null;
			}
		} finally {
			busy = false;
		}
	}

	function choose(e: Event) {
		file = (e.currentTarget as HTMLInputElement).files?.[0] ?? null;
		report = null;
		done = null;
		if (file) void send(false);
	}

	/** What a field's cells take, in words. */
	function takes(f: FormField): string {
		switch (f.type) {
			case 'number':
				return `A whole number${f.min !== undefined ? `, at least ${f.min}` : ''}${f.max !== undefined ? `, at most ${f.max}` : ''}`;
			case 'boolean':
				return 'Yes or no';
			case 'date':
				return 'A date, as YYYY-MM-DD';
			case 'datetime':
				return 'A date and time, as YYYY-MM-DD HH:MM (UTC)';
			case 'choice':
				return `One of: ${(f.options ?? []).map((o) => o.label).join(', ')}`;
			default:
				return f.max_length ? `Text, up to ${f.max_length} characters` : 'Text';
		}
	}

	function outcome(r: ImportRow): string {
		switch (r.action) {
			case 'create':
				return 'Added';
			case 'update':
				return `Changes ${(r.changes ?? []).map((c) => klass?.fields.find((f) => f.name === c)?.label ?? c.replace('_', ' ')).join(', ')}`;
			case 'unchanged':
				return 'No change';
			default:
				return r.problem ?? 'Problem';
		}
	}

	function summary(c: ImportReport['counts'], applied: boolean): string {
		const parts = [
			c.create && `${c.create} ${applied ? 'added' : 'to add'}`,
			c.update && `${c.update} ${applied ? 'changed' : 'to change'}`,
			c.unchanged && `${c.unchanged} unchanged`,
			c.error && `${c.error} ${applied ? 'skipped' : c.error === 1 ? 'with a problem' : 'with problems'}`
		].filter(Boolean);
		return parts.length ? parts.join(', ') : 'Nothing in the file';
	}
</script>

<svelte:head><title>Import items – ServiceLater</title></svelte:head>

<div class="page">
	<a class="back" href="/cmdb">
		<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
		Configuration items
	</a>
	<h1>Import items</h1>
	<p class="lede">
		Add and update items from a spreadsheet: download a template, fill it in, save it as CSV and upload it. You see what
		each row will do before anything changes.
	</p>

	<section aria-labelledby="what-h">
		<h2 id="what-h">1. What to import</h2>
		<div class="field">
			<label for="imp-what">Import</label>
			<select id="imp-what" bind:value={what} onchange={reset}>
				<optgroup label="Items of a class">
					{#each cmdb.tree() as { cls: c, depth } (c.name)}
						<option value={c.name}>{' '.repeat(depth)}{c.label}</option>
					{/each}
				</optgroup>
				<option value="">Relationships between items</option>
			</select>
		</div>
		<div class="row">
			<button class="btn primary" type="button" onclick={() => fetchFile(true)}>Download the template</button>
			<button class="btn" type="button" onclick={() => fetchFile(false)}>
				{klass ? `Download the ${klass.label.toLowerCase()} items there are` : 'Download the relationships there are'}
			</button>
		</div>

		<details>
			<summary>What each column takes</summary>
			{#if klass}
				<p class="help">
					Each row is one item, matched to an existing one of its class by name (capitals aside): a new name adds an item,
					a known one changes it. An empty cell leaves that value as it is. Nothing is ever removed. Headers can be the
					column names or their labels.{familyNote}
				</p>
				<dl>
					<dt><code>name</code> <span class="req">required</span></dt>
					<dd>The item's name, unique within its class</dd>
					{#if family.length > 1}
						<dt><code>class</code></dt>
						<dd>
							Which class the row is: {family.map((c) => c.label).join(', ')}. Empty means {klass.label}. To change an item
							that is already there, give its class
						</dd>
					{/if}
					<dt><code>status</code></dt>
					<dd>One of: {(cmdb.model?.statuses ?? []).map((s) => s.label).join(', ')}. New items are Active unless you say</dd>
					<dt><code>description</code></dt>
					<dd>Text</dd>
					<dt><code>owner_group</code>, <code>support_group</code></dt>
					<dd>A group's name</dd>
					{#each klass.fields as f (f.name)}
						<dt><code>{f.name}</code> {f.label}{#if f.required}<span class="req"> required</span>{/if}</dt>
						<dd>{takes(f)}</dd>
					{/each}
					{#each extraFields as { field: f, classes } (f.name)}
						<dt><code>{f.name}</code> {f.label}</dt>
						<dd>{takes(f)}. {classes.join(', ')} only</dd>
					{/each}
				</dl>
			{:else}
				<p class="help">
					Each row relates two items that already exist (import the items first), reading
					<em>from</em> &lt;type&gt; <em>to</em>: "Payroll depends on pg-01". A relationship that is already there is left
					alone; nothing is ever removed.
				</p>
				<dl>
					<dt><code>from_class</code>, <code>to_class</code></dt>
					<dd>A class's name or label</dd>
					<dt><code>from_name</code>, <code>to_name</code></dt>
					<dd>The item's name</dd>
					<dt><code>type</code></dt>
					<dd>One of: {(cmdb.model?.relationships ?? []).map((r) => r.label).join(', ')}</dd>
				</dl>
			{/if}
		</details>
	</section>

	<section aria-labelledby="upload-h">
		<h2 id="upload-h">2. Upload the file</h2>
		<div class="field">
			<label for="imp-file">CSV file</label>
			<input id="imp-file" type="file" accept=".csv,text/csv" bind:this={input} onchange={choose} disabled={busy} />
		</div>
		{#if busy && !report}<p class="help" aria-live="polite">Reading the file…</p>{/if}
		{#if error}<p class="error" role="alert">{error}</p>{/if}
		{#if done}
			<p class="done" role="status">
				Imported: {summary(done.counts, true)}. <a href="/cmdb">See the items</a>
			</p>
		{/if}
	</section>

	{#if report}
		<section aria-labelledby="check-h">
			<h2 id="check-h">3. Check and import</h2>
			<p class="counts" role="status">{summary(report.counts, false)}.</p>
			{#if report.rows.length}
				<label class="check"><input type="checkbox" bind:checked={showAll} /> Show unchanged rows too</label>
				<ul class="rows" aria-label="Rows">
					{#each shown.slice(0, SHOW) as r (r.row)}
						<li class={r.action}>
							<span class="line">Row {r.row}</span>
							<span class="name">{r.name || '(no name)'}{#if r.class && family.length > 1}<span class="cls">{cmdb.classOf(r.class)?.label ?? r.class}</span>{/if}</span>
							<span class="what">{outcome(r)}</span>
						</li>
					{/each}
				</ul>
				{#if shown.length > SHOW}<p class="help">And {shown.length - SHOW} more.</p>{/if}
			{/if}
			{#if report.counts.error}
				<p class="help">
					Fix the rows with problems and upload the file again, or import the rest and leave those out.
				</p>
				<label class="check"><input type="checkbox" bind:checked={skip} /> Leave out the rows with problems</label>
			{/if}
			<div class="row">
				<button class="btn primary" type="button" disabled={busy || toDo === 0 || (report.counts.error > 0 && !skip)} onclick={() => send(true)}>
					{toDo === 0 ? 'Nothing to import' : `Import ${toDo} ${toDo === 1 ? 'row' : 'rows'}`}
				</button>
				<button class="btn" type="button" onclick={reset} disabled={busy}>Start again</button>
			</div>
		</section>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 56rem;
	}
	.back {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		margin-bottom: 0.75rem;
		font-weight: 600;
		text-decoration: none;
	}
	h1 {
		font: 800 1.6rem/1.2 var(--font-display);
		margin-bottom: 0.4rem;
	}
	.lede {
		color: var(--ink-2);
		margin-bottom: 1.25rem;
	}
	h2 {
		font: 700 1.15rem/1.2 var(--font-display);
		margin-bottom: 0.75rem;
	}
	section {
		display: grid;
		gap: 0.75rem;
		padding: 1.25rem 0;
		border-top: 1px solid var(--rule);
	}
	.field {
		max-width: 24rem;
	}
	.row {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
	}
	.help {
		font-size: 0.85rem;
		color: var(--ink-2);
	}
	details summary {
		cursor: pointer;
		font-weight: 600;
	}
	details[open] summary {
		margin-bottom: 0.6rem;
	}
	dl {
		display: grid;
		grid-template-columns: minmax(10rem, max-content) 1fr;
		gap: 0.35rem 1rem;
		margin-top: 0.6rem;
		font-size: 0.9rem;
	}
	dd {
		margin: 0;
		color: var(--ink-2);
	}
	.req {
		color: var(--red);
		font-size: 0.8rem;
	}
	.check {
		display: inline-flex;
		align-items: center;
		gap: 0.45rem;
		font-size: 0.9rem;
	}
	.counts {
		font-weight: 600;
	}
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
		border: 1px solid var(--rule);
		border-radius: 4px;
		max-height: 28rem;
		overflow-y: auto;
		background: var(--surface);
	}
	.rows li {
		display: grid;
		grid-template-columns: 4.5rem minmax(8rem, 1fr) 2fr;
		gap: 0.75rem;
		padding: 0.4rem 0.75rem;
		border-bottom: 1px solid var(--rule);
		font-size: 0.9rem;
		overflow-wrap: anywhere;
	}
	.rows li:last-child {
		border-bottom: 0;
	}
	.line {
		color: var(--ink-2);
	}
	.name {
		font-weight: 600;
	}
	.cls {
		margin-left: 0.5rem;
		font-weight: 400;
		font-size: 0.8rem;
		color: var(--ink-2);
	}
	.rows li.error {
		box-shadow: inset 3px 0 0 var(--red);
	}
	.rows li.error .what {
		color: var(--red);
	}
	.rows li.unchanged .what {
		color: var(--ink-2);
	}
	.error {
		padding: 0.5rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
	}
	.done {
		padding: 0.5rem 0.75rem;
		border-left: 3px solid var(--marker);
		background: var(--marker-soft);
	}
	@media (max-width: 40rem) {
		dl {
			grid-template-columns: 1fr;
		}
		dd {
			margin-bottom: 0.4rem;
		}
		.rows li {
			grid-template-columns: 1fr;
			gap: 0.1rem;
		}
	}
</style>
