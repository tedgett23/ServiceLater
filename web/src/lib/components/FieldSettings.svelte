<script lang="ts">
	import ConditionInput from '$lib/components/ConditionInput.svelte';
	import { nameFrom, parseValue, questionTypes, valueText, type DefField } from '$lib/catalogEditor';
	import type { SimpleField } from '$lib/conditions';

	/**
	 * Everything about one field of a form or one catalog question: what it is called and asks for, its choices, when it
	 * is shown, required or read-only, and its limits. Shared by the form editor and the catalog editor, so the two
	 * never differ. A record's own field (core) can only be relabelled, explained and given conditions.
	 */
	let {
		field = $bindable(),
		labels = { label: 'Label', type: 'Type' },
		core,
		plain = false,
		saved = false,
		values = 0,
		others = [],
		fields,
		history = false,
		problems = [],
		onrename
	}: {
		field: DefField;
		/** What the label and type boxes are called (the catalog asks "Question" and "Kind of answer"). */
		labels?: { label: string; type: string };
		/** For a record's own field: its built-in label and kind. */
		core?: { label: string; type: string };
		/** A field with no kind of its own (a request's description): no name, kind or default. */
		plain?: boolean;
		/** The field was saved before: its name is fixed (values are kept by name). */
		saved?: boolean;
		/** How many records hold a value for it: then its kind is fixed too (long and short text may swap). */
		values?: number;
		/** Names other fields have. */
		others?: string[];
		/** Fields its conditions can be built from. */
		fields: SimpleField[];
		/** Offer the "record changes in the history" switch. */
		history?: boolean;
		/** What the server says is wrong with it. */
		problems?: string[];
		/** Told when the field's name changes, so whatever places it by name can follow. */
		onrename?: (from: string, to: string) => void;
	} = $props();

	function rename(to: string) {
		const from = field.name;
		field.name = to;
		if (from !== to) onrename?.(from, to);
	}

	const text = ['text', 'textarea'];
	const types = $derived(
		values > 0 && field.type ? questionTypes.filter((t) => t.type === field.type || (text.includes(field.type!) && text.includes(t.type))) : questionTypes
	);

	function setLabel(v: string) {
		const auto = !field.label || field.name === nameFrom(field.label, others);
		field.label = v;
		// A new field's name follows its label until it is saved (values are kept by name).
		if (auto && !saved && !core && !plain && v.trim()) rename(nameFrom(v, others));
	}

	function setType(t: DefField['type']) {
		field.type = t;
		if (t === 'choice' && !field.options?.length) field.options = [{ value: '', label: '' }];
		if (t !== 'choice') delete field.options;
		if (t !== 'text' && t !== 'textarea') delete field.max_length;
		if (t !== 'number') {
			delete field.min;
			delete field.max;
		}
		delete field.default;
	}
</script>

