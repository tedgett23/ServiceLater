<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import { groups } from '$lib/groups.svelte';
	import { can, P, roleLabel, type GroupDetail, type RoleInfo, type User } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let group = $state<GroupDetail | null>(null);
	let loadError = $state('');
	let error = $state('');
	let busy = $state(false);

	// Editing the name and description.
	let name = $state('');
	let description = $state('');
	let restricted = $state(false);
	const dirty = $derived(
		!!group && (name.trim() !== group.name || description !== group.description || restricted !== group.restricted)
	);

	// Finding someone to add.
	let search = $state('');
	let found = $state<User[]>([]);
	let searchTimer: ReturnType<typeof setTimeout>;
	const memberIds = $derived(new Set(group?.members.map((m) => m.id) ?? []));
	const candidates = $derived(found.filter((u) => !memberIds.has(u.id)));

	let confirmingDelete = $state(false);

	// Roles granted to the group, which everyone in it holds.
	let { data } = $props();
	const manageRoles = $derived(can(data.user, P.rolesManage));
	let allRoles = $state<RoleInfo[]>([]);
	let addRole = $state('');
	const grantable = $derived(allRoles.filter((r) => !r.built_in && !group?.roles.some((h) => h.role === r.name)));

	async function load() {
		try {
			group = await api<GroupDetail>(`/api/v1/groups/${page.params.id}`);
			name = group.name;
			description = group.description;
			restricted = group.restricted;
			loadError = '';
		} catch (e) {
			loadError = e instanceof ApiError && e.status === 404 ? 'That group does not exist.' : 'Could not load this group.';
		}
	}

	onMount(() => {
		void load();
		if (manageRoles)
			void api<{ items: RoleInfo[] }>('/api/v1/roles')
				.then((r) => (allRoles = r.items))
				.catch(() => {});
	});

	async function grantRole(e: Event) {
		e.preventDefault();
		const r = allRoles.find((x) => x.name === addRole);
		if (!r) return;
		await act(() => api(`/api/v1/roles/${r.name}/grants`, { method: 'POST', body: { group_id: page.params.id } }), `${r.label} granted`);
		addRole = '';
	}

	/** Run a change, then reload this group and what the pickers elsewhere have cached. */
	async function act(request: () => Promise<unknown>, done: string) {
		busy = true;
		error = '';
		try {
			await request();
			groups.invalidate(page.params.id);
			void groups.reload();
			await load();
			ui.announce = done;
		} catch (e) {
			error = e instanceof Error ? e.message : 'That change didn’t save';
		} finally {
			busy = false;
		}
	}

	function save(e: Event) {
		e.preventDefault();
		if (!dirty) return;
		void act(
			() => api(`/api/v1/groups/${page.params.id}`, { method: 'PATCH', body: { name: name.trim(), description, restricted } }),
			'Group saved'
		);
	}

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

	const add = (u: User) =>
		act(() => api(`/api/v1/groups/${page.params.id}/members/${u.id}`, { method: 'PUT' }), `${u.name} added`);
	const remove = (u: User) =>
		act(() => api(`/api/v1/groups/${page.params.id}/members/${u.id}`, { method: 'DELETE' }), `${u.name} removed`);

	async function destroy() {
		busy = true;
		error = '';
		try {
			await api(`/api/v1/groups/${page.params.id}`, { method: 'DELETE' });
			groups.invalidate(page.params.id);
			await groups.reload();
			ui.announce = 'Group deleted';
			await goto('/admin/groups');
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not delete the group';
			confirmingDelete = false;
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>{group ? group.name : 'Group'} – ServiceLater</title></svelte:head>

<div class="page">
	<a class="back" href="/admin/groups">
		<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
		Groups
	</a>

	{#if loadError}
		<p class="msg" role="alert">{loadError}</p>
	{:else if group}
		<h1>{group.name}{#if group.restricted}{' '}<span class="restricted">Restricted</span>{/if}</h1>
		{#if group.description}<p class="lede">{group.description}</p>{/if}
		{#if group.restricted}
			<p class="hint">Records routed to this group are seen only by its members, their requesters and assignees, and people allowed to see restricted records.</p>
		{/if}

		{#if error}<p class="error" role="alert">{error}</p>{/if}

		<section aria-labelledby="members-h">
			<h2 id="members-h">Members</h2>
			{#if group.members.length === 0}
				<p class="msg">Nobody is in this group yet.</p>
			{/if}
			<ul class="members">
				{#each group.members as m (m.id)}
					<li>
						<span class="who">
							<a href="/admin/users/{m.id}"><strong>{m.name}</strong></a>
							<span class="email">{m.email}</span>
						</span>
						<span class="role">{m.deactivated_at ? 'Deactivated' : (roleLabel[m.role] ?? m.role)}</span>
						<button class="btn quiet-btn" disabled={busy} onclick={() => remove(m)} aria-label="Remove {m.name}">Remove</button>
					</li>
				{/each}
			</ul>

			<div class="add">
				<label class="field">
					<span>Add someone</span>
					<input type="search" bind:value={search} oninput={onSearch} placeholder="Search by name or email" />
				</label>
				{#if search.trim() && candidates.length === 0 && found.length > 0}
					<p class="msg">Everyone who matches is already in this group.</p>
				{/if}
				<ul class="found">
					{#each candidates as u (u.id)}
						<li>
							<span class="who">
								<strong>{u.name}</strong>
								<span class="email">{u.email}</span>
							</span>
							<button class="btn" disabled={busy} onclick={() => add(u)} aria-label="Add {u.name}">Add</button>
						</li>
					{/each}
				</ul>
			</div>
		</section>

		<section class="edit" aria-labelledby="roles-h">
			<h2 id="roles-h">Roles</h2>
			{#if group.roles.length === 0}
				<p class="hint">This group gives its members no roles.</p>
			{:else}
				<p class="hint">Everyone in this group holds these. Changing who is in it needs the roles:manage permission.</p>
			{/if}
			<ul class="members">
				{#each group.roles as r (r.grant_id)}
					<li>
						<span class="who"><a href="/admin/roles/{r.role}"><strong>{r.label}</strong></a></span>
						{#if manageRoles}
							<button
								class="btn quiet-btn"
								disabled={busy}
								onclick={() => act(() => api(`/api/v1/roles/${r.role}/grants/${r.grant_id}`, { method: 'DELETE' }), `${r.label} revoked`)}
								aria-label="Revoke {r.label}">Revoke</button
							>
						{/if}
					</li>
				{/each}
			</ul>
			{#if manageRoles && grantable.length > 0}
				<form class="row" onsubmit={grantRole}>
					<label class="field grow">
						<span>Grant a role to everyone in it</span>
						<select bind:value={addRole}>
							<option value="" disabled>Choose a role</option>
							{#each grantable as r (r.name)}
								<option value={r.name}>{r.label}</option>
							{/each}
						</select>
					</label>
					<button class="btn" disabled={!addRole || busy}>Grant</button>
				</form>
			{/if}
		</section>

		<form class="edit" onsubmit={save} aria-labelledby="edit-h">
			<h2 id="edit-h">Details</h2>
			<label class="field">
				<span>Name</span>
				<input bind:value={name} maxlength="100" required />
			</label>
			<p class="hint">Workflows name groups, as in <code>group('{group.name}')</code>. Renaming one breaks any that use the old name.</p>
			<label class="field">
				<span>Description</span>
				<input bind:value={description} maxlength="1000" />
			</label>
			<!-- Who sees the group's records changes with this, so only those who manage roles may change it. -->
			<label class="check">
				<input type="checkbox" bind:checked={restricted} disabled={!manageRoles} />
				<span>
					Restricted
					<small>Only members see the records routed to this group, as HR or Security need. Other staff stop seeing them.{#if !manageRoles}
							Changing this needs permission to manage roles.{/if}</small>
				</span>
			</label>
			<div><button class="btn primary" disabled={!dirty || !name.trim() || busy}>Save</button></div>
		</form>

		<section class="danger" aria-labelledby="delete-h">
			<h2 id="delete-h">Delete</h2>
			<p class="hint">A group that tickets are assigned to cannot be deleted. Move those tickets first.</p>
			{#if confirmingDelete}
				<div class="row">
					<button class="btn reject" disabled={busy} onclick={destroy}>Delete {group.name}</button>
					<button class="btn" onclick={() => (confirmingDelete = false)}>Keep it</button>
				</div>
			{:else}
				<div><button class="btn reject" onclick={() => (confirmingDelete = true)}>Delete group</button></div>
			{/if}
		</section>
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
		margin-bottom: 0.5rem;
	}
	.lede {
		margin-top: 0.25rem;
		color: var(--ink-2);
		overflow-wrap: anywhere;
	}
	section,
	.edit {
		margin-top: 1.75rem;
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.members li,
	.found li {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		padding: 0.55rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.who {
		display: grid;
		min-width: 0;
		margin-right: auto;
	}
	.who a {
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		color: var(--ink);
	}
	.who strong,
	.email {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.email,
	.role {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.role {
		white-space: nowrap;
	}
	.add {
		display: grid;
		gap: 0.5rem;
		margin-top: 1rem;
	}
	.edit {
		display: grid;
		gap: 0.75rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	.danger {
		display: grid;
		gap: 0.5rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	.grow {
		flex: 1 1 12rem;
	}
	.row {
		display: flex;
		flex-wrap: wrap;
		align-items: end;
		gap: 0.5rem;
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	code {
		font-size: 0.85em;
	}
	.quiet-btn {
		min-height: 2rem;
		font-weight: 500;
	}
	.btn.reject {
		color: var(--red);
	}
	.msg {
		padding: 0.5rem 0;
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
	.restricted {
		vertical-align: middle;
		margin-left: 0.4rem;
		padding: 0.1rem 0.45rem;
		border-radius: 5px;
		background: var(--marker-soft);
		color: var(--marker-ink);
		font-size: 0.75rem;
		font-weight: 600;
		letter-spacing: 0;
	}
	.check {
		display: flex;
		align-items: flex-start;
		gap: 0.55rem;
		font-weight: 550;
	}
	.check input {
		margin-top: 0.2rem;
	}
	.check small {
		display: block;
		font-weight: 400;
		color: var(--ink-2);
		font-size: 0.8125rem;
	}
</style>
