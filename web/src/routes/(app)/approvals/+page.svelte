<script lang="ts">
	import { onMount } from 'svelte';
	import { approvals } from '$lib/approvals.svelte';
	import { age } from '$lib/format';

	let comments = $state<Record<string, string>>({});
	let deciding = $state<Record<string, boolean>>({});
	let errors = $state<Record<string, string>>({});

	onMount(() => void approvals.load());

	async function decide(id: string, decision: 'approved' | 'rejected') {
		deciding = { ...deciding, [id]: true };
		errors = { ...errors, [id]: '' };
		try {
			await approvals.decide(id, decision, (comments[id] ?? '').trim());
		} catch (e) {
			errors = { ...errors, [id]: e instanceof Error ? e.message : 'Could not save that decision' };
		} finally {
			deciding = { ...deciding, [id]: false };
		}
	}
</script>

<svelte:head><title>Approvals – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Approvals</h1>

	{#if approvals.error}
		<p class="msg" role="alert">{approvals.error}. <button class="link" onclick={() => approvals.load()}>Try again</button></p>
	{:else if !approvals.loading && approvals.items.length === 0}
		<p class="msg">Nothing is waiting on you.</p>
	{/if}

	<ul aria-label="Waiting for your decision">
		{#each approvals.items as a (a.id)}
			<li>
				<div class="row">
					<span class="name">{a.workflow}</span>
					<time class="age" datetime={a.created_at}>{age(a.created_at)}</time>
				</div>
				{#if a.group}<p class="for">For {a.group}: whoever in it answers first decides for the group.</p>{/if}
				{#if a.reference || a.title}
					<a class="ticket" href="/tickets/{a.reference}">
						{#if a.reference}<span class="ref">{a.reference}</span>{/if}
						{a.title}
					</a>
				{/if}
				<label class="field comment">
					<span class="sr-only">Comment (optional)</span>
					<textarea
						placeholder="Add a comment (optional)"
						rows="2"
						bind:value={comments[a.id]}
						disabled={deciding[a.id]}
					></textarea>
				</label>
				{#if errors[a.id]}<p class="msg" role="alert">{errors[a.id]}</p>{/if}
				<div class="actions">
					<button class="btn primary" onclick={() => decide(a.id, 'approved')} disabled={deciding[a.id]}>
						Approve
					</button>
					<button class="btn reject" onclick={() => decide(a.id, 'rejected')} disabled={deciding[a.id]}>
						Reject
					</button>
				</div>
			</li>
		{/each}
	</ul>

	{#if approvals.loading}<p class="msg" aria-live="polite">Loading…</p>{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 40rem;
		margin: 0 auto;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
		margin-bottom: 0.75rem;
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	li {
		display: grid;
		gap: 0.5rem;
		padding: 0.9rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.row {
		display: flex;
		align-items: baseline;
		justify-content: space-between;
		gap: 0.75rem;
	}
	.name {
		font-weight: 700;
	}
	.for {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.age {
		color: var(--ink-2);
		font-size: 0.85rem;
		white-space: nowrap;
	}
	.ticket {
		display: block;
		color: var(--ink);
		text-decoration: none;
	}
	.ticket:hover {
		text-decoration: underline;
	}
	.ref {
		font-weight: 700;
		margin-right: 0.5rem;
	}
	.comment textarea {
		min-height: 3.5rem;
	}
	.actions {
		display: flex;
		gap: 0.5rem;
	}
	.btn.reject {
		color: var(--red);
	}
	.msg {
		padding: 0.5rem 0;
		color: var(--ink-2);
	}
	.link {
		border: 0;
		background: none;
		padding: 0;
		text-decoration: underline;
		cursor: pointer;
	}
</style>