<div class="settings">
	{#if problems.length}
		<ul class="problems" role="alert">
			{#each problems as p, i (i)}<li>{p}</li>{/each}
		</ul>
	{/if}
	<div class="two">
		<label class="field">
			<span>{labels.label}</span>
			<input value={field.label ?? ''} oninput={(e) => setLabel(e.currentTarget.value)} maxlength="100" placeholder={core?.label ?? ''} />
		</label>
		{#if core}
			<div class="field">
				<span>{labels.type}</span>
				<p class="fixed">Built in: {questionTypes.find((t) => t.type === core.type)?.label ?? core.type}</p>
			</div>
		{:else if !plain}
			<label class="field">
				<span>{labels.type}</span>
				<select value={field.type} onchange={(e) => setType(e.currentTarget.value as DefField['type'])}>
					{#each types as t (t.type)}<option value={t.type}>{t.label}</option>{/each}
				</select>
				{#if values > 0}<small class="help">Fixed: {values} record{values === 1 ? ' holds' : 's hold'} a value.</small>{/if}
			</label>
		{/if}
	</div>
	{#if !core && !plain}
		<label class="field">
			<span>Name</span>
			<input value={field.name} oninput={(e) => rename(e.currentTarget.value)} pattern="[a-z][a-z0-9_]*" maxlength="40" readonly={saved} class="mono" />
			<small class="help">
				Rules and workflows read it as record.fields.{field.name}{saved ? '. It is fixed now: values are kept by name.' : '.'}
			</small>
		</label>
	{/if}
	<label class="field">
		<span>Help</span>
		<input bind:value={field.help} maxlength="300" placeholder="A hint under the field" />
	</label>
	{#if field.type === 'choice' && !core}
		<fieldset class="options">
			<legend>Choices</legend>
			{#each field.options ?? [] as o, j (j)}
				<div class="option">
					<input
						class="select"
						aria-label="Choice {j + 1}"
						value={o.label}
						placeholder="What people see"
						oninput={(e) => {
							// The value kept follows the label until someone sets it apart.
							if (!o.value || o.value === o.label) o.value = e.currentTarget.value;
							o.label = e.currentTarget.value;
						}}
					/>
					<input class="select" aria-label="Choice {j + 1} value" bind:value={o.value} placeholder="Value kept" />
					<input class="select mono" aria-label="Choice {j + 1} offered when" bind:value={o.when} placeholder="Offered when (optional)" />
					<button type="button" class="btn quiet" onclick={() => field.options!.splice(j, 1)} aria-label="Remove choice {j + 1}">×</button>
				</div>
			{/each}
			<button type="button" class="btn" onclick={() => (field.options ??= []).push({ value: '', label: '' })}>Add a choice</button>
		</fieldset>
	{/if}

	<ConditionInput legend="Shown" bind:expression={() => field.visible_when ?? '', (v) => (field.visible_when = v || undefined)} empty="always" {fields} placeholder={'record.fields.category == "hardware"'} />
	<ConditionInput
		legend="Required"
		bind:expression={() => field.required_when ?? '', (v) => (field.required_when = v || undefined)}
		bind:always={() => !!field.required, (v) => (field.required = v || undefined)}
		empty="never"
		offersAlways
		{fields}
		placeholder={'record.status == "resolved"'}
	/>
	<ConditionInput legend="Read-only" bind:expression={() => field.read_only_when ?? '', (v) => (field.read_only_when = v || undefined)} empty="never" {fields} placeholder={'record.status == "closed"'} />

	{#if !core && !plain}
		<div class="two">
			<label class="field">
				<span>Starts as</span>
				<input value={valueText(field.default)} oninput={(e) => (field.default = parseValue(e.currentTarget.value, field.type))} placeholder={field.type === 'boolean' ? 'true or false' : 'Empty'} />
			</label>
			{#if field.type === 'text' || field.type === 'textarea'}
				<label class="field">
					<span>Longest answer</span>
					<input type="number" min="1" value={field.max_length ?? ''} oninput={(e) => (field.max_length = Number(e.currentTarget.value) || undefined)} placeholder="Characters" />
				</label>
			{/if}
			{#if field.type === 'number'}
				<label class="field">
					<span>At least</span>
					<input type="number" value={field.min ?? ''} oninput={(e) => (field.min = e.currentTarget.value === '' ? undefined : Number(e.currentTarget.value))} />
				</label>
				<label class="field">
					<span>At most</span>
					<input type="number" value={field.max ?? ''} oninput={(e) => (field.max = e.currentTarget.value === '' ? undefined : Number(e.currentTarget.value))} />
				</label>
			{/if}
		</div>
		{#if history}
			<label class="check"><input type="checkbox" checked={field.history !== false} onchange={(e) => (field.history = e.currentTarget.checked ? undefined : false)} /> Record changes to it in the history</label>
		{/if}
	{/if}
</div>

<style>
	.settings {
		display: grid;
		gap: 0.75rem;
		min-width: 0;
	}
	.two {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(12rem, 1fr));
		gap: 0.75rem;
	}
	.fixed {
		padding: 0.45rem 0;
		color: var(--ink-2);
	}
	.mono {
		font-family: var(--font-mono);
		font-size: 0.8125rem;
	}
	.options {
		display: grid;
		gap: 0.4rem;
		border: 0;
		padding: 0;
		margin: 0;
	}
	.options legend {
		font-weight: 600;
		font-size: 0.875rem;
		margin-bottom: 0.25rem;
	}
	.option {
		display: grid;
		grid-template-columns: 1fr 1fr 1.3fr auto;
		gap: 0.35rem;
	}
	.option .select {
		min-width: 0;
	}
	.check {
		display: flex;
		gap: 0.45rem;
		align-items: center;
	}
	.help {
		color: var(--ink-2);
		font-size: 0.8125rem;
	}
	.problems {
		margin: 0;
		padding: 0.5rem 0.75rem 0.5rem 1.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
		font-size: 0.875rem;
	}
	@media (max-width: 40rem) {
		.option {
			grid-template-columns: 1fr 1fr auto;
		}
		.option input:nth-child(3) {
			grid-column: 1 / 3;
		}
	}
</style>
