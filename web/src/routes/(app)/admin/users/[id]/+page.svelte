<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import ApiKeys from '$lib/components/ApiKeys.svelte';
	import PersonPicker from '$lib/components/PersonPicker.svelte';
	import { groups } from '$lib/groups.svelte';
	import { can, P, roleLabel, type PersonDetail, type Role, type RoleInfo } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let { data } = $props();

	let person = $state<PersonDetail | null>(null);
	let loadError = $state('');
	let error = $state('');
	let busy = $state(false);
	const self = $derived(person?.id === data.user.id);

	// Editing the details.
	let name = $state('');
	let email = $state('');
	let role = $state<Role>('end_user');
	const dirty = $derived(!!person && (name.trim() !== person.name || email.trim().toLowerCase() !== person.email || role !== person.role));

	let password = $state('');
	let addTo = $state('');
	const joinable = $derived(groups.list.filter((g) => !person?.groups.some((m) => m.id === g.id)));
	let confirmingDelete = $state(false);

	// Roles on top of the base role.
	const manageRoles = $derived(can(data.user, P.rolesManage));
	let allRoles = $state<RoleInfo[]>([]);
	let addRole = $state('');
	const grantable = $derived(allRoles.filter((r) => !r.built_in && !person?.roles.some((h) => h.role === r.name && !h.group_id)));

	async function load() {
		try {
			person = await api<PersonDetail>(`/api/v1/users/${page.params.id}`);
			name = person.name;
			email = person.email;
			role = person.role;
			loadError = '';
		} catch (e) {
			loadError = e instanceof ApiError && e.status === 404 ? 'That person does not exist.' : 'Could not load this person.';
		}
	}

	onMount(() => {
		void load();
		void groups.reload().catch(() => {});
		void api<{ items: RoleInfo[] }>('/api/v1/roles')
			.then((r) => (allRoles = r.items))
			.catch(() => {});
	});

	async function grantRole(e: Event) {
		e.preventDefault();
		const r = allRoles.find((x) => x.name === addRole);
		if (r && (await act(() => api(`/api/v1/roles/${r.name}/grants`, { method: 'POST', body: { user_id: page.params.id } }), `${r.label} granted`)))
			addRole = '';
	}

	/** Run a change, then reload this person and announce it. */
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

	const patch = (body: Record<string, unknown>, done: string) =>
		act(() => api(`/api/v1/users/${page.params.id}`, { method: 'PATCH', body }), done);

	function save(e: Event) {
		e.preventDefault();
		if (!dirty) return;
		void patch({ name: name.trim(), email: email.trim(), role }, 'Saved');
	}

	async function setPassword(e: Event) {
		e.preventDefault();
		const ok = await act(
			() => api(`/api/v1/users/${page.params.id}/password`, { method: 'PUT', body: { password } }),
			'Password changed'
		);
		if (ok) password = '';
	}

	/** Membership changes also invalidate what the ticket pickers cached about the group. */
	async function membership(groupId: string, method: 'PUT' | 'DELETE', done: string) {
		const ok = await act(() => api(`/api/v1/groups/${groupId}/members/${page.params.id}`, { method }), done);
		if (ok) {
			groups.invalidate(groupId);
			void groups.reload().catch(() => {});
		}
		return ok;
	}

	async function join(e: Event) {
		e.preventDefault();
		const g = groups.list.find((x) => x.id === addTo);
		if (g && (await membership(g.id, 'PUT', `Added to ${g.name}`))) addTo = '';
	}

	async function destroy() {
		busy = true;
		error = '';
		try {
			await api(`/api/v1/users/${page.params.id}`, { method: 'DELETE' });
			ui.announce = 'User deleted';
			await goto('/admin/users');
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not delete them';
			confirmingDelete = false;
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>{person ? person.name : 'User'} – ServiceLater</title></svelte:head>

<div class="page">
	<a class="back" href="/admin/users">
		<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
		Users
	</a>

	{#if loadError}
		<p class="msg" role="alert">{loadError}</p>
	{:else if person}
		<h1>{person.name}</h1>
		<p class="lede">
			{#if person.service_account}Service account, base role {roleLabel[person.role].toLowerCase()}{:else}{roleLabel[person.role]}{/if}{#if person.deactivated_at}<span class="off">, deactivated</span>{/if}{#if self}, you{/if}
		</p>

		{#if error}<p class="error" role="alert">{error}</p>{/if}
		{#if person.service_account && person.description}<p class="about">{person.description}</p>{/if}

		<form class="block" onsubmit={save} aria-labelledby="details-h">
			<h2 id="details-h">Details</h2>
			<label class="field">
				<span>Name</span>
				<input bind:value={name} maxlength="200" required />
			</label>
			<label class="field">
				<span>Email</span>
				<input type="email" bind:value={email} required />
			</label>
			<label class="field">
				<span>Role</span>
				<select bind:value={role} disabled={self}>
					<option value="end_user">End user</option>
					<option value="agent">Agent</option>
					<option value="admin" disabled={!manageRoles && person.role !== 'admin'}>Admin</option>
				</select>
			</label>
			{#if self}<p class="hint">You cannot change your own role. Another admin can.</p>{/if}
			<div><button class="btn primary" disabled={!dirty || !name.trim() || !email.trim() || busy}>Save</button></div>
		</form>

		{#if person.service_account}
			<ApiKeys userId={person.id} permissions={person.permissions} disabled={!!person.deactivated_at} />
		{:else}
		<section class="block" aria-labelledby="manager-h">
			<h2 id="manager-h">Manager</h2>
			<p class="hint">Approves what {person.name} requests, when a request’s workflow asks the manager.</p>
			<PersonPicker
				id="manager"
				label="Manager"
				value={person.manager_id}
				name={person.manager_name}
				exclude={[person.id]}
				disabled={busy}
				onchange={(id, who) => patch({ manager_id: id }, id ? `${who} is now the manager` : 'Manager removed')}
			/>
		</section>
		{/if}

		<section class="block" aria-labelledby="groups-h">
			<h2 id="groups-h">Groups</h2>
			{#if person.groups.length === 0}
				<p class="msg">Not in any group.</p>
			{/if}
			<ul class="groups">
				{#each person.groups as g (g.id)}
					<li>
						<a href="/admin/groups/{g.id}">{g.name}</a>
						<button
							class="btn quiet-btn"
							disabled={busy}
							onclick={() => membership(g.id, 'DELETE', `Removed from ${g.name}`)}
							aria-label="Remove from {g.name}">Remove</button
						>
					</li>
				{/each}
			</ul>
			{#if joinable.length > 0}
				<form class="row" onsubmit={join}>
					<label class="field grow">
						<span>Add to a group</span>
						<select bind:value={addTo}>
							<option value="" disabled>Choose a group</option>
							{#each joinable as g (g.id)}
								<option value={g.id}>{g.name}</option>
							{/each}
						</select>
					</label>
					<button class="btn" disabled={!addTo || busy}>Add</button>
				</form>
			{/if}
		</section>

		<section class="block" aria-labelledby="roles-h">
			<h2 id="roles-h">Roles</h2>
			<p class="hint">Base role: {roleLabel[person.role]}. These add to it.</p>
			{#if person.roles.length === 0}
				<p class="msg">No other roles.</p>
			{/if}
			<ul class="groups">
				{#each person.roles as r (r.grant_id)}
					<li>
						<span>
							<a href="/admin/roles/{r.role}">{r.label}</a>
							{#if r.group_name}<span class="via">through {r.group_name}</span>{/if}
						</span>
						{#if manageRoles && !r.group_id}
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
						<span>Grant a role</span>
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

		{#if !person.service_account}
		<form class="block" onsubmit={setPassword} aria-labelledby="password-h">
			<h2 id="password-h">Password</h2>
			<label class="field">
				<span>New password</span>
				<input type="password" bind:value={password} minlength="8" maxlength="72" required autocomplete="new-password" />
			</label>
			<p class="hint">At least 8 characters.{self ? '' : ' Setting it signs them out everywhere.'}</p>
			<div><button class="btn" disabled={password.length < 8 || busy}>Set password</button></div>
		</form>
		{/if}

		{#if !self}
			<section class="block" aria-labelledby="access-h">
				<h2 id="access-h">Access</h2>
				{#if person.service_account}
					<p class="hint">
						{person.deactivated_at
							? 'Deactivated: none of its keys work.'
							: 'Deactivating stops every one of its keys at once. Its records and history stay.'}
					</p>
					<div>
						{#if person.deactivated_at}
							<button class="btn" disabled={busy} onclick={() => patch({ deactivated: false }, 'Reactivated')}>Reactivate</button>
						{:else}
							<button class="btn reject" disabled={busy} onclick={() => patch({ deactivated: true }, 'Deactivated')}>Deactivate</button>
						{/if}
					</div>
				{:else if person.deactivated_at}
					<p class="hint">Deactivated: they cannot sign in, and are not offered as an assignee or asked to approve anything.</p>
					<div><button class="btn" disabled={busy} onclick={() => patch({ deactivated: false }, 'Reactivated')}>Reactivate</button></div>
				{:else}
					<p class="hint">
						Deactivating signs them out and stops them signing in. Their tickets, comments and group memberships stay.
					</p>
					<div>
						<button class="btn reject" disabled={busy} onclick={() => patch({ deactivated: true }, 'Deactivated')}>Deactivate</button>
					</div>
				{/if}
			</section>

			<section class="block" aria-labelledby="delete-h">
				<h2 id="delete-h">Delete</h2>
				<p class="hint">Only someone with no tickets, comments or approvals can be deleted. Deactivate anyone else.</p>
				{#if confirmingDelete}
					<div class="row">
						<button class="btn reject" disabled={busy} onclick={destroy}>Delete {person.name}</button>
						<button class="btn" onclick={() => (confirmingDelete = false)}>Keep them</button>
					</div>
				{:else}
					<div><button class="btn reject" onclick={() => (confirmingDelete = true)}>Delete user</button></div>
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
		margin-top: 0.25rem;
		color: var(--ink-2);
	}
	.off {
		color: var(--red);
		font-weight: 600;
	}
	.about {
		margin-top: 0.5rem;
		color: var(--ink-2);
	}
	.block {
		display: grid;
		gap: 0.75rem;
		margin-top: 1.5rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	.groups {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.groups li {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
		padding: 0.45rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.via {
		margin-left: 0.4rem;
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.groups a {
		font-weight: 600;
		overflow-wrap: anywhere;
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
	.btn.reject {
		color: var(--red);
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
</style>
