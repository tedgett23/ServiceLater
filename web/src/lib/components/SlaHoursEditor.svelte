<script lang="ts">
	import { untrack } from 'svelte';
	import { api, ApiError } from '$lib/api';
	import { calendarChoice } from '$lib/calendars';
	import type { SimpleField } from '$lib/conditions';
	import ConditionInput from './ConditionInput.svelte';
	import type { BusinessCalendar, CalendarRule, SlaDef } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	/**
	 * Which hours one SLA counts: rules in order, the first whose condition holds deciding, then what every other record
	 * counts. Saved as only that change in the app's edit layer; new clocks use it, running ones from their next save.
	 */
	let {
		sla,
		calendars,
		fields,
		onsaved
	}: {
		sla: SlaDef;
		calendars: BusinessCalendar[];
		/** What the conditions can test for this SLA's records. */
		fields: SimpleField[];
		onsaved: (s: SlaDef) => void;
	} = $props();

	interface Row {
		id: number;
		when: string;
		use: string;
	}
	let next = 0;
	/** The rules before the last, and what the rest count (the last rule, which always holds). */
	function rowsOf(rules: CalendarRule[]) {
		const all = rules.map((r) => ({ id: next++, when: r.when ?? '', use: r.use }));
		const last = all.at(-1);
		if (!last || last.when || last.use === 'group') return { rules: all, otherwise: '24x7' }; // nothing else matches: every hour
		return { rules: all.slice(0, -1), otherwise: last.use };
	}
	const start = untrack(() => rowsOf(sla.calendar));
	let rules = $state<Row[]>(start.rules);
	let otherwise = $state(start.otherwise);
	/** Bumped to start the condition inputs again from rules (after a save or undo). */
	let generation = $state(0);
	let problems = $state<string[]>([]);
	let saving = $state(false);
	let confirmingUndo = $state(false);

	const wanted = $derived<CalendarRule[]>([...rules.map((r) => (r.when.trim() ? { when: r.when.trim(), use: r.use } : { use: r.use })), { use: otherwise }]);
	const normal = (rs: CalendarRule[]) => JSON.stringify(rs.map((r) => (r.when ? { when: r.when, use: r.use } : { use: r.use })));
	const shipped = $derived(normal(sla.calendar));
	const dirty = $derived(normal(wanted) !== shipped);

	function reset(to: SlaDef) {
		const r = rowsOf(to.calendar);
		rules = r.rules;
		otherwise = r.otherwise;
		problems = [];
		generation++;
	}

	function move(i: number, by: number) {
		const [r] = rules.splice(i, 1);
		rules.splice(i + by, 0, r);
	}

	async function save() {
		saving = true;
		problems = [];
		try {
			const s = await api<SlaDef>(`/api/v1/slas/${sla.app}/${sla.name}/calendar`, { method: 'PUT', body: { calendar: wanted } });
			ui.announce = `${sla.label}: hours saved`;
			onsaved(s);
			reset(s);
		} catch (e) {
			const more = e instanceof ApiError ? (e.body as { problems?: string[] } | undefined)?.problems : undefined;
			problems = more?.length ? more : [e instanceof Error ? e.message : 'The hours did not save'];
		} finally {
			saving = false;
		}
	}

	async function undo() {
		problems = [];
		try {
			const s = await api<SlaDef>(`/api/v1/slas/${sla.app}/${sla.name}/edits`, { method: 'DELETE' });
			confirmingUndo = false;
			ui.announce = `${sla.label} counts its app's hours again`;
			onsaved(s);
			reset(s);
		} catch (e) {
			problems = [e instanceof Error ? e.message : 'Could not undo it'];
		}
	}

	const known = $derived(new Set(calendars.map((c) => c.name)));
</script>

