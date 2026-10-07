<script lang="ts">
	import { api } from '$lib/api';
	import type { Freeze } from '$lib/types';

	/** Declares a change freeze, or changes one when `freeze` is given. */
	let {
		freeze = null,
		start = null,
		onsaved,
		oncancel
	}: {
		freeze?: Freeze | null;
		/** Where a new freeze starts out: the first day on screen. */
		start?: Date | null;
		onsaved: (f: Freeze) => void;
		oncancel: () => void;
	} = $props();

	/** A time as a datetime-local input wants it, in the browser's time zone. */
	function local(d: Date) {
		const pad = (n: number) => String(n).padStart(2, '0');
		return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
	}
	function initial() {
		if (freeze) return { name: freeze.name, reason: freeze.reason, from: local(new Date(freeze.starts_at)), until: local(new Date(freeze.ends_at)) };
		const from = start ?? new Date();
		const until = new Date(from);
		until.setDate(until.getDate() + 1);
		return { name: '', reason: '', from: local(from), until: local(until) };
	}
	const draft = $state(initial());
	let saving = $state(false);
	let error = $state('');

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		error = '';
		const from = new Date(draft.from);
		const until = new Date(draft.until);
		if (Number.isNaN(from.getTime()) || Number.isNaN(until.getTime())) {
			error = 'Give the freeze a start and an end.';
			return;
		}
		saving = true;
		try {
			const body = { name: draft.name, reason: draft.reason, starts_at: from.toISOString(), ends_at: until.toISOString() };
			const saved = freeze
				? await api<Freeze>(`/api/v1/freeze-windows/${freeze.id}`, { method: 'PATCH', body })
				: await api<Freeze>('/api/v1/freeze-windows', { method: 'POST', body });
			onsaved(saved);
		} catch (err) {
			error = err instanceof Error ? err.message : 'Could not save the freeze';
		} finally {
			saving = false;
		}
	}
</script>

<form class="freeze-form" onsubmit={submit} aria-label={freeze ? `Change ${freeze.name}` : 'Declare a change freeze'}>
	<label class="field name">
		<span>Name</span>
		<input bind:value={draft.name} required maxlength="200" placeholder="Year-end freeze" />
	</label>
	<label class="field">
		<span>Starts</span>
		<input type="datetime-local" bind:value={draft.from} required />
	</label>
	<label class="field">
		<span>Ends</span>
		<input type="datetime-local" bind:value={draft.until} required />
	</label>
	<label class="field reason">
		<span>Reason <em>(optional)</em></span>
		<input bind:value={draft.reason} maxlength="2000" placeholder="Finance closes the books." />
	</label>
	{#if error}<p class="error" role="alert">{error}</p>{/if}
	<div class="actions">
		<button class="btn primary" disabled={saving}>{freeze ? 'Save freeze' : 'Declare freeze'}</button>
		<button type="button" class="btn" onclick={oncancel}>Cancel</button>
	</div>
</form>

<style>
	.freeze-form {
		display: grid;
		grid-template-columns: repeat(2, minmax(0, 1fr));
		gap: 0.75rem 1rem;
		padding: 1rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.name,
	.reason,
	.error,
	.actions {
		grid-column: 1 / -1;
	}
	em {
		font-style: normal;
		font-weight: 400;
		color: var(--ink-2);
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
	.actions {
		display: flex;
		gap: 0.5rem;
	}
	@media (max-width: 40rem) {
		.freeze-form {
			grid-template-columns: minmax(0, 1fr);
		}
	}
</style>
