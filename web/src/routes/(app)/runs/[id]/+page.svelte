<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import RunStatus from '$lib/components/RunStatus.svelte';
	import WorkflowGraph from '$lib/components/WorkflowGraph.svelte';
	import { when } from '$lib/format';
	import { runFinished, type WorkflowRun } from '$lib/types';

	let run = $state<WorkflowRun | null>(null);
	let error = $state('');
	let cancelling = $state(false);

	async function load() {
		try {
			run = await api<WorkflowRun>(`/api/v1/workflow-runs/${page.params.id}`);
			error = '';
		} catch (e) {
			if (!run) error = e instanceof ApiError && e.status === 404 ? 'That run does not exist.' : 'Could not load this run.';
		}
	}

	// A run moves on its own, so keep looking until it has finished.
	onMount(() => {
		void load();
		const t = setInterval(() => run && !runFinished(run.status) && void load(), 2000);
		return () => clearInterval(t);
	});

	async function cancel() {
		if (!run) return;
		cancelling = true;
		try {
			run = { ...run, ...(await api<WorkflowRun>(`/api/v1/workflow-runs/${run.id}/cancel`, { method: 'POST' })) };
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not cancel this run';
		} finally {
			cancelling = false;
		}
	}

	/** How each step stands: visited (how often and how it went), the one it is at now, or not reached. */
	function standing(id: string) {
		if (!run) return { visits: 0, state: 'pending' as const };
		const visits = (run.steps ?? []).filter((s) => s.step_id === id);
		const last = visits.at(-1);
		if (last?.outcome === 'failed') return { visits: visits.length, state: 'failed' as const };
		if (!runFinished(run.status) && run.current_step === id) return { visits: visits.length, state: 'current' as const };
		return { visits: visits.length, state: visits.length ? ('visited' as const) : ('pending' as const) };
	}

	/** One line of what a step did, from its detail. */
	function summary(detail: Record<string, unknown>) {
		return Object.entries(detail ?? {})
			.map(([k, v]) => `${k.replaceAll('_', ' ')}: ${typeof v === 'object' ? JSON.stringify(v) : v}`)
			.join(' · ');
	}
</script>

<svelte:head><title>{run ? run.workflow : 'Workflow run'} – ServiceLater</title></svelte:head>

<div class="page">
	<p class="back"><a href="/runs">Workflow runs</a></p>

	{#if error && !run}
		<p class="msg" role="alert">{error}</p>
	{:else if run}
		<header>
			<h1>{run.workflow}</h1>
			<div class="meta">
				<RunStatus status={run.status} />
				{#if run.reference}<a class="ref" href="/tickets/{run.reference}">{run.reference}</a>{/if}
				<span>{run.app} {run.app_version}</span>
				<span>Started {when(run.created_at)}</span>
				{#if run.finished_at}<span>Finished {when(run.finished_at)}</span>{/if}
				{#if run.status === 'waiting' && run.wake_at}<span>Checks again {when(run.wake_at)}</span>{/if}
			</div>
			{#if !runFinished(run.status)}
				<button class="btn" onclick={cancel} disabled={cancelling}>Cancel this run</button>
			{/if}
		</header>

		{#if error}<p class="msg" role="alert">{error}</p>{/if}
		{#if run.status === 'failed'}
			<p class="failure" role="alert"><strong>It stopped at {run.current_step}.</strong> {run.error}</p>
		{/if}

		<section aria-labelledby="steps">
			<h2 id="steps">Steps</h2>
			<p class="note">The workflow as it was when this run started.</p>
			<WorkflowGraph graph={run.graph ?? []} start={run.start} {standing} />
		</section>

		<section aria-labelledby="history">
			<h2 id="history">What happened</h2>
			{#if !run.steps?.length}
				<p class="note">Nothing yet. The run is waiting for a worker.</p>
			{:else}
				<table>
					<thead>
						<tr><th scope="col">#</th><th scope="col">Step</th><th scope="col">Result</th><th scope="col">Detail</th><th scope="col">When</th></tr>
					</thead>
					<tbody>
						{#each run.steps as s (s.seq)}
							<tr class={s.outcome}>
								<td>{s.seq}</td>
								<td>{s.step_id} <span class="type">{s.type}</span></td>
								<td>{s.outcome === 'ok' ? 'Done' : s.outcome === 'waiting' ? 'Went to sleep' : s.outcome === 'timeout' ? 'Timed out' : 'Failed'}</td>
								<td>{s.error || summary(s.detail)}</td>
								<td><time datetime={s.at}>{when(s.at)}</time></td>
							</tr>
						{/each}
					</tbody>
				</table>
			{/if}
		</section>

		{#if run.vars && Object.keys(run.vars).length}
			<section aria-labelledby="vars">
				<h2 id="vars">Variables</h2>
				<dl>
					{#each Object.entries(run.vars) as [k, v] (k)}
						<dt>{k}</dt>
						<dd>{typeof v === 'object' ? JSON.stringify(v) : v}</dd>
					{/each}
				</dl>
			</section>
		{/if}
	{:else}
		<p class="msg" aria-live="polite">Loading…</p>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2.5rem;
		max-width: 56rem;
		margin: 0 auto;
	}
	.back {
		font-size: 0.875rem;
		margin-bottom: 0.5rem;
	}
	.back a {
		color: var(--ink-2);
	}
	header {
		display: grid;
		gap: 0.5rem;
		justify-items: start;
		margin-bottom: 1rem;
	}
	h1 {
		font: 800 1.8rem/1.15 var(--font-display);
	}
	h2 {
		font: 800 1.15rem/1.3 var(--font-display);
		margin: 1.5rem 0 0.25rem;
	}
	.meta {
		display: flex;
		flex-wrap: wrap;
		gap: 0.35rem 1rem;
		align-items: baseline;
		color: var(--ink-2);
		font-size: 0.9rem;
	}
	.ref {
		font-weight: 700;
		color: var(--ink);
	}
	.note {
		color: var(--ink-2);
		font-size: 0.875rem;
		margin-bottom: 0.5rem;
	}
	.msg {
		padding: 1rem 0;
		color: var(--ink-2);
	}
	.failure {
		padding: 0.6rem 0.8rem;
		border-left: 4px solid var(--red);
		background: var(--surface);
	}

	.type {
		color: var(--ink-2);
		font-size: 0.85rem;
	}

	table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.9rem;
	}
	th {
		text-align: left;
		font-weight: 700;
		color: var(--ink-2);
		padding: 0.35rem 0.5rem 0.35rem 0.25rem;
		border-bottom: 2px solid var(--rule);
	}
	td {
		padding: 0.45rem 0.5rem 0.45rem 0.25rem;
		border-bottom: 1px solid var(--rule);
		vertical-align: top;
	}
	tr.failed td:nth-child(3),
	tr.failed td:nth-child(4) {
		color: var(--red);
	}
	dl {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: 0.25rem 1rem;
		margin: 0.5rem 0 0;
	}
	dt {
		font-weight: 700;
	}
	dd {
		margin: 0;
		overflow-wrap: anywhere;
	}

	@media (max-width: 40rem) {
		th:nth-child(5),
		td:nth-child(5) {
			display: none;
		}
	}
</style>
