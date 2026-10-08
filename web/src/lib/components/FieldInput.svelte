<script lang="ts">
	import { priorityLabel } from '$lib/format';
	import type { FieldState, FieldValue, FormField } from '$lib/types';

	/**
	 * One form field's control, by its type. It reports a value of the field's kind: text, a whole number,
	 * true/false, a date as YYYY-MM-DD or a time as ISO, and null when emptied. With `live` it reports every keystroke
	 * (a form being filled in); otherwise only when the person is done with the field (an edit that saves).
	 */
	let {
		field,
		value,
		fieldState = {},
		id,
		disabled = false,
		live = false,
		autofocus = false,
		onchange
	}: {
		field: FormField;
		value: FieldValue | null;
		fieldState?: FieldState;
		id: string;
		disabled?: boolean;
		live?: boolean;
		autofocus?: boolean;
		onchange: (v: FieldValue | null) => void;
	} = $props();

	// Typed text is kept here while the box has focus, so a live update cannot overwrite it mid-sentence.
	let draft = $state('');
	let focused = false;
	$effect(() => {
		const v = value;
		if (!focused) draft = v === null ? '' : field.type === 'datetime' ? local(String(v)) : String(v);
	});

	/** An ISO time as a datetime-local input wants it, in the browser's time zone. */
	function local(iso: string) {
		if (!iso) return '';
		const d = new Date(iso);
		const pad = (n: number) => String(n).padStart(2, '0');
		return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
	}

	function parse(text: string): FieldValue | null {
		if (text === '') return null;
		switch (field.type) {
			case 'number':
			case 'priority': {
				const n = Number(text);
				return Number.isFinite(n) ? n : text;
			}
			case 'datetime':
				return new Date(text).toISOString();
		}
		return text;
	}

	function commit(text: string) {
		const v = parse(text);
		if (v === value || (v === null && (value === null || value === ''))) return;
		onchange(v);
	}

	// A choice offers the options the server says apply now, and always the one it has.
	const options = $derived(
		(field.options ?? []).filter((o) => !fieldState.options || fieldState.options.includes(o.value) || o.value === value)
	);
	const hasEmpty = $derived((field.options ?? []).some((o) => o.value === ''));
	const required = $derived(!!(fieldState.required ?? field.required));
	const describedBy = $derived(field.help ? `${id}-help` : undefined);
</script>

{#if field.type === 'textarea'}
	<!-- svelte-ignore a11y_autofocus -->
	<textarea
		{id}
		{disabled}
		{required}
		{autofocus}
		maxlength={field.max_length || 10000}
		aria-describedby={describedBy}
		bind:value={draft}
		onfocus={() => (focused = true)}
		oninput={() => live && onchange(parse(draft))}
		onblur={() => {
			focused = false;
			if (!live) commit(draft);
		}}
	></textarea>
{:else if field.type === 'choice' || field.type === 'priority'}
	<select
		{id}
		{disabled}
		{required}
		aria-describedby={describedBy}
		value={value === null ? '' : String(value)}
		onchange={(e) => commit(e.currentTarget.value)}
	>
		{#if field.type === 'priority'}
			{#each [1, 2, 3, 4] as const as p (p)}<option value={String(p)}>{priorityLabel[p]}</option>{/each}
		{:else}
			{#if !hasEmpty}<option value="">{required ? 'Choose…' : 'Not set'}</option>{/if}
			{#each options as o (o.value)}<option value={o.value}>{o.label}</option>{/each}
		{/if}
	</select>
{:else if field.type === 'boolean'}
	<input
		{id}
		type="checkbox"
		{disabled}
		aria-describedby={describedBy}
		checked={value === true}
		onchange={(e) => onchange(e.currentTarget.checked)}
	/>
{:else}
	<!-- svelte-ignore a11y_autofocus -->
	<input
		{id}
		type={field.type === 'number' ? 'number' : field.type === 'date' ? 'date' : field.type === 'datetime' ? 'datetime-local' : 'text'}
		{disabled}
		{required}
		{autofocus}
		min={field.min}
		max={field.max}
		step={field.type === 'number' ? 1 : undefined}
		maxlength={field.type === 'text' ? field.max_length || (field.name === 'title' ? 200 : 1000) : undefined}
		aria-describedby={describedBy}
		bind:value={draft}
		onfocus={() => (focused = true)}
		oninput={() => live && onchange(parse(draft))}
		onchange={() => !live && commit(draft)}
		onblur={() => (focused = false)}
	/>
{/if}
{#if field.help}<small class="help" id="{id}-help">{field.help}</small>{/if}

<style>
	textarea {
		min-height: 4.5rem;
	}
	input[type='checkbox'] {
		justify-self: start;
		width: 1.15rem;
		min-height: 1.15rem;
		accent-color: var(--blue);
	}
	:disabled {
		opacity: 0.7;
		cursor: not-allowed;
	}
	.help {
		font-size: 0.8rem;
		color: var(--ink-2);
	}
</style>
