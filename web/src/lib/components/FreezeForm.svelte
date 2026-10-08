<script lang="ts">
	import { api } from '$lib/api';
	import CIPicker from '$lib/components/CIPicker.svelte';
	import { session } from '$lib/session.svelte';
	import { can, P, type CISummary, type Freeze } from '$lib/types';

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
	// The items it is limited to; none means every change. Only those who read the CMDB can find items to name.
	const readsCMDB = can(session.user, P.cmdbRead);
	const initialItems = () => freeze?.items ?? [];
	let items = $state<CISummary[]>(initialItems());
	// Items the person cannot see stay as they are: the form only sends the list when they could see all of it.
	const hiddenItems = () => (freeze?.item_ids.length ?? 0) - (freeze?.items.length ?? 0);
	const hidden = hiddenItems();
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
			const body: Record<string, unknown> = { name: draft.name, reason: draft.reason, starts_at: from.toISOString(), ends_at: until.toISOString() };
			if (readsCMDB && hidden === 0) body.items = items.map((c) => c.id);
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
	{#if readsCMDB && hidden === 0}
		<fieldset class="scope">
			<legend>Applies to</legend>
			{#if items.length === 0}
				<p class="hint">Every change. Name items to freeze only the changes linked to them.</p>
			{:else}
				<ul>
					{#each items as c (c.id)}
						<li>
							<span><strong>{c.name}</strong> <span class="hint">{c.class_label}</span></span>
							<button type="button" class="btn" onclick={() => (items = items.filter((x) => x.id !== c.id))} aria-label="Remove {c.name}">Remove</button>
						</li>
					{/each}
				</ul>
			{/if}
			<CIPicker label="Limit it to an item" action="Add" exclude={items.map((c) => c.id)} onpick={(c) => (items = [...items, c])} />
		</fieldset>
	{/if}
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
	.scope,
	.error,
	.actions {
		grid-column: 1 / -1;
	}
	.scope {
		display: grid;
		gap: 0.5rem;
		border: 0;
		padding: 0;
		margin: 0;
		min-width: 0;
	}
	legend {
		padding: 0;
		margin-bottom: 0.35rem;
		font-size: 0.85rem;
		font-weight: 600;
	}
	.scope ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.scope li {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
		padding: 0.35rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.875rem;
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
