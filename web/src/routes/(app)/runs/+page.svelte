<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import RunStatus from '$lib/components/RunStatus.svelte';
	import { age } from '$lib/format';
	import { runFinished, type RunStatus as Status, type WorkflowRun } from '$lib/types';

	type Filter = '' | Status;
	const filters: { id: Filter; label: string }[] = [
		{ id: '', label: 'All' },
		{ id: 'waiting', label: 'Waiting' },
		{ id: 'failed', label: 'Failed' },
		{ id: 'done', label: 'Done' }
	];

	let filter = $state<Filter>('');
	let runs = $state<WorkflowRun[]>([]);
	let hasMore = $state(false);
	let loading = $state(true);
	let error = $state('');

	async function load(more = false) {
		loading = true;
		error = '';
		try {
			const qs = new URLSearchParams({ limit: '50', offset: String(more ? runs.length : 0) });
			if (filter) qs.set('status', filter);
			const res = await api<{ items: WorkflowRun[]; has_more: boolean }>(`/api/v1/workflow-runs?${qs}`);
			runs = more ? [...runs, ...res.items] : res.items;
			hasMore = res.has_more;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load workflow runs';
		} finally {
			loading = false;
		}
	}

	function choose(f: Filter) {
		filter = f;
		void load();
	}

	// Runs move on their own: keep the list current while any of them is unfinished.
	onMount(() => {
		void load();
		const t = setInterval(() => {
			if (!loading && runs.some((r) => !runFinished(r.status))) void refresh();
		}, 3000);
		return () => clearInterval(t);
	});

	async function refresh() {
		try {
			const qs = new URLSearchParams({ limit: String(Math.max(runs.length, 50)) });
			if (filter) qs.set('status', filter);
			const res = await api<{ items: WorkflowRun[]; has_more: boolean }>(`/api/v1/workflow-runs?${qs}`);
			runs = res.items;
			hasMore = res.has_more;
		} catch {
			/* the next tick tries again */
		}
	}
</script>

<svelte:head><title>Workflow runs – ServiceLater</title></svelte:head>

<div class="page">
	<div class="head">
		<h1>Workflow runs</h1>
		<div class="filters" role="group" aria-label="Show">
			{#each filters as f (f.id)}
				<button aria-pressed={filter === f.id} onclick={() => choose(f.id)}>{f.label}</button>
			{/each}
		</div>
	</div>

	{#if error}
		<p class="msg" role="alert">{error}. <button class="link" onclick={() => load()}>Try again</button></p>
	{:else if !loading && runs.length === 0}
		<p class="msg">
			{#if filter}No {filter} runs.{:else}No workflow has run yet. A rule starts one when its conditions are met.{/if}
		</p>
	{/if}

	<ul>
		{#each runs as r (r.id)}
			<li>
				<a class="row" href="/runs/{r.id}">
					<span class="name">{r.workflow}</span>
					<time class="age" datetime={r.updated_at}>{age(r.updated_at)}</time>
					<span class="sub">
						<RunStatus status={r.status} />
						{#if r.reference}<span class="ref">{r.reference}</span>{/if}
						{#if r.status === 'failed'}
							<span class="why">{r.error}</span>
						{:else if !runFinished(r.status)}
							<span class="at">at {r.current_step}</span>
						{/if}
					</span>
				</a>
			</li>
		{/each}
	</ul>

	{#if loading}<p class="msg" aria-live="polite">Loading…</p>{/if}
	{#if hasMore && !loading}
		<button class="btn more" onclick={() => load(true)}>Show more</button>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 56rem;
		margin: 0 auto;
	}
	.head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 1rem;
		flex-wrap: wrap;
		margin-bottom: 0.75rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	.filters {
		display: flex;
		gap: 0.25rem;
	}
	.filters button {
		padding: 0.25rem 0.7rem;
		border: 1px solid transparent;
		border-radius: var(--radius);
		background: transparent;
		font-size: 0.875rem;
		font-weight: 600;
		color: var(--ink-2);
		cursor: pointer;
	}
	.filters button:hover {
		background: var(--blue-soft);
	}
	.filters button[aria-pressed='true'] {
		background: var(--ink);
		color: var(--surface);
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
		border-top: 1px solid var(--rule);
	}
	.row {
		display: grid;
		grid-template-columns: 1fr auto;
		gap: 0.15rem 0.75rem;
		padding: 0.65rem 0.25rem;
		border-bottom: 1px solid var(--rule);
		text-decoration: none;
	}
	.row:hover {
		background: var(--blue-soft);
	}
	.name {
		font-weight: 700;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.age {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.sub {
		grid-column: 1 / -1;
		display: flex;
		align-items: baseline;
		gap: 0.9rem;
		font-size: 0.875rem;
		color: var(--ink-2);
		min-width: 0;
	}
	.ref {
		font-weight: 600;
		color: var(--ink);
	}
	.why {
		color: var(--red);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		min-width: 0;
	}
	.msg {
		padding: 1rem 0.25rem;
		color: var(--ink-2);
	}
	.link {
		border: 0;
		background: none;
		padding: 0;
		text-decoration: underline;
		cursor: pointer;
	}
	.more {
		margin-top: 0.75rem;
	}
</style>
