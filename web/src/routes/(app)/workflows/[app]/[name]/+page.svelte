<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import WorkflowBuilder from '$lib/builder/WorkflowBuilder.svelte';
	import type { Step, WorkflowActivity, WorkflowDef } from '$lib/builder/workflow';
	import { realtime } from '$lib/realtime.svelte';
	import { when } from '$lib/format';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import { can, P } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let { data } = $props();
	const editable = $derived(can(data.user, P.workflowsEdit));
	const app = $derived(page.params.app!);
	const name = $derived(page.params.name!);
	const path = $derived(`/api/v1/workflows/${app}/${name}`);

	let def = $state<WorkflowDef | null>(null);
	let loadError = $state('');
	/** Bumped to start the builder again from def (after a reset). */
	let generation = $state(0);
	let builder = $state<ReturnType<typeof WorkflowBuilder>>();
	let confirmingReset = $state(false);

	// Where the workflow's runs are, live: asked for again when one of its runs starts, moves or ends.
	let activity = $state<WorkflowActivity | null>(null);
	let showActivity = $state(true);
	async function loadActivity() {
		if (!def || (!def.shipped && !def.edited)) return; // not installed yet: no runs
		try {
			activity = await api<WorkflowActivity>(`${path}/activity`);
		} catch {
			/* keep what is shown */
		}
	}
	let soon: ReturnType<typeof setTimeout> | undefined;
	function refresh() {
		clearTimeout(soon);
		soon = setTimeout(loadActivity, 300);
	}
	$effect(() => {
		void def?.name;
		void loadActivity();
	});
	$effect(() => {
		const off = realtime.on((e) => {
			if ((e.type === 'run.changed' && e.app === app && e.workflow === name) || e.type === 'resync') refresh();
		});
		// A safety net: a wait that ends on its own time sends nothing until the run moves.
		const tick = setInterval(loadActivity, 30_000);
		return () => {
			off();
			clearInterval(tick);
			clearTimeout(soon);
		};
	});
	let error = $state('');

	async function load() {
		try {
			def = await api<WorkflowDef>(path);
		} catch (e) {
			// A new workflow (from the list's form) starts as one step that ends it.
			const type = page.url.searchParams.get('new');
			if (e instanceof ApiError && e.status === 404 && type && editable) {
				const list = await api<{ apps: { name: string; permissions: string[] }[] }>('/api/v1/workflows').catch(() => ({ apps: [] }));
				const perms = list.apps.find((a) => a.name === app)?.permissions ?? [];
				def = { app, name, record_type: type, start: 'done', steps: [{ id: 'done', type: 'end' }], layout: {}, edited: false, shipped: false, layers: [], yaml: '', app_permissions: perms };
			} else {
				loadError = e instanceof ApiError && e.status === 404 ? 'That workflow does not exist.' : 'Could not load the workflow.';
			}
		}
		generation++;
	}
	$effect(() => {
		void path;
		void load();
		void lifecycle.ensure();
	});

	async function save(body: { record_type: string; start: string; steps: Step[]; layout: Record<string, { x: number; y: number }> }) {
		error = '';
		try {
			def = await api<WorkflowDef>(path, { method: 'PUT', body });
			ui.announce = `${name} saved`;
			if (page.url.searchParams.has('new')) void goto(`/workflows/${app}/${name}`, { replaceState: true, keepFocus: true, noScroll: true });
			return true;
		} catch (e) {
			if (e instanceof ApiError && e.status === 422) {
				const problems = (e.body as { problems?: string[] } | undefined)?.problems;
				builder?.showProblems(problems?.length ? problems : [e.message]);
			} else {
				error = e instanceof Error ? e.message : 'The workflow did not save';
			}
			return false;
		}
	}

	async function reset() {
		error = '';
		try {
			const res = await api<WorkflowDef | undefined>(`${path}/edits`, { method: 'DELETE' });
			confirmingReset = false;
			if (!res) {
				ui.announce = `${name} removed`;
				await goto('/workflows');
				return;
			}
			def = res;
			generation++;
			ui.announce = `${name} is back to the app's version`;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not reset it';
		}
	}
</script>

<svelte:head><title>{name} – Workflows – ServiceLater</title></svelte:head>

