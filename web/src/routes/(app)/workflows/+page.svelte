<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import { can, P } from '$lib/types';

	type Item = { app: string; name: string; version: string; record_type: string; steps: number; edited: boolean };

	let { data } = $props();
	const editor = $derived(can(data.user, P.workflowsEdit));
	let items = $state<Item[]>([]);
	let error = $state('');
	let loading = $state(true);
	let apps = $state<string[]>([]);

	// A new workflow belongs to an app (its studio layer), and is for one record type.
	let newApp = $state('');
	let newName = $state('');
	let newType = $state('incident');

	onMount(async () => {
		void lifecycle.ensure();
		try {
			const r = await api<{ items: Item[]; apps: { name: string }[] }>('/api/v1/workflows');
			items = r.items;
			apps = r.apps.map((a) => a.name);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load workflows';
		} finally {
			loading = false;
		}
		newApp = apps.includes('core-itsm') ? 'core-itsm' : (apps[0] ?? '');
	});

	// An app's name, readable: change-management is "Change management".
	const appLabel = (name: string) => (name.charAt(0).toUpperCase() + name.slice(1)).replaceAll('-', ' ').replace(/\bitsm\b/i, 'ITSM').replace(/\bcmdb\b/i, 'CMDB');

	const byApp = $derived(
		Object.entries(items.reduce<Record<string, Item[]>>((acc, i) => ((acc[i.app] ??= []).push(i), acc), {})).sort(([a], [b]) => a.localeCompare(b))
	);

	// The app that looks after the chosen type (the one whose lifecycle it uses) is the natural owner: it can read
	// those records, and its rules can start the workflow.
	$effect(() => {
		const owner = lifecycle.byType[newType]?.app;
		if (owner && apps.includes(owner)) newApp = owner;
	});

	function create(e: Event) {
		e.preventDefault();
		const name = newName.trim();
		if (!name || !newApp) return;
		void goto(`/workflows/${newApp}/${name}?new=${encodeURIComponent(newType)}`);
	}
</script>

<svelte:head><title>Workflows – ServiceLater</title></svelte:head>

<div class="page">
	<div class="top">
		<div>
			<h1>Workflows</h1>
			<p class="lede">What runs on its own when records change, step by step. <a href="/runs">See the runs</a>.</p>
		</div>
	</div>

	{#if error}<p class="error" role="alert">{error}</p>{:else if !loading && items.length === 0}<p class="msg">No workflows are installed.</p>{/if}

	{#each byApp as [app, list] (app)}
		<section aria-labelledby="app-{app}">
			<h2 id="app-{app}">{appLabel(app)} <span class="app">{app}</span></h2>
			<ul class="rows">
				{#each list as w (w.name)}
					<li>
						<!-- Workflows are named, not labelled: the name is what rules start them by. -->
						<a href="/workflows/{w.app}/{w.name}">
							<span class="name">{w.name}</span>
							{#if w.edited}<span class="badge">Edited here</span>{/if}
							<span class="meta">{lifecycle.typeLabel(w.record_type)} · {w.steps} steps</span>
						</a>
					</li>
				{/each}
			</ul>
		</section>
	{/each}

	{#if editor}
		<form class="new" onsubmit={create} aria-labelledby="new-h">
			<h2 id="new-h">New workflow</h2>
			<p class="hint">
				It belongs to an app: the app's rules can start it, and its steps act as the app, with the app's permissions. Pick the
				app that looks after the record type (incident-management for incidents). It can also be started by hand.
			</p>
			<div class="grid">
				<label class="field">
					<span>App</span>
					<select bind:value={newApp}>{#each apps as a (a)}<option value={a}>{a}</option>{/each}</select>
				</label>
				<label class="field">
					<span>Name</span>
					<input bind:value={newName} placeholder="escalate-p1" pattern={'[a-z][a-z0-9\\-]{1,62}'} title="Lowercase letters, digits and hyphens" required />
				</label>
				<label class="field">
					<span>For</span>
					<select bind:value={newType}>
						{#each lifecycle.types as t (t.type)}<option value={t.type}>{t.label} records</option>{/each}
					</select>
				</label>
			</div>
			<div><button class="btn primary" disabled={!newName.trim() || !newApp}>Create workflow</button></div>
		</form>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 52rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	h2 {
		display: flex;
		align-items: baseline;
		gap: 0.5rem;
		font: 650 1.0625rem/1.3 var(--font-display);
		margin-bottom: 0.4rem;
	}
	h2 .app {
		font: 500 0.75rem/1 var(--font-mono);
		color: var(--ink-2);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	section {
		margin-bottom: 1.25rem;
	}
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.rows li {
		border-bottom: 1px solid var(--rule);
	}
	.rows a {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.25rem 0.75rem;
		padding: 0.65rem 0.25rem;
		color: var(--ink);
		text-decoration: none;
	}
	.rows a:hover .name {
		text-decoration: underline;
	}
	.name {
		font: 600 0.875rem/1.3 var(--font-mono);
	}
	.rows .meta {
		margin-left: auto;
	}
	.meta,
	.hint,
	.msg {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.badge {
		padding: 0 0.4rem;
		border-radius: 4px;
		background: var(--marker-soft);
		color: var(--marker-ink);
		font-size: 0.75rem;
		font-weight: 600;
	}
	.new {
		display: grid;
		gap: 0.75rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(12rem, 1fr));
		gap: 0.75rem 1rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
</style>
