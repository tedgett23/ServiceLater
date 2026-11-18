<script lang="ts">
	import { lifecycle } from '$lib/lifecycle.svelte';
	import { parseValue, valueText, type RecordQuery } from './workflow';

	/**
	 * What a query step or a schedule finds: a record type, filters (field, operator, value: text or =an expression),
	 * and how many at most. The server checks the fields against what records can be found by.
	 */
	let {
		query,
		editable,
		fixedType = '',
		idPrefix,
		onchange
	}: { query: RecordQuery; editable: boolean; fixedType?: string; idPrefix: string; onchange: (q: RecordQuery) => void } = $props();

	const fields = $derived([
		'status',
		'state_category',
		'assignee_id',
		'assignment_group_id',
		'requester_id',
		'parent_id',
		'created_at',
		'updated_at',
		'id',
		...(query.type === 'change' ? ['change_type', 'planned_start', 'planned_end'] : [])
	]);
	const ops = [
		{ op: 'eq', label: 'is' },
		{ op: 'ne', label: 'is not' },
		{ op: 'lt', label: 'before / less than' },
		{ op: 'lte', label: 'at most' },
		{ op: 'gt', label: 'after / more than' },
		{ op: 'gte', label: 'at least' },
		{ op: 'in', label: 'is one of (=list)' }
	];

	type Row = { field: string; op: string; value: string };
	const rows = $derived(
		Object.entries(query.where ?? {}).flatMap(([field, byOp]) => Object.entries(byOp).map(([op, v]) => ({ field, op, value: valueText(v) })))
	);

	function emit(next: Row[], patch: Partial<RecordQuery> = {}) {
		const where: Record<string, Record<string, { expr?: string; literal?: string | number | boolean }>> = {};
		for (const r of next) {
			if (!r.field || !r.op) continue;
			(where[r.field] ??= {})[r.op] = parseValue(r.value) ?? { literal: '' };
		}
		const q: RecordQuery = { ...query, ...patch, where };
		if (!Object.keys(where).length) delete q.where;
		onchange(q);
	}
	const edit = (i: number, patch: Partial<Row>) => emit(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
</script>

<div class="query">
	{#if !fixedType}
		<div class="field">
			<label for="{idPrefix}-type">Find</label>
			<select id="{idPrefix}-type" value={query.type} disabled={!editable} onchange={(e) => emit(rows, { type: e.currentTarget.value })}>
				<option value="">Choose a type</option>
				{#each lifecycle.types as t (t.type)}<option value={t.type}>{t.plural}</option>{/each}
			</select>
		</div>
	{/if}
	<fieldset>
		<legend>Where</legend>
		{#each rows as r, i (i)}
			<div class="row">
				<select aria-label="Filter {i + 1} field" value={r.field} disabled={!editable} onchange={(e) => edit(i, { field: e.currentTarget.value })}>
					{#each fields as f (f)}<option value={f}>{f}</option>{/each}
				</select>
				<select aria-label="Filter {i + 1} test" value={r.op} disabled={!editable} onchange={(e) => edit(i, { op: e.currentTarget.value })}>
					{#each ops as o (o.op)}<option value={o.op}>{o.label}</option>{/each}
				</select>
				<input aria-label="Filter {i + 1} value" value={r.value} disabled={!editable} placeholder="open, or =now - duration('168h')" onchange={(e) => edit(i, { value: e.currentTarget.value })} />
				{#if editable}<button type="button" class="btn" aria-label="Remove filter {i + 1}" onclick={() => emit(rows.filter((_, j) => j !== i))}>×</button>{/if}
			</div>
		{/each}
		{#if editable}
			<button type="button" class="btn" onclick={() => emit([...rows, { field: 'state_category', op: 'eq', value: 'open' }])}>Add a filter</button>
		{/if}
		<small class="hint">All must hold. Times are =expressions: =now, =now - duration('24h').</small>
	</fieldset>
	<div class="field">
		<label for="{idPrefix}-limit">At most</label>
		<input id="{idPrefix}-limit" type="number" min="1" max="1000" value={query.limit ?? ''} placeholder="100" disabled={!editable}
			onchange={(e) => emit(rows, { limit: Number(e.currentTarget.value) || undefined })} />
	</div>
</div>

<style>
	.query {
		display: grid;
		gap: 0.6rem;
	}
	fieldset {
		display: grid;
		gap: 0.4rem;
		margin: 0;
		padding: 0;
		border: 0;
		min-width: 0;
	}
	legend {
		padding: 0;
		font-size: 0.8125rem;
		font-weight: 600;
		color: var(--ink-2);
		margin-bottom: 0.25rem;
	}
	.row {
		display: grid;
		grid-template-columns: minmax(0, 1.1fr) minmax(0, 1fr) minmax(0, 1.3fr) auto;
		gap: 0.3rem;
	}
	.row select,
	.row input {
		min-width: 0;
		min-height: 2.1rem;
		padding: 0.3rem 0.4rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		font-size: 0.8125rem;
		color: var(--ink);
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.8rem;
	}
	.btn {
		justify-self: start;
	}
</style>
