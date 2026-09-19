<script lang="ts">
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { priorityLabel } from '$lib/format';
	import { queue } from '$lib/queue.svelte';
	import type { Ticket } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let dialog: HTMLDialogElement;
	let title = $state('');
	let description = $state('');
	let priority = $state<1 | 2 | 3 | 4>(3);
	let busy = $state(false);
	let error = $state('');

	// The dialog element owns focus trapping and Escape; we just mirror its open state.
	$effect(() => {
		if (ui.newTicketOpen && !dialog.open) dialog.showModal();
		if (!ui.newTicketOpen && dialog.open) dialog.close();
	});

	function reset() {
		title = description = error = '';
		priority = 3;
	}

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			const t = await api<Ticket>('/api/v1/tickets', { method: 'POST', body: { title, description, priority } });
			queue.prepend(t);
			ui.newTicketOpen = false;
			ui.announce = `Created ticket ${t.number}`;
			reset();
			await goto(`/tickets/${t.number}`);
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

		<label class="field">
			<span>What’s wrong?</span>
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
