<script lang="ts">
	import { untrack } from 'svelte';
	import { opLabels, opsFor, parseSimple, toExpression, type Clause, type SimpleField } from '$lib/conditions';

	/**
	 * When a field is shown, required or read-only: always or never, a simple condition built from the form's own fields
	 * ("Category is Hardware"), or a CEL expression for anything more. A simple condition is kept as the expression it
	 * stands for, shown underneath so people learn the syntax as they go.
	 */
	let {
		legend,
		expression = $bindable(''),
		always = $bindable(false),
		empty,
		offersAlways = false,
		fields,
		placeholder = ''
	}: {
		/** Shown, Required, Read-only. The expression's box is "<legend> when". */
		legend: string;
		expression?: string;
		/** Set for "Always" (required: true); only when offersAlways. */
		always?: boolean;
		/** What no condition means: the field is always shown, or never required or read-only. */
		empty: 'always' | 'never';
		/** Offer both Never and Always (required). */
		offersAlways?: boolean;
		fields: SimpleField[];
		placeholder?: string;
	} = $props();

	type Mode = 'always' | 'never' | 'simple' | 'expression';
	const modes = $derived<Mode[]>(offersAlways ? ['never', 'always', 'simple', 'expression'] : [empty, 'simple', 'expression']);
	const modeLabel: Record<Mode, string> = { always: 'Always', never: 'Never', simple: 'Simple', expression: 'Expression' };

	const start = untrack(() => {
		if (always) return { mode: 'always' as Mode, clauses: [] as Clause[] };
		if (!expression.trim()) return { mode: empty as Mode, clauses: [] as Clause[] };
		const c = parseSimple(expression, fields);
		return c ? { mode: 'simple' as Mode, clauses: c } : { mode: 'expression' as Mode, clauses: [] as Clause[] };
	});
	let mode = $state<Mode>(start.mode);
	let clauses = $state<Clause[]>(start.clauses);

	function firstClause(): Clause {
		const f = fields[0];
		return { field: f?.name ?? '', op: 'is', value: f?.options?.[0]?.value ?? (f?.type === 'boolean' ? true : '') };
	}

	function choose(m: Mode) {
		if (m === mode) return;
		if (m === 'always' || m === 'never') {
			always = m === 'always' && offersAlways;
			expression = '';
		} else {
			always = false;
		}
		if (m === 'simple') {
			clauses = parseSimple(expression, fields) ?? (fields.length ? [firstClause()] : []);
			expression = toExpression(clauses, fields);
		}
		mode = m;
	}

	function changed() {
		expression = toExpression(clauses, fields);
	}

	function setField(c: Clause, name: string) {
		const f = fields.find((x) => x.name === name)!;
		c.field = name;
		if (!opsFor(f).includes(c.op)) c.op = 'is';
		c.value = f.options?.[0]?.value ?? (f.type === 'boolean' ? true : '');
		changed();
	}

	const fieldOf = (c: Clause) => fields.find((f) => f.name === c.field);
</script>

<fieldset class="cond">
	<div class="head">
		<legend>{legend}</legend>
		<div class="segmented" role="group" aria-label="{legend}: how">
			{#each modes as m (m)}
				<button type="button" aria-pressed={mode === m} onclick={() => choose(m)}>{modeLabel[m]}</button>
			{/each}
		</div>
	</div>
	{#if mode === 'simple'}
		{#if fields.length === 0}
			<p class="help">There are no fields to build a condition from yet: add a choice, yes-or-no, text or number field, or write an expression.</p>
		{/if}
		{#each clauses as c, i (i)}
			{@const f = fieldOf(c)}
			<div class="clause">
				<span class="word">{i === 0 ? 'when' : 'and'}</span>
				<select class="select" aria-label="{legend}: field {i + 1}" value={c.field} onchange={(e) => setField(c, e.currentTarget.value)}>
					{#each fields as x (x.name)}<option value={x.name}>{x.label}</option>{/each}
				</select>
				<select
					class="select"
					aria-label="{legend}: test {i + 1}"
					value={c.op}
					onchange={(e) => {
						c.op = e.currentTarget.value as Clause['op'];
						changed();
					}}
				>
					{#each f ? opsFor(f) : [] as op (op)}<option value={op}>{opLabels[op]}</option>{/each}
				</select>
				{#if c.op === 'is' || c.op === 'is_not'}
					{#if f?.options}
						<select
							class="select"
							aria-label="{legend}: value {i + 1}"
							value={String(c.value)}
							onchange={(e) => {
								c.value = f.type === 'priority' ? Number(e.currentTarget.value) : e.currentTarget.value;
								changed();
							}}
						>
							{#each f.options as o (o.value)}<option value={o.value}>{o.label}</option>{/each}
						</select>
					{:else if f?.type === 'boolean'}
						<select
							class="select"
							aria-label="{legend}: value {i + 1}"
							value={String(c.value)}
							onchange={(e) => {
								c.value = e.currentTarget.value === 'true';
								changed();
							}}
						>
							<option value="true">Yes</option>
							<option value="false">No</option>
						</select>
					{:else}
						<input
							class="select"
							aria-label="{legend}: value {i + 1}"
							type={f?.type === 'number' ? 'number' : 'text'}
							value={c.value ?? ''}
							oninput={(e) => {
								c.value = f?.type === 'number' ? Number(e.currentTarget.value) : e.currentTarget.value;
								changed();
							}}
						/>
					{/if}
				{/if}
				{#if clauses.length > 1}
					<button
						type="button"
						class="btn quiet"
						aria-label="Remove condition {i + 1}"
						onclick={() => {
							clauses.splice(i, 1);
							changed();
						}}>×</button
					>
				{/if}
			</div>
		{/each}
		{#if fields.length}
			<button
				type="button"
				class="link"
				onclick={() => {
					clauses.push(firstClause());
					changed();
				}}>+ and</button
			>
			<code class="cel">{expression}</code>
		{/if}
	{:else if mode === 'expression'}
		<label class="field">
			<span class="sr-only">{legend} when</span>
			<input aria-label="{legend} when" class="mono" bind:value={expression} {placeholder} spellcheck="false" autocomplete="off" />
		</label>
	{/if}
</fieldset>

<style>
	.cond {
		border: 1px solid var(--rule);
		border-radius: 8px;
		padding: 0.6rem 0.75rem;
		background: var(--ground);
		display: grid;
		gap: 0.5rem;
		min-width: 0;
		margin: 0;
	}
	.head {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
	}
	legend {
		float: left;
		padding: 0;
		font-weight: 600;
		flex: 1;
	}
	.segmented {
		font-size: 0.8125rem;
	}
	.clause {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		flex-wrap: wrap;
	}
	.clause .select {
		width: auto;
		min-width: 0;
		max-width: 100%;
		min-height: 2rem;
		padding: 0.25rem 0.5rem;
	}
	.clause input.select {
		width: 9rem;
	}
	.word {
		color: var(--ink-2);
		font-size: 0.875rem;
		min-width: 2.2rem;
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
	.cel {
		font: 0.75rem/1.4 var(--font-mono);
		color: var(--ink-2);
		overflow-wrap: anywhere;
	}
	.mono {
		font-family: var(--font-mono);
		font-size: 0.8125rem;
	}
	.help {
		color: var(--ink-2);
		font-size: 0.8125rem;
	}
</style>
