<script lang="ts">
	import { api } from '$lib/api';
	import Status from '$lib/components/Status.svelte';
	import { groups } from '$lib/groups.svelte';
	import { session } from '$lib/session.svelte';
	import { canRecord, type Ticket } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	/**
	 * The tasks under a record: each team's part of the work on a request, with where it stands. Its requester follows
	 * them here; staff who may file tasks add one by hand when the workflow did not.
	 */
	let { ticket, onadded }: { ticket: Ticket; onadded: () => void } = $props();

	const tasks = $derived(ticket.tasks ?? []);
	const canAdd = $derived(
		ticket.type === 'request' && ticket.state_category !== 'closed' && canRecord(session.user, 'create', 'work_task') && canRecord(session.user, 'update', 'work_task')
	);

	let adding = $state(false);
	let title = $state('');
	let group = $state('');
	let busy = $state(false);
	let error = $state('');

	async function open() {
		adding = true;
		void groups.reload().catch(() => {});
	}

	async function add(e: Event) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			const t = await api<Ticket>('/api/v1/tickets', {
				method: 'POST',
				body: { type: 'work_task', title: title.trim(), parent_id: ticket.id, requester_id: ticket.requester_id, assignment_group_id: group || undefined }
			});
			ui.announce = `${t.reference} added`;
			title = group = '';
			adding = false;
			onadded();
		} catch (err) {
			error = err instanceof Error ? err.message : 'The task wasn’t added';
		} finally {
			busy = false;
		}
	}
</script>

{#if tasks.length || canAdd}
	<section class="tasks" aria-labelledby="tasks-h">
		<div class="head">
			<h2 id="tasks-h">Tasks</h2>
			{#if canAdd && !adding}<button class="btn" type="button" onclick={open}>Add a task</button>{/if}
		</div>
		{#if tasks.length === 0}
			<p class="none">No tasks yet.</p>
		{:else}
			<p class="summary">
				{ticket.open_tasks ? `${ticket.open_tasks} of ${tasks.length} still to do` : `All ${tasks.length} finished`}
			</p>
			<ul class="rows">
				{#each tasks as t (t.id)}
					<li>
						<a href="/tickets/{t.reference}"><span class="ref">{t.reference}</span> {t.title}</a>
						<span class="who">{t.assignee_name ?? t.assignment_group_name ?? 'Unassigned'}</span>
						<Status status={t.status} category={t.state_category} type={t.type} />
					</li>
				{/each}
			</ul>
		{/if}
		{#if adding}
			<form class="add" onsubmit={add}>
				<label class="field grow">
					<span>Task</span>
					<!-- svelte-ignore a11y_autofocus -->
					<input bind:value={title} maxlength="200" required autofocus placeholder="What needs doing" />
				</label>
				<label class="field">
					<span>Team</span>
					<select bind:value={group}>
						<option value="">No team</option>
						{#each groups.list as g (g.id)}<option value={g.id}>{g.name}</option>{/each}
					</select>
				</label>
				<div class="buttons">
					<button class="btn primary" disabled={busy || !title.trim()}>Add</button>
					<button class="btn" type="button" onclick={() => (adding = false)}>Cancel</button>
				</div>
				{#if error}<p class="error" role="alert">{error}</p>{/if}
			</form>
		{/if}
	</section>
{/if}

<style>
	.tasks {
		display: grid;
		gap: 0.6rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	.head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
	}
	h2 {
		font: 650 1.0625rem/1.3 var(--font-display);
		letter-spacing: -0.015em;
	}
	.summary,
	.none,
	.who {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.rows li {
		display: grid;
		grid-template-columns: minmax(0, 1fr) auto auto;
		align-items: baseline;
		gap: 0.75rem;
		padding: 0.5rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.rows li:last-child {
		border-bottom: 0;
	}
	.rows a {
		color: var(--ink);
		overflow-wrap: anywhere;
	}
	.ref {
		font-weight: 700;
	}
	.add {
		display: flex;
		flex-wrap: wrap;
		align-items: end;
		gap: 0.75rem;
	}
	.grow {
		flex: 1 1 14rem;
	}
	.buttons {
		display: flex;
		gap: 0.5rem;
	}
	.error {
		flex-basis: 100%;
		color: var(--red);
		font-size: 0.9rem;
	}
	@media (max-width: 40rem) {
		.rows li {
			grid-template-columns: minmax(0, 1fr) auto;
		}
		.who {
			grid-column: 1;
			grid-row: 2;
		}
	}
</style>