<div class="page">
	<nav class="crumbs" aria-label="Breadcrumb">
		<ol>
			<li><a href="/workflows">Workflows</a></li>
			<li>{page.params.app}</li>
			<li aria-current="page">{page.params.name}</li>
		</ol>
	</nav>
	{#if loadError}
		<p class="msg" role="alert">{loadError}</p>
	{:else if def}
		<div class="top">
			<div>
				<h1>{def.name}</h1>
				<p class="lede">
					{def.app} · {lifecycle.typeLabel(def.record_type)} records
					{#if def.edited}
						· <span class="badge">Edited here</span>{def.edited_by ? ` by ${def.edited_by}` : ''}{def.edited_at ? `, ${when(def.edited_at)}` : ''}
					{:else if !def.shipped}
						· not saved yet
					{/if}
				</p>
			</div>
			{#if editable && def.edited}
				<div class="reset">
					{#if confirmingReset}
						<button class="btn reject" onclick={reset}>{def.shipped ? "Go back to the app's version" : 'Delete this workflow'}</button>
						<button class="btn" onclick={() => (confirmingReset = false)}>Keep it</button>
					{:else}
						<button class="btn" onclick={() => (confirmingReset = true)}>{def.shipped ? 'Reset to the app’s version' : 'Delete workflow'}</button>
					{/if}
				</div>
			{/if}
		</div>
		{#if !editable}<p class="hint">You can look at this workflow; changing it needs the workflows:edit permission.</p>{/if}
		{#if error}<p class="error" role="alert">{error}</p>{/if}

		{#key generation}
			<WorkflowBuilder bind:this={builder} {def} {editable} activity={showActivity ? activity : null} onsave={save}>
				{#snippet tools()}
					{#if activity}
						<label class="live-toggle"><input type="checkbox" bind:checked={showActivity} /> Show where its runs are</label>
					{/if}
				{/snippet}
			</WorkflowBuilder>
		{/key}

		{#if def.yaml}
			<details class="yaml">
				<summary>YAML</summary>
				{#if def.override}
					<h2>Your change</h2>
					<p class="hint">
						Kept in ServiceLater, apart from the app, as only what you changed, so upgrades of {def.app} still bring their
						changes to the rest. It is the app's <code>local/{def.path}</code>: <code>servicelater app export-local</code> writes
						it out for version control, and <code>import-local</code> loads it into another environment.
					</p>
					<pre>{def.override}</pre>
					<h2>The whole workflow</h2>
				{:else}
					<p class="hint">The workflow as a file, for keeping apps in version control.</p>
				{/if}
				<pre>{def.yaml}</pre>
			</details>
		{/if}
	{/if}
</div>

<style>
	/* A column: the header, then the builder to the bottom of the window; the YAML is below, reached by scrolling. */
	.page {
		height: 100%;
		overflow-y: auto;
		display: flex;
		flex-direction: column;
		padding: 1rem 1.25rem 1.25rem;
	}
	.crumbs ol {
		display: flex;
		flex-wrap: wrap;
		gap: 0.35rem;
		margin: 0 0 0.6rem;
		padding: 0;
		list-style: none;
		font-size: 0.8125rem;
		color: var(--ink-2);
	}
	.crumbs li + li::before {
		content: '/';
		margin-right: 0.35rem;
		color: color-mix(in srgb, var(--ink-2) 55%, transparent);
	}
	.crumbs a {
		color: var(--ink-2);
		font-weight: 550;
		text-decoration: none;
	}
	.crumbs a:hover {
		color: var(--ink);
		text-decoration: underline;
	}
	.crumbs li:not(:first-child) {
		font-family: var(--font-mono);
		font-size: 0.75rem;
	}
	.top {
		display: flex;
		flex-wrap: wrap;
		align-items: flex-start;
		justify-content: space-between;
		gap: 0.75rem;
		margin-bottom: 0.75rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
		overflow-wrap: anywhere;
	}
	.lede,
	.hint,
	.msg {
		color: var(--ink-2);
	}
	.hint {
		font-size: 0.875rem;
		margin-bottom: 0.5rem;
	}
	.badge {
		padding: 0.05rem 0.4rem;
		border-radius: var(--radius);
		background: var(--marker);
		color: var(--on-marker);
		font-size: 0.75rem;
		font-weight: 700;
	}
	.live-toggle {
		display: inline-flex;
		align-items: center;
		gap: 0.45rem;
		font-size: 0.875rem;
		white-space: nowrap;
	}
	.reset {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
	}
	.btn.reject {
		color: var(--red);
	}
	.error {
		color: var(--red);
		font-weight: 600;
		margin-bottom: 0.5rem;
	}
	.yaml {
		margin-top: 1rem;
	}
	.yaml h2 {
		margin-top: 0.75rem;
		font: 650 0.9375rem/1.3 var(--font-display);
	}
	.yaml summary {
		cursor: pointer;
		font-weight: 600;
	}
	.yaml pre {
		overflow-x: auto;
		padding: 0.75rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		font-size: 0.85rem;
	}
</style>
