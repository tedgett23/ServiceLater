<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import { groups } from '$lib/groups.svelte';
	import { can, P, type PermissionInfo, type RoleDetail, type User } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let { data } = $props();
	const manage = $derived(can(data.user, P.rolesManage));

	let role = $state<RoleDetail | null>(null);
	let catalog = $state<PermissionInfo[]>([]);
	let loadError = $state('');
	let error = $state('');
	let busy = $state(false);

	let search = $state('');
	let found = $state<User[]>([]);
	let searchTimer: ReturnType<typeof setTimeout>;
	let groupId = $state('');
	const holders = $derived(new Set(role?.granted_to.map((g) => g.user_id ?? g.group_id) ?? []));

	const describe = (p: string) => catalog.find((c) => c.name === p)?.description ?? '';

	async function load() {
		try {
			role = await api<RoleDetail>(`/api/v1/roles/${page.params.name}`);
			loadError = '';
		} catch (e) {
			loadError = e instanceof ApiError && e.status === 404 ? 'That role does not exist.' : 'Could not load this role.';
		}
	}

	onMount(() => {
		void load();
		void api<{ items: PermissionInfo[] }>('/api/v1/permissions').then((r) => (catalog = r.items));
		void groups.reload().catch(() => {});
	});

	async function act(request: () => Promise<unknown>, done: string) {
		busy = true;
		error = '';
		try {
			await request();
			await load();
			ui.announce = done;
			return true;
		} catch (e) {
			error = e instanceof Error ? e.message : 'That change didn’t save';
			return false;
		} finally {
			busy = false;
		}
	}

	const grant = (body: { user_id?: string; group_id?: string }, done: string) =>
		act(() => api(`/api/v1/roles/${page.params.name}/grants`, { method: 'POST', body }), done);

	function onSearch() {
		clearTimeout(searchTimer);
		const q = search.trim();
		if (!q) {
			found = [];
			return;
		}
		searchTimer = setTimeout(async () => {
			try {
				found = (await api<{ items: User[] }>(`/api/v1/users?${new URLSearchParams({ q, limit: '8' })}`)).items;
			} catch {
				found = [];
			}
		}, 200);
	}

	async function grantGroup(e: Event) {
		e.preventDefault();
		const g = groups.list.find((x) => x.id === groupId);
		if (g && (await grant({ group_id: g.id }, `Granted to ${g.name}`))) groupId = '';
	}
</script>

<svelte:head><title>{role ? role.label : 'Role'} – ServiceLater</title></svelte:head>

<div class="page">
	<a class="back" href="/admin/roles">
		<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
		Roles
	</a>

	{#if loadError}
		<p class="msg" role="alert">{loadError}</p>
	{:else if role}
		<h1>{role.label}</h1>
		<p class="lede">
			<code>{role.name}</code> · {role.built_in ? 'Built-in base role' : `From ${role.app}`}
		</p>
		{#if role.description}<p>{role.description}</p>{/if}

		{#if error}<p class="error" role="alert">{error}</p>{/if}

		<section class="block" aria-labelledby="perms-h">
			<h2 id="perms-h">What it allows</h2>
			{#if role.permissions.length === 0}<p class="msg">Nothing beyond filing and following their own tickets.</p>{/if}
			<ul class="perms">
				{#each role.permissions as p (p)}
					<li><code>{p}</code> <span>{describe(p)}</span></li>
				{/each}
			</ul>
		</section>

		{#if role.built_in}
			<p class="hint block">A base role is set on each person (Users, then a person's Role), not granted.</p>
		{:else}
			<section class="block" aria-labelledby="grants-h">
				<h2 id="grants-h">Granted to</h2>
				{#if role.granted_to.length === 0}<p class="msg">Nobody yet.</p>{/if}
				<ul class="grants">
					{#each role.granted_to as g (g.id)}
						<li>
							{#if g.group_id}
								<span class="who"><a href="/admin/groups/{g.group_id}"><strong>{g.group_name}</strong></a><span class="sub">Group: everyone in it</span></span>
							{:else}
								<span class="who"><a href="/admin/users/{g.user_id}"><strong>{g.user_name}</strong></a><span class="sub">{g.user_email}</span></span>
							{/if}
							{#if manage}
								<button
									class="btn quiet-btn"
									disabled={busy}
									onclick={() => act(() => api(`/api/v1/roles/${role!.name}/grants/${g.id}`, { method: 'DELETE' }), 'Revoked')}
									aria-label="Revoke from {g.group_name ?? g.user_name}">Revoke</button
								>
							{/if}
						</li>
					{/each}
				</ul>

				{#if manage}
					<div class="add">
						<label class="field">
							<span>Grant to someone</span>
							<input type="search" bind:value={search} oninput={onSearch} placeholder="Search by name or email" />
						</label>
						<ul class="grants">
							{#each found.filter((u) => !holders.has(u.id)) as u (u.id)}
								<li>
									<span class="who"><strong>{u.name}</strong><span class="sub">{u.email}</span></span>
									<button class="btn" disabled={busy} onclick={() => grant({ user_id: u.id }, `Granted to ${u.name}`)} aria-label="Grant to {u.name}"
										>Grant</button
									>
								</li>
							{/each}
						</ul>
						<form class="row" onsubmit={grantGroup}>
							<label class="field grow">
								<span>Grant to a group</span>
								<select bind:value={groupId}>
									<option value="" disabled>Choose a group</option>
									{#each groups.list.filter((x) => !holders.has(x.id)) as x (x.id)}
										<option value={x.id}>{x.name}</option>
									{/each}
								</select>
							</label>
							<button class="btn" disabled={!groupId || busy}>Grant</button>
						</form>
						<p class="hint">Granting to a group gives the role to everyone in it, and changing that group's members then needs this permission too.</p>
					</div>
				{/if}
			</section>
		{/if}
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 44rem;
	}
	.back {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		margin-bottom: 0.75rem;
		font-weight: 600;
		text-decoration: none;
	}
	h1 {
		font: 650 1.5rem/1.3 var(--font-display);
		overflow-wrap: anywhere;
	}
	h2 {
		font: 650 1.0625rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 0.5rem;
		color: var(--ink-2);
	}
	code {
		font-size: 0.85em;
	}
	.block {
		display: grid;
		gap: 0.6rem;
		margin-top: 1.5rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.perms li {
		display: grid;
		grid-template-columns: minmax(9rem, auto) 1fr;
		gap: 0.75rem;
		padding: 0.4rem 0;
		border-bottom: 1px solid var(--rule);
		font-size: 0.9rem;
	}
	.perms span {
		color: var(--ink-2);
	}
	.grants li {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		padding: 0.5rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.who {
		display: grid;
		min-width: 0;
		margin-right: auto;
	}
	.who a {
		color: var(--ink);
	}
	.sub {
		color: var(--ink-2);
		font-size: 0.85rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.add {
		display: grid;
		gap: 0.6rem;
		margin-top: 0.5rem;
	}
	.row {
		display: flex;
		flex-wrap: wrap;
		align-items: end;
		gap: 0.5rem;
	}
	.grow {
		flex: 1 1 12rem;
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.quiet-btn {
		min-height: 2rem;
		font-weight: 500;
	}
	.msg {
		padding: 0.25rem 0;
		color: var(--ink-2);
	}
	.error {
		margin-top: 1rem;
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
		color: var(--red);
		font-weight: 600;
		font-size: 0.9rem;
	}
	@media (max-width: 40rem) {
		.perms li {
			grid-template-columns: 1fr;
			gap: 0.1rem;
		}
	}
</style>
