<script lang="ts">
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import Status from '$lib/components/Status.svelte';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import { session } from '$lib/session.svelte';
	import { canRecord, type Ticket } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	/**
	 * Incidents and the problems behind them. On an incident: the problem it is linked to, and (for staff) linking it to
	 * an open problem or raising a new one from it. On a problem: the incidents linked to it and how many are still open.
	 * The server sends the references and titles; nothing here works them out.
	 */
	let { ticket, canEdit, onchange }: { ticket: Ticket; canEdit: boolean; onchange: (t: Ticket) => void } = $props();

	const isProblem = $derived(ticket.type === 'problem');
	const incidents = $derived(ticket.incidents ?? []);
	// Problems are in use only while an app gives them a lifecycle; a person reads them with records:read:problem.
	const problemsInUse = $derived(lifecycle.types.some((t) => t.type === 'problem'));
	const mayLink = $derived(ticket.type === 'incident' && canEdit && problemsInUse && canRecord(session.user, 'read', 'problem'));
	const mayRaise = $derived(mayLink && lifecycle.types.some((t) => t.type === 'problem' && t.can_file));

	let mode = $state<'' | 'link' | 'raise'>('');
	let open = $state<Ticket[]>([]);
	let chosen = $state('');
	let title = $state('');
	let busy = $state(false);
	let error = $state('');

	async function startLink() {
		mode = 'link';
		error = '';
		chosen = '';
		try {
			open = (await api<{ items: Ticket[] }>('/api/v1/tickets?type=problem&open=true&limit=100')).items;
		} catch (err) {
			error = err instanceof Error ? err.message : 'The problems could not be loaded';
		}
	}

	function startRaise() {
		mode = 'raise';
		error = '';
		title = ticket.title;
	}

	async function link(problemId: string | null) {
		busy = true;
		error = '';
		try {
			const saved = await api<Ticket>(`/api/v1/tickets/${ticket.id}`, { method: 'PATCH', body: { problem_id: problemId } });
			ui.announce = problemId ? `Linked to ${saved.problem_reference}` : 'No longer linked to a problem';
			mode = '';
			onchange(saved);
		} catch (err) {
			error = err instanceof Error ? err.message : 'The link wasn’t saved';
		} finally {
			busy = false;
		}
	}

	async function raise(e: Event) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			const p = await api<Ticket>('/api/v1/tickets', {
				method: 'POST',
				body: { type: 'problem', title: title.trim(), description: ticket.description, priority: ticket.priority, incidents: [ticket.id] }
			});
			ui.announce = `${p.reference} raised`;
			await goto(`/tickets/${p.reference}`);
		} catch (err) {
			error = err instanceof Error ? err.message : 'The problem wasn’t raised';
		} finally {
			busy = false;
		}
	}
</script>

{#if isProblem}
	<section class="links" aria-labelledby="incidents-h">
		<h2 id="incidents-h">Incidents</h2>
		{#if incidents.length === 0}
			<p class="none">No incidents are linked yet. Link one from the incident’s page.</p>
		{:else}
			<p class="summary">
				{ticket.open_incidents ? `${ticket.open_incidents} of ${incidents.length} still open` : `All ${incidents.length} resolved`}
			</p>
			<ul class="rows">
				{#each incidents as t (t.id)}
					<li>
						<a href="/tickets/{t.reference}"><span class="ref">{t.reference}</span> {t.title}</a>
						<span class="who">{t.assignee_name ?? t.assignment_group_name ?? 'Unassigned'}</span>
						<Status status={t.status} category={t.state_category} type={t.type} />
					</li>
				{/each}
			</ul>
		{/if}
	</section>
{:else if ticket.type === 'incident' && (ticket.problem_reference || mayLink)}
	<section class="links" aria-labelledby="problem-h">
		<div class="head">
			<h2 id="problem-h">Problem</h2>
			{#if mayLink && !mode && !ticket.problem_id}
				<div class="buttons">
					<button class="btn" type="button" onclick={startLink}>Link to a problem</button>
					{#if mayRaise}<button class="btn" type="button" onclick={startRaise}>Raise a problem</button>{/if}
				</div>
			{/if}
		</div>
		{#if ticket.problem_reference}
			<ul class="rows">
				<li>
					<a href="/tickets/{ticket.problem_reference}"><span class="ref">{ticket.problem_reference}</span> {ticket.problem_title}</a>
					{#if mayLink}<button class="btn quiet-btn" type="button" disabled={busy} onclick={() => link(null)}>Unlink</button>{/if}
				</li>
			</ul>
		{:else if !mode}
			<p class="none">Not linked to a problem.</p>
		{/if}

		{#if mode === 'link'}
			<form
				class="add"
				onsubmit={(e) => {
					e.preventDefault();
					if (chosen) void link(chosen);
				}}
			>
				<label class="field grow">
					<span>Open problem</span>
					<select bind:value={chosen} required>
						<option value="" disabled>{open.length ? 'Choose a problem' : 'No open problems'}</option>
						{#each open as p (p.id)}<option value={p.id}>{p.reference} {p.title}</option>{/each}
					</select>
				</label>
				<div class="buttons">
					<button class="btn primary" disabled={busy || !chosen}>Link</button>
					<button class="btn" type="button" onclick={() => (mode = '')}>Cancel</button>
				</div>
			</form>
		{:else if mode === 'raise'}
			<form class="add" onsubmit={raise}>
				<label class="field grow">
					<span>Problem</span>
					<!-- svelte-ignore a11y_autofocus -->
					<input bind:value={title} maxlength="200" required autofocus placeholder="What keeps going wrong" />
				</label>
				<div class="buttons">
					<button class="btn primary" disabled={busy || !title.trim()}>Raise</button>
					<button class="btn" type="button" onclick={() => (mode = '')}>Cancel</button>
				</div>
			</form>
		{/if}
		{#if error}<p class="error" role="alert">{error}</p>{/if}
	</section>
{/if}

<style>
	.links {
		display: grid;
		gap: 0.6rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	.head {
		display: flex;
		flex-wrap: wrap;
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
