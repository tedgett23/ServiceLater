<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { groups } from '$lib/groups.svelte';
	import { ui } from '$lib/ui.svelte';

	/** A single sign-on provider as the API gives it. */
	interface Provider {
		id: string;
		label: string;
		issuer: string;
		client_id: string;
		secret_set: boolean;
		allowed_domains: string[];
		create_users: boolean;
		groups_claim: string;
		group_maps: { claim_value: string; group_id: string; group_name?: string }[];
		enabled: boolean;
		redirect_uri: string;
	}

	let providers = $state<Provider[]>([]);
	let passwords = $state<'everyone' | 'admins'>('everyone');
	let loadError = $state('');
	let error = $state('');
	let busy = $state(false);

	// The provider being added or changed. isNew: its id can still be chosen.
	let editing = $state<(Provider & { isNew: boolean; secret: string; clearSecret: boolean; domains: string }) | null>(null);
	let confirmDelete = $state(false);

	async function load() {
		try {
			providers = (await api<{ items: Provider[] }>('/api/v1/sso-providers')).items;
			passwords = (await api<{ passwords: 'everyone' | 'admins' }>('/api/v1/settings/sign-in')).passwords;
			loadError = '';
		} catch (e) {
			loadError = e instanceof Error ? e.message : 'Could not load the sign-in settings';
		}
	}
	onMount(() => {
		void load();
		void groups.reload().catch(() => {});
	});

	function startNew() {
		error = '';
		confirmDelete = false;
		editing = {
			isNew: true, id: '', label: '', issuer: '', client_id: '', secret_set: false, allowed_domains: [], create_users: true,
			groups_claim: '', group_maps: [], enabled: true, redirect_uri: '', secret: '', clearSecret: false, domains: ''
		};
	}
	function edit(p: Provider) {
		error = '';
		confirmDelete = false;
		editing = { ...p, group_maps: p.group_maps.map((m) => ({ ...m })), isNew: false, secret: '', clearSecret: false, domains: p.allowed_domains.join(', ') };
	}

	// The redirect address for a provider not saved yet, worked out the way the server does when no base URL is set.
	const redirectFor = (id: string) =>
		providers.find((p) => p.id === id)?.redirect_uri ?? `${location.origin}/api/v1/auth/sso/${id || '<id>'}/callback`;

	async function save(e: Event) {
		e.preventDefault();
		if (!editing) return;
		busy = true;
		error = '';
		const p = editing;
		const body: Record<string, unknown> = {
			label: p.label.trim(), issuer: p.issuer.trim(), client_id: p.client_id.trim(), create_users: p.create_users,
			groups_claim: p.groups_claim.trim(), enabled: p.enabled,
			allowed_domains: p.domains.split(/[\s,]+/).filter(Boolean),
			group_maps: p.group_maps.filter((m) => m.claim_value.trim() && m.group_id).map((m) => ({ claim_value: m.claim_value.trim(), group_id: m.group_id }))
		};
		if (p.secret) body.client_secret = p.secret;
		else if (p.clearSecret) body.client_secret = null;
		try {
			await api(`/api/v1/sso-providers/${p.id.trim()}`, { method: 'PUT', body });
			ui.announce = `${p.label} saved`;
			editing = null;
			await load();
		} catch (err) {
			error = err instanceof Error ? err.message : 'The provider wasn’t saved';
		} finally {
			busy = false;
		}
	}

	async function remove() {
		if (!editing) return;
		busy = true;
		try {
			await api(`/api/v1/sso-providers/${editing.id}`, { method: 'DELETE' });
			ui.announce = `${editing.label} removed`;
			editing = null;
			await load();
		} catch (err) {
			error = err instanceof Error ? err.message : 'The provider wasn’t removed';
		} finally {
			busy = false;
		}
	}

	async function setPasswords(value: 'everyone' | 'admins') {
		error = '';
		try {
			passwords = (await api<{ passwords: 'everyone' | 'admins' }>('/api/v1/settings/sign-in', { method: 'PUT', body: { passwords: value } })).passwords;
			ui.announce = value === 'admins' ? 'Passwords are for admins only' : 'Everyone may use a password';
		} catch (err) {
			error = err instanceof Error ? err.message : 'That didn’t save';
			await load();
		}
	}

	async function copy(text: string) {
		try {
			await navigator.clipboard.writeText(text);
			ui.announce = 'Copied';
		} catch {
			/* selectable as well */
		}
	}
