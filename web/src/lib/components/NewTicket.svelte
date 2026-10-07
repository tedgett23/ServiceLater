<script lang="ts">
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { priorityLabel } from '$lib/format';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import { queue } from '$lib/queue.svelte';
	import type { Ticket } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let dialog: HTMLDialogElement;
	let title = $state('');
	let description = $state('');
	let priority = $state<1 | 2 | 3 | 4>(3);
	// Which kind of record to file. The choice is offered only when the person may file more than one type.
	let chosen = $state('');
	const kind = $derived(lifecycle.fileable.some((t) => t.type === chosen) ? chosen : (lifecycle.fileable[0]?.type ?? 'incident'));
	let busy = $state(false);
	let error = $state('');

	// The dialog element owns focus trapping and Escape; we just mirror its open state.
	$effect(() => {
		if (ui.newTicketOpen && !dialog.open) dialog.showModal();
		if (!ui.newTicketOpen && dialog.open) dialog.close();
	});

	function reset() {
		title = description = error = chosen = '';
		priority = 3;
	}

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			const t = await api<Ticket>('/api/v1/tickets', { method: 'POST', body: { type: kind, title, description, priority } });
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

		<label class="field">
			<span>Summary</span>
			<!-- svelte-ignore a11y_autofocus -->
			<input name="title" required maxlength="200" autofocus bind:value={title} />
		</label>
		<label class="field">
			<span>Details</span>
			<textarea name="description" maxlength="10000" bind:value={description}></textarea>
		</label>
		<label class="field">
			<span>Priority</span>
			<select name="priority" bind:value={priority}>
				{#each [1, 2, 3, 4] as const as p (p)}
					<option value={p}>{priorityLabel[p]}</option>
				{/each}
			</select>
		</label>

		{#if error}<p class="error" role="alert">{error}</p>{/if}

		<div class="actions">
			<button type="button" class="btn" onclick={() => (ui.newTicketOpen = false)}>Cancel</button>
			<button class="btn primary" disabled={busy}>{busy ? 'Creating…' : 'Create ticket'}</button>
		</div>
	</form>
</dialog>

<style>
	dialog {
		width: min(32rem, calc(100vw - 2rem));
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