{#snippet choices(value: string, withGroup: boolean)}
	<option value="24x7">Every hour · 24×7</option>
	{#each calendars as c (c.id)}<option value={c.name}>{calendarChoice(c)}</option>{/each}
	{#if withGroup}<option value="group">The assignment group's own hours, if it has them</option>{/if}
	{#if value !== '24x7' && value !== 'group' && !known.has(value)}<option {value}>{value} (no such calendar: every hour counts)</option>{/if}
{/snippet}

<section class="card" aria-labelledby="sla-{sla.app}-{sla.name}">
	<header>
		<div class="title">
			<h2 id="sla-{sla.app}-{sla.name}">{sla.label}</h2>
			<code>{sla.app}/{sla.name}</code>
			{#if sla.edited}<span class="badge">Changed here</span>{/if}
		</div>
		<div class="buttons">
			{#if sla.edited && !dirty}
				{#if confirmingUndo}
					<button class="btn" onclick={undo}>Count the app's hours</button>
					<button class="btn quiet" onclick={() => (confirmingUndo = false)}>Keep these</button>
				{:else}
					<button class="btn" onclick={() => (confirmingUndo = true)}>Undo changes</button>
				{/if}
			{/if}
			{#if dirty}
				<button class="btn" onclick={() => reset(sla)}>Discard</button>
				<button class="btn primary" onclick={save} disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
			{/if}
		</div>
	</header>
	<p class="sub">
		{sla.record_type_label} · target
		{#if sla.target}{sla.target}{:else}<span title={sla.target_rule}>set by the record</span>{/if}
	</p>

	{#key generation}
		<ol class="rules">
			{#each rules as r, i (r.id)}
				<li class="rule">
					<span class="n" aria-hidden="true">{i + 1}</span>
					<div class="body">
						<ConditionInput legend="Rule {i + 1}" bind:expression={r.when} empty="always" {fields} />
						<label class="use">
							<span>count</span>
							<select class="select" bind:value={r.use} aria-label="Rule {i + 1}: hours">{@render choices(r.use, true)}</select>
						</label>
					</div>
					<div class="order">
						<button class="btn quiet" aria-label="Move rule {i + 1} up" disabled={i === 0} onclick={() => move(i, -1)}>↑</button>
						<button class="btn quiet" aria-label="Move rule {i + 1} down" disabled={i === rules.length - 1} onclick={() => move(i, 1)}>↓</button>
						<button class="btn quiet" aria-label="Remove rule {i + 1}" onclick={() => rules.splice(i, 1)}>×</button>
					</div>
				</li>
			{/each}
			<li class="rule last">
				<span class="n" aria-hidden="true">→</span>
				<label class="use">
					<span>{rules.length ? 'Otherwise count' : 'Always count'}</span>
					<select class="select" bind:value={otherwise} aria-label="{rules.length ? 'Otherwise' : 'Always'}: hours">{@render choices(otherwise, false)}</select>
				</label>
			</li>
		</ol>
	{/key}
	<button class="link" onclick={() => rules.push({ id: next++, when: '', use: 'group' })}>+ Add a rule</button>

	{#if problems.length}
		<ul class="problems" role="alert">
			{#each problems as p, i (i)}<li>{p}</li>{/each}
		</ul>
	{/if}
</section>

<style>
	.card {
		border: 1px solid var(--rule);
		border-radius: 10px;
		background: var(--surface);
		padding: 0.9rem 1rem;
		display: grid;
		gap: 0.6rem;
		min-width: 0;
	}
	header {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		justify-content: space-between;
		gap: 0.5rem;
	}
	.title {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.5rem;
		min-width: 0;
	}
	h2 {
		font: 650 1rem/1.3 var(--font-display);
	}
	code {
		font-family: var(--font-mono);
		font-size: 0.75rem;
		color: var(--ink-2);
	}
	.badge {
		padding: 0.05rem 0.4rem;
		border-radius: 6px;
		background: var(--marker-soft);
		color: var(--marker-ink);
		font-size: 0.75rem;
		font-weight: 650;
	}
	.buttons {
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem;
	}
	.sub {
		color: var(--ink-2);
		font-size: 0.8125rem;
		margin-top: -0.35rem;
	}
	.sub span[title] {
		text-decoration: underline dotted;
		cursor: help;
	}
	.rules {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 0.5rem;
	}
	.rule {
		display: grid;
		grid-template-columns: 1.25rem minmax(0, 1fr) auto;
		gap: 0.5rem;
		align-items: start;
		padding: 0.55rem 0.6rem;
		border: 1px solid var(--rule);
		border-radius: 8px;
		background: var(--ground);
	}
	.rule.last {
		grid-template-columns: 1.25rem minmax(0, 1fr);
		align-items: center;
		border-style: dashed;
		background: transparent;
	}
	.n {
		color: var(--ink-2);
		font-weight: 650;
		font-size: 0.8125rem;
		padding-top: 0.45rem;
	}
	.rule.last .n {
		padding-top: 0;
	}
	.body {
		display: grid;
		gap: 0.5rem;
		min-width: 0;
	}
	.use {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.5rem;
		min-width: 0;
	}
	.use span {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.use .select {
		min-width: 0;
		max-width: 100%;
		flex: 1 1 16rem;
	}
	.order {
		display: flex;
		gap: 0.15rem;
	}
	.link {
		justify-self: start;
		background: none;
		border: 0;
		padding: 0;
		color: var(--blue);
		font-weight: 600;
		font-size: 0.875rem;
		cursor: pointer;
	}
	.problems {
		margin: 0;
		padding-left: 1.1rem;
		color: var(--red);
		font-size: 0.875rem;
	}
	@media (max-width: 40rem) {
		.rule {
			grid-template-columns: minmax(0, 1fr);
		}
		.n {
			display: none;
		}
		.rule.last {
			grid-template-columns: minmax(0, 1fr);
		}
	}
</style>