</script>

<svelte:head><title>Sign-in – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Sign-in</h1>
	<p class="lede">
		Let people sign in with your organisation’s identity provider (OpenID Connect: Okta, Microsoft Entra ID, Google Workspace,
		Keycloak and others), and decide who may still use a password.
	</p>
	{#if loadError}<p class="error" role="alert">{loadError}</p>{/if}
	{#if error && !editing}<p class="error" role="alert">{error}</p>{/if}

	<section class="block" aria-labelledby="providers-h">
		<div class="head">
			<h2 id="providers-h">Single sign-on</h2>
			{#if !editing}<button class="btn" type="button" onclick={startNew}>Add a provider</button>{/if}
		</div>
		{#if providers.length === 0 && !editing}
			<p class="msg">No providers yet: everyone signs in with a password.</p>
		{/if}
		<ul class="list">
			{#each providers as p (p.id)}
				<li>
					<div>
						<span class="name">{p.label}</span>
						{#if !p.enabled}<span class="off">Off</span>{/if}
						<div class="meta">
							<span>{p.issuer}</span>
							<span>{p.allowed_domains.length ? p.allowed_domains.join(', ') : 'Any domain'}</span>
							<span>{p.create_users ? 'Creates accounts' : 'Existing accounts only'}</span>
							{#if p.group_maps.length}<span>{p.group_maps.length} group mapping{p.group_maps.length === 1 ? '' : 's'}</span>{/if}
						</div>
					</div>
					<button class="btn quiet-btn" type="button" onclick={() => edit(p)} aria-label="Edit {p.label}">Edit</button>
				</li>
			{/each}
		</ul>

		{#if editing}
			<form class="editor" onsubmit={save} aria-label={editing.isNew ? 'New provider' : `Edit ${editing.label}`}>
				<div class="pair">
					<label class="field">
						<span>Button label</span>
						<input bind:value={editing.label} maxlength="60" required placeholder="Okta" />
					</label>
					<label class="field">
						<span>Short name (in the address)</span>
						<input bind:value={editing.id} pattern="[a-z][a-z0-9\-]{'{'}1,30{'}'}" required disabled={!editing.isNew} placeholder="okta" />
					</label>
				</div>
				<div class="redirect">
					<span>Register this redirect address at the provider:</span>
					<code>{redirectFor(editing.id)}</code>
					<button class="btn quiet-btn" type="button" onclick={() => copy(redirectFor(editing!.id))}>Copy</button>
				</div>
				<label class="field">
					<span>Issuer</span>
					<input bind:value={editing.issuer} required placeholder="https://example.okta.com" />
				</label>
				<div class="pair">
					<label class="field">
						<span>Client id</span>
						<input bind:value={editing.client_id} required autocomplete="off" />
					</label>
					<label class="field">
						<span>Client secret</span>
						<input type="password" bind:value={editing.secret} autocomplete="new-password"
							placeholder={editing.secret_set ? 'Stored: leave empty to keep it' : 'None (a public client)'} />
					</label>
				</div>
				{#if editing.secret_set}
					<label class="check"><input type="checkbox" bind:checked={editing.clearSecret} disabled={!!editing.secret} /> Remove the stored secret</label>
				{/if}
				<label class="field">
					<span>Allowed email domains</span>
					<input bind:value={editing.domains} placeholder="example.com, example.org (empty: any)" />
				</label>
				<label class="check"><input type="checkbox" bind:checked={editing.create_users} /> Give people an account the first time they sign in (as end users)</label>
				<label class="check"><input type="checkbox" bind:checked={editing.enabled} /> Offer it on the sign-in page</label>

				<fieldset class="maps">
					<legend>Groups</legend>
					<label class="field">
						<span>Groups claim</span>
						<input bind:value={editing.groups_claim} placeholder="groups (empty: groups are not synced)" />
					</label>
					<p class="hint">
						At each sign-in, people join the groups their claim values map to, and leave the mapped groups theirs no longer do. Groups
						not mapped here are left alone.
					</p>
					{#each editing.group_maps as m, i (i)}
						<div class="maprow">
							<label class="field grow">
								<span>Claim value</span>
								<input bind:value={m.claim_value} placeholder="sl-service-desk" />
							</label>
							<label class="field grow">
								<span>Group</span>
								<select bind:value={m.group_id}>
									<option value="" disabled>Choose a group</option>
									{#each groups.list as g (g.id)}<option value={g.id}>{g.name}</option>{/each}
								</select>
							</label>
							<button class="btn quiet-btn" type="button" aria-label="Remove mapping {i + 1}"
								onclick={() => editing!.group_maps.splice(i, 1)}>Remove</button>
						</div>
					{/each}
					<div>
						<button class="btn" type="button" disabled={!editing.groups_claim.trim()}
							onclick={() => editing!.group_maps.push({ claim_value: '', group_id: '' })}>Add a mapping</button>
					</div>
				</fieldset>

				{#if error}<p class="error" role="alert">{error}</p>{/if}
				<div class="buttons">
					<button class="btn primary" disabled={busy}>Save</button>
					<button class="btn" type="button" onclick={() => (editing = null)}>Cancel</button>
					{#if !editing.isNew}
						{#if confirmDelete}
							<button class="btn reject" type="button" disabled={busy} onclick={remove}>Remove {editing.label}</button>
						{:else}
							<button class="btn reject" type="button" onclick={() => (confirmDelete = true)}>Remove</button>
						{/if}
					{/if}
				</div>
			</form>
		{/if}
	</section>

	<section class="block" aria-labelledby="passwords-h">
		<h2 id="passwords-h">Passwords</h2>
		<label class="check">
			<input type="radio" name="passwords" value="everyone" checked={passwords === 'everyone'} onchange={() => setPasswords('everyone')} />
			Anyone with a password may use it
		</label>
		<label class="check">
			<input type="radio" name="passwords" value="admins" checked={passwords === 'admins'} onchange={() => setPasswords('admins')} />
			Admins only; everyone else signs in with single sign-on
		</label>
		<p class="hint">Admins keep their passwords so a broken provider cannot lock everyone out.</p>
	</section>
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 48rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	h2 {
		font: 650 1.0625rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 0.5rem;
		color: var(--ink-2);
	}
	.block {
		display: grid;
		gap: 0.75rem;
		margin-top: 1.5rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	.head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
	}
	.msg,
	.hint,
	.meta {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.list {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.list li {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: 0.75rem;
		padding: 0.6rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.name {
		font-weight: 650;
	}
	.off {
		margin-left: 0.4rem;
		font-size: 0.8125rem;
		color: var(--ink-2);
	}
	.meta {
		display: flex;
		flex-wrap: wrap;
		gap: 0 1rem;
		overflow-wrap: anywhere;
	}
	.editor {
		display: grid;
		gap: 0.75rem;
		padding: 1rem;
		border: 1px solid var(--rule);
		border-radius: 8px;
		background: var(--surface);
	}
	.pair {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 0.75rem;
	}
	.redirect {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.4rem 0.6rem;
		padding: 0.5rem 0.75rem;
		border-left: 3px solid var(--marker);
		background: var(--raised);
		font-size: 0.875rem;
	}
	.redirect code {
		font-family: var(--font-mono);
		font-size: 0.8125rem;
		overflow-wrap: anywhere;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.45rem;
		font-size: 0.9rem;
	}
	.maps {
		display: grid;
		gap: 0.6rem;
		margin: 0;
		padding: 0.75rem;
		border: 1px solid var(--rule);
		border-radius: 8px;
	}
	.maps legend {
		font-weight: 600;
		font-size: 0.9rem;
	}
	.maprow {
		display: flex;
		flex-wrap: wrap;
		align-items: end;
		gap: 0.6rem;
	}
	.grow {
		flex: 1 1 12rem;
	}
	.buttons {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
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
