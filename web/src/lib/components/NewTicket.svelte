<script lang="ts">
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import FieldInput from '$lib/components/FieldInput.svelte';
	import { wide } from '$lib/fields';
	import { forms } from '$lib/forms.svelte';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import { queue } from '$lib/queue.svelte';
	import type { FieldState, FieldValue, FormField, Ticket } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let dialog: HTMLDialogElement;
	// Which kind of record to file. The choice is offered only when the person may file more than one type.
	let chosen = $state('');
	const kind = $derived(lifecycle.fileable.some((t) => t.type === chosen) ? chosen : (lifecycle.fileable[0]?.type ?? 'incident'));
	let busy = $state(false);
	let error = $state('');

	// The type's form for filing: its fields in order, what has been filled in, and what the server says of each field
	// now (shown, required, writable, which options).
	const form = $derived(forms.of(kind));
	const fields = $derived(
		(form?.create ?? []).map((n) => form!.fields.find((f) => f.name === n)).filter((f): f is FormField => !!f)
	);
	let values = $state<Record<string, FieldValue | null>>({});
	let states = $state<Record<string, FieldState>>({});
	let filledFor = '';

	$effect(() => {
		for (const t of lifecycle.fileable) void forms.ensure(t.type);
	});

	// The dialog element owns focus trapping and Escape; we just mirror its open state.
	$effect(() => {
		if (ui.newTicketOpen && !dialog.open) dialog.showModal();
		if (!ui.newTicketOpen && dialog.open) dialog.close();
	});

	// A new type (or its form arriving) starts from the form's defaults.
	$effect(() => {
		if (!form || filledFor === kind) return;
		filledFor = kind;
		values = Object.fromEntries(form.fields.filter((f) => f.default !== undefined).map((f) => [f.name, f.default!]));
		states = {};
		void askStates();
	});

	/** The request body for what has been filled in. Read-only and hidden fields are left out when filing. */
	function body(forSave: boolean) {
		const out: Record<string, unknown> = {};
		const change: Record<string, unknown> = {};
		const extra: Record<string, unknown> = {};
		for (const f of fields) {
			const v = values[f.name];
			if (forSave && (states[f.name]?.hidden || states[f.name]?.read_only)) continue;
			if (v === null || v === undefined || v === '') continue;
			if (!f.core) extra[f.name] = v;
			else if (['title', 'description', 'priority'].includes(f.name)) out[f.name] = v;
			else change[f.name] = v;
		}
		if (Object.keys(change).length) out.change = change;
		if (Object.keys(extra).length) out.fields = extra;
		return out;
	}

	// Asking again whenever a field the form's conditions read changes, a moment after the person stops.
	let timer: ReturnType<typeof setTimeout> | undefined;
	let asked = 0;
	async function askStates() {
		const type = kind;
		const n = ++asked;
		try {
			const res = await api<{ states: Record<string, FieldState>; values: Record<string, FieldValue> | null }>(
				`/api/v1/forms/${type}/states`,
				{ method: 'POST', body: body(false) }
			);
			if (n !== asked || type !== kind) return;
			states = res.states;
			// A field the person cannot change shows what the rules will make it (priority from impact and urgency).
			for (const [name, v] of Object.entries(res.values ?? {})) if (states[name]?.read_only) values[name] = v;
		} catch {
			/* keep the states we have: saving checks again */
		}
	}

	function set(f: FormField, v: FieldValue | null) {
		values[f.name] = v;
		if (form?.reactive.includes(f.name)) {
			clearTimeout(timer);
			timer = setTimeout(askStates, 150);
		}
	}

	function reset() {
		error = chosen = '';
		filledFor = '';
	}

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			const t = await api<Ticket>('/api/v1/tickets', { method: 'POST', body: { type: kind, ...body(true) } });
			queue.prepend(t);
			ui.newTicketOpen = false;
			ui.announce = `Created ${t.reference}`;
			reset();
			await goto(`/tickets/${t.reference}`);
		} catch (err) {
			error = err instanceof Error ? err.message : 'Could not create the ticket';
		} finally {
			busy = false;
		}
	}
</script>

<dialog bind:this={dialog} onclose={() => (ui.newTicketOpen = false)} aria-labelledby="new-title">
	<form onsubmit={submit}>
		<h2 id="new-title">New ticket</h2>

		{#if lifecycle.fileable.length > 1}
			<fieldset class="kinds">
				<legend>Type</legend>
				{#each lifecycle.fileable as t (t.type)}
					<label class="kind">
						<input type="radio" name="type" value={t.type} checked={kind === t.type} onchange={() => (chosen = t.type)} />
						<span>{t.label}</span>
					</label>
				{/each}
			</fieldset>
		{/if}

		<div class="fields">
			{#each fields as f, i (f.name)}
				{@const st = states[f.name] ?? {}}
				{#if !st.hidden}
					<div class="field" class:wide={wide(f) || f.type === 'datetime' || f.name === 'title'}>
						<label for="nt-{f.name}">{f.label}{#if (st.required ?? f.required) && f.name !== 'title'}<span class="req" aria-hidden="true"> *</span>{/if}</label>
						<FieldInput
							field={f}
							value={values[f.name] ?? null}
							fieldState={st}
							id="nt-{f.name}"
							disabled={!!st.read_only}
							live
							autofocus={i === 0}
							onchange={(v) => set(f, v)}
						/>
					</div>
				{/if}
			{/each}
		</div>

		{#if error}<p class="error" role="alert">{error}</p>{/if}

		<div class="actions">
			<button type="button" class="btn" onclick={() => (ui.newTicketOpen = false)}>Cancel</button>
			<button class="btn primary" disabled={busy || !form}>{busy ? 'Creating…' : 'Create ticket'}</button>
		</div>
	</form>
</dialog>

<style>
	dialog {
		width: min(32rem, calc(100vw - 2rem));
		max-height: calc(100dvh - 2rem);
		overflow-y: auto;
		padding: 0;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
		color: var(--ink);
	}
	dialog::backdrop {
		background: color-mix(in srgb, var(--ink) 55%, transparent);
	}
	form {
		display: grid;
		gap: 1rem;
		padding: 1.5rem;
	}
	h2 {
		font: 700 1.9rem/1 var(--font-display);
	}
	.kinds {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
		margin: 0;
		padding: 0;
		border: 0;
	}
	.kinds legend {
		margin-bottom: 0.35rem;
		font-weight: 600;
		font-size: 0.85rem;
	}
	.kind {
		position: relative;
	}
	.kind input {
		position: absolute;
		opacity: 0;
	}
	.kind span {
		display: inline-block;
		padding: 0.4rem 0.9rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		cursor: pointer;
	}
	.kind input:checked + span {
		border-color: var(--blue);
		box-shadow: inset 0 0 0 1px var(--blue);
		font-weight: 600;
	}
	.kind input:focus-visible + span {
		outline: 2px solid var(--blue);
		outline-offset: 2px;
	}
	.fields {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(12rem, 1fr));
		gap: 1rem;
		align-items: start;
	}
	.wide {
		grid-column: 1 / -1;
	}
	.req {
		color: var(--red);
	}
	.actions {
		display: flex;
		justify-content: flex-end;
		gap: 0.5rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
		font-size: 0.9rem;
	}
</style>
