<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { cmdb } from '$lib/cmdb.svelte';
	import { realtime } from '$lib/realtime.svelte';
	import { can, P, type CISummary } from '$lib/types';

	let { data } = $props();
	const canManage = $derived(can(data.user, P.cmdbManage));

	let search = $state('');
	let cls = $state('');
	let retired = $state(false);
	let items = $state<CISummary[]>([]);
	let more = $state(false);
	let loading = $state(true);
	let error = $state('');
	const pageSize = 50;
	let timer: ReturnType<typeof setTimeout>;

	function query(offset: number) {
		const q = new URLSearchParams({ limit: String(pageSize), offset: String(offset) });
		if (search.trim()) q.set('q', search.trim());
		if (cls) q.set('class', cls);
		if (retired) q.set('include_retired', 'true');
		return `/api/v1/cis?${q}`;
	}

	/** Load the first page again, or the next one after what is shown. */
	async function load(append = false) {
		try {
			const r = await api<{ items: CISummary[]; more: boolean }>(query(append ? items.length : 0));
			items = append ? [...items, ...r.items] : r.items;
			more = r.more;
			error = '';
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the items';
		} finally {
			loading = false;
		}
	}

	function later() {
		clearTimeout(timer);
		timer = setTimeout(() => void load(), 200);
	}

	onMount(() => {
		void cmdb.ensure();
		void load();
		// Someone added, changed or removed an item: show the list as it is now.
		return realtime.on((e) => {
			if (e.type === 'ci.changed' || e.type === 'resync') later();
		});
	});
</script>

<svelte:head><title>Configuration items – ServiceLater</title></svelte:head>

<div class="page">
	<div class="top">
		<div>
			<h1>Configuration items</h1>
			<p class="lede">The services, systems and devices tickets are about, and what depends on what.</p>
		</div>
		{#if canManage}
			<div class="actions">
				<a class="btn" href="/cmdb/import">Import</a>
				<a class="btn primary" href="/cmdb/new">New item</a>
			</div>
		{/if}
	</div>

	<div class="filters" role="search">
		<label class="field grow">
			<span>Name</span>
			<input type="search" bind:value={search} oninput={later} placeholder="Search by name" />
		</label>
		<label class="field">
			<span>Class</span>
			<select bind:value={cls} onchange={() => void load()}>
				<option value="">Every class</option>
				{#each cmdb.tree() as { cls: c, depth } (c.name)}
					<option value={c.name}>{' '.repeat(depth)}{c.label}</option>
				{/each}
			</select>
		</label>
		<label class="check"><input type="checkbox" bind:checked={retired} onchange={() => void load()} /> Include retired</label>
	</div>

	{#if error}
		<p class="error" role="alert">{error}</p>
	{:else if !loading && items.length === 0}
		<p class="msg">{search || cls ? 'Nothing matches.' : 'No configuration items yet.'}</p>
	{/if}

	<ul class="items" aria-label="Configuration items">
		{#each items as c (c.id)}
			<li>
				<a href="/cmdb/{c.id}">
					<span class="name">{c.name}</span>
					<span class="cls">{c.class_label}</span>
					{#if c.status !== 'active' || c.support_group_name}
						<span class="sub">
							{#if c.status !== 'active'}<span class="status">{cmdb.statusLabel(c.status)}</span>{/if}
							{#if c.support_group_name}<span>Supported by {c.support_group_name}</span>{/if}
						</span>
					{/if}
				</a>
			</li>
		{/each}
	</ul>
	{#if more}
		<button class="btn" onclick={() => void load(true)}>Show more</button>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 56rem;
	}
	.top .btn {
		flex: none;
		white-space: nowrap;
	}
	.top {
		display: flex;
		flex-wrap: wrap;
		align-items: flex-start;
		justify-content: space-between;
		gap: 0.75rem 1rem;
	}
	.top > div:first-child {
		flex: 1 1 18rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	.filters {
		display: flex;
		flex-wrap: wrap;
		align-items: flex-end;
		gap: 0.75rem 1rem;
		margin-bottom: 0.75rem;
	}
	.grow {
		flex: 1 1 14rem;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		min-height: 2.5rem;
	}
	.items {
		list-style: none;
		margin: 0 0 1rem;
		padding: 0;
	}
	.items li {
		border-bottom: 1px solid var(--rule);
	}
	.items a {
		display: grid;
		grid-template-columns: minmax(0, 1fr) auto;
		column-gap: 1rem;
		padding: 0.7rem 0.25rem;
		color: var(--ink);
		text-decoration: none;
	}
	.items a:hover .name {
		text-decoration: underline;
	}
	.name {
		font-weight: 700;
		overflow-wrap: anywhere;
	}
	.cls {
		color: var(--ink-2);
		font-size: 0.875rem;
		white-space: nowrap;
	}
	.sub {
		grid-column: 1 / -1;
		display: flex;
		flex-wrap: wrap;
		gap: 0 1rem;
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.status {
		font-weight: 600;
		color: var(--ink);
	}
	.msg {
		padding: 0.5rem 0;
		color: var(--ink-2);
	}
	.error {
		padding: 0.5rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
	}
	.actions {
		display: flex;
		gap: 0.5rem;
		flex-shrink: 0;
	}
</style>
