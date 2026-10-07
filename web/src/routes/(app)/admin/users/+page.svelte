<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { roleLabel, type Person, type Role } from '$lib/types';

	const PAGE = 50;

	let people = $state<Person[]>([]);
	let hasMore = $state(false);
	let loading = $state(true);
	let error = $state('');
	let search = $state('');
	let role = $state<Role | ''>('');
	let showDeactivated = $state(false);
	let searchTimer: ReturnType<typeof setTimeout>;
	let generation = 0;

	// Adding someone.
	let name = $state('');
	let email = $state('');
	let newRole = $state<Role>('end_user');
	let password = $state('');
	let creating = $state(false);
	let createError = $state('');

	function query(offset: number) {
		const p = new URLSearchParams({ limit: String(PAGE), offset: String(offset) });
		if (search.trim()) p.set('q', search.trim());
		if (role) p.set('role', role);
		if (showDeactivated) p.set('include_deactivated', 'true');
		return `/api/v1/users?${p}`;
	}

	async function load(more = false) {
		const gen = more ? generation : ++generation;
		loading = true;
		error = '';
		try {
			const page = (await api<{ items: Person[] }>(query(more ? people.length : 0))).items;
			if (gen !== generation) return; // a newer search superseded this one
			people = more ? [...people, ...page] : page;
			hasMore = page.length === PAGE;
		} catch (e) {
			if (gen === generation) error = e instanceof Error ? e.message : 'Could not load people';
		} finally {
			if (gen === generation) loading = false;
		}
	}

	onMount(() => void load());

	function onSearch() {
		clearTimeout(searchTimer);
		searchTimer = setTimeout(() => void load(), 200);
	}

	async function create(e: Event) {
		e.preventDefault();
		if (creating) return;
		creating = true;
		createError = '';
		try {
			const p = await api<Person>('/api/v1/users', {
				method: 'POST',
				body: { name: name.trim(), email: email.trim(), role: newRole, password }
			});
			await goto(`/admin/users/${p.id}`);
		} catch (err) {
			createError = err instanceof Error ? err.message : 'Could not add them';
		} finally {
			creating = false;
		}
	}
</script>

<svelte:head><title>Users – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Users</h1>
	<p class="lede">Everyone who can sign in. Agents and admins work tickets; admins also manage people and groups.</p>

	<div class="filters">
		<label class="field grow">
			<span class="sr-only">Search people</span>
			<input type="search" bind:value={search} oninput={onSearch} placeholder="Search by name or email" />
		</label>
		<label class="field">
			<span class="sr-only">Role</span>
			<select bind:value={role} onchange={() => void load()}>
				<option value="">Any role</option>
				<option value="admin">Admins</option>
				<option value="agent">Agents</option>
				<option value="end_user">End users</option>
			</select>
		</label>
		<label class="check">
			<input type="checkbox" bind:checked={showDeactivated} onchange={() => void load()} />
			Show deactivated
		</label>
	</div>

	{#if error}
		<p class="msg" role="alert">{error}</p>
	{:else if !loading && people.length === 0}
		<p class="msg">Nobody matches.</p>
	{/if}

	<ul class="people">
		{#each people as p (p.id)}
			<li class:off={p.deactivated_at}>
				<a href="/admin/users/{p.id}">
					<span class="name">{p.name}</span>
					<span class="role">{p.deactivated_at ? 'Deactivated' : roleLabel[p.role]}</span>
					<span class="email">{p.email}</span>
				</a>
			</li>
		{/each}
	</ul>
	{#if hasMore}
		<div class="more"><button class="btn" disabled={loading} onclick={() => load(true)}>Show more</button></div>
	{/if}

	<form class="create" onsubmit={create}>
		<h2>Add someone</h2>
		<div class="pair">
			<label class="field">
				<span>Name</span>
				<input bind:value={name} maxlength="200" required autocomplete="off" />
			</label>
			<label class="field">
				<span>Email</span>
				<input type="email" bind:value={email} required autocomplete="off" />
			</label>
		</div>
		<div class="pair">
			<label class="field">
				<span>Role</span>
				<select bind:value={newRole}>
					<option value="end_user">End user</option>
					<option value="agent">Agent</option>
					<option value="admin">Admin</option>
				</select>
			</label>
			<label class="field">
				<span>Password</span>
				<input type="password" bind:value={password} minlength="8" maxlength="72" required autocomplete="new-password" />
			</label>
		</div>
		<p class="hint">At least 8 characters. Give it to them yourself: ServiceLater does not send email yet.</p>
		{#if createError}<p class="error" role="alert">{createError}</p>{/if}
		<div>
			<button class="btn primary" disabled={!name.trim() || !email.trim() || password.length < 8 || creating}>Add user</button>
		</div>
	</form>
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 48rem;
	}
	h1 {
		font: 800 1.6rem/1.2 var(--font-display);
	}
	h2 {
		font: 700 1.25rem/1.2 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	.filters {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.5rem 0.75rem;
		margin-bottom: 0.5rem;
	}
	.grow {
		flex: 1 1 14rem;
	}
	.check {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		font-size: 0.9rem;
		white-space: nowrap;
	}
	.people {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.people li {
		border-bottom: 1px solid var(--rule);
	}
	.people a {
		display: grid;
		grid-template-columns: minmax(0, 1fr) auto;
		column-gap: 1rem;
		padding: 0.65rem 0.25rem;
		color: var(--ink);
		text-decoration: none;
	}
	.people a:hover .name {
		text-decoration: underline;
	}
	.off .name {
		color: var(--ink-2);
	}
	.name {
		font-weight: 700;
		overflow-wrap: anywhere;
	}
	.role,
	.email {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.role {
		white-space: nowrap;
	}
	.email {
		grid-column: 1 / -1;
		overflow-wrap: anywhere;
	}
	.more {
		margin-top: 0.75rem;
	}
	.create {
		display: grid;
		gap: 0.75rem;
		margin-top: 2rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	.pair {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 0.75rem;
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.85rem;
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
	@media (max-width: 40rem) {
		.pair {
			grid-template-columns: 1fr;
		}
	}
</style>
