<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { groups } from '$lib/groups.svelte';
	import type { Group } from '$lib/types';

	let loading = $state(true);
	let error = $state('');
	let name = $state('');
	let description = $state('');
	let creating = $state(false);
	let createError = $state('');

	onMount(async () => {
		try {
			await groups.reload();
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load groups';
		} finally {
			loading = false;
		}
	});

	async function create(e: Event) {
		e.preventDefault();
		if (!name.trim() || creating) return;
		creating = true;
		createError = '';
		try {
			const g = await api<Group>('/api/v1/groups', { method: 'POST', body: { name: name.trim(), description } });
			name = '';
			description = '';
			await groups.reload();
			await goto(`/admin/groups/${g.id}`);
		} catch (err) {
			createError = err instanceof Error ? err.message : 'Could not create the group';
		} finally {
			creating = false;
		}
	}

	const people = (n: number) => (n === 1 ? '1 person' : `${n} people`);
</script>

<svelte:head><title>Groups – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Groups</h1>
	<p class="lede">Teams that tickets are routed to. Workflows can ask a whole group to approve something.</p>

	{#if error}
		<p class="msg" role="alert">{error}</p>
	{:else if !loading && groups.list.length === 0}
		<p class="msg">No groups yet.</p>
	{/if}

	<ul class="groups">
		{#each groups.list as g (g.id)}
			<li>
				<a href="/admin/groups/{g.id}">
					<span class="name">{g.name}</span>
					<span class="count">{people(g.member_count)}</span>
					{#if g.description}<span class="desc">{g.description}</span>{/if}
				</a>
			</li>
		{/each}
	</ul>

	<form class="create" onsubmit={create}>
		<h2>New group</h2>
		<label class="field">
			<span>Name</span>
			<input bind:value={name} maxlength="100" required />
		</label>
		<label class="field">
			<span>Description</span>
			<input bind:value={description} maxlength="1000" placeholder="What this team looks after" />
		</label>
		{#if createError}<p class="error" role="alert">{createError}</p>{/if}
		<div><button class="btn primary" disabled={!name.trim() || creating}>Create group</button></div>
	</form>
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 44rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	h2 {
		font: 650 1.0625rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	.groups {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.groups li {
		border-bottom: 1px solid var(--rule);
	}
	.groups a {
		display: grid;
		grid-template-columns: minmax(0, 1fr) auto;
		column-gap: 1rem;
		padding: 0.75rem 0.25rem;
		color: var(--ink);
		text-decoration: none;
	}
	.groups a:hover .name {
		text-decoration: underline;
	}
	.name {
		font-weight: 700;
		overflow-wrap: anywhere;
	}
	.count {
		color: var(--ink-2);
		font-size: 0.875rem;
		white-space: nowrap;
	}
	.desc {
		grid-column: 1 / -1;
		color: var(--ink-2);
		font-size: 0.875rem;
		overflow-wrap: anywhere;
	}
	.create {
		display: grid;
		gap: 0.75rem;
		margin-top: 2rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	.msg {
		padding: 0.5rem 0;
		color: var(--ink-2);
	}
	.error {
		padding: 0.5rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
		color: var(--red);
		font-weight: 600;
		font-size: 0.9rem;
	}
</style>
