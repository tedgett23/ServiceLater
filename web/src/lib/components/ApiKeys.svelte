<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import type { ApiKey } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	/**
	 * A service account's API keys: make one (named, perhaps expiring, perhaps narrowed to some of the account's
	 * permissions), see when each was last used, and revoke them. A new key is shown once, here, and never again.
	 */
	let { userId, permissions, disabled = false }: { userId: string; permissions: string[]; disabled?: boolean } = $props();

	let keys = $state<ApiKey[]>([]);
	let error = $state('');
	let busy = $state(false);
	let adding = $state(false);
	let name = $state('');
	let expires = $state('');
	let narrow = $state(false);
	let scopes = $state<string[]>([]);
	let made = $state<{ name: string; key: string } | null>(null);
	let copied = $state(false);
	let confirming = $state('');
	/** The key whose rate limit is being changed, and the choice so far. */
	let limiting = $state('');
	let limitMode = $state<'setting' | 'own' | 'none'>('setting');
	let limitValue = $state(1200);

	const origin = typeof location === 'undefined' ? '' : location.origin;
	const today = new Date().toISOString().slice(0, 10);

	async function load() {
		try {
			keys = (await api<{ items: ApiKey[] }>(`/api/v1/users/${userId}/api-keys`)).items;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the keys';
		}
	}
	onMount(() => void load());

	function keyState(k: ApiKey) {
		if (k.revoked_at) return 'Revoked';
		if (k.expires_at && new Date(k.expires_at) <= new Date()) return 'Expired';
		return '';
	}
	const day = (s: string) => new Date(s).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
	const when = (s: string) => new Date(s).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });

	async function create(e: Event) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			const body: Record<string, unknown> = { name: name.trim() };
			// The end of the chosen day, in the person's time zone.
			if (expires) body.expires_at = new Date(`${expires}T23:59:59`).toISOString();
			if (narrow) body.scopes = scopes;
			const k = await api<ApiKey & { key: string }>(`/api/v1/users/${userId}/api-keys`, { method: 'POST', body });
			made = { name: k.name, key: k.key };
			copied = false;
			adding = narrow = false;
			name = expires = '';
			scopes = [];
			ui.announce = 'Key made. Copy it now: it will not be shown again.';
			await load();
		} catch (err) {
			error = err instanceof Error ? err.message : 'The key wasn’t made';
		} finally {
			busy = false;
		}
	}

	async function copy() {
		if (!made) return;
		try {
			await navigator.clipboard.writeText(made.key);
			copied = true;
			ui.announce = 'Key copied';
		} catch {
			/* the key is selectable text as well */
		}
	}

	function editLimit(k: ApiKey) {
		limiting = k.id;
		limitMode = k.rate_limit === null ? 'setting' : k.rate_limit === 0 ? 'none' : 'own';
		limitValue = k.rate_limit || 1200;
	}

	const limitOk = $derived(limitMode !== 'own' || (Number.isInteger(limitValue) && limitValue >= 10 && limitValue <= 1_000_000));

	async function saveLimit(k: ApiKey) {
		busy = true;
		error = '';
		try {
			const rate_limit = limitMode === 'setting' ? null : limitMode === 'none' ? 0 : limitValue;
			await api(`/api/v1/users/${userId}/api-keys/${k.id}`, { method: 'PATCH', body: { rate_limit } });
			limiting = '';
			ui.announce = `${k.name}’s rate limit saved`;
			await load();
		} catch (err) {
			error = err instanceof Error ? err.message : 'The rate limit wasn’t saved';
		} finally {
			busy = false;
		}
	}

	const limitText = (k: ApiKey) =>
		k.rate_limit === null ? 'Rate limit: the setting for keys' : k.rate_limit === 0 ? 'No rate limit' : `Rate limit: ${k.rate_limit.toLocaleString()} a minute`;

	async function revoke(k: ApiKey) {
		busy = true;
		error = '';
		try {
			await api(`/api/v1/users/${userId}/api-keys/${k.id}`, { method: 'DELETE' });
			confirming = '';
			ui.announce = `${k.name} revoked`;
			await load();
		} catch (err) {
			error = err instanceof Error ? err.message : 'The key wasn’t revoked';
		} finally {
			busy = false;
		}
	}
</script>

<section class="block" aria-labelledby="keys-h">
	<div class="head">
		<h2 id="keys-h">API keys</h2>
		{#if !adding && !disabled}<button class="btn" type="button" onclick={() => ((adding = true), (made = null))}>New key</button>{/if}
	</div>
	<p class="hint">
		The integration sends a key with every request, as <code>Authorization: Bearer slk_…</code>, and acts as this account.
		Deactivating the account stops all of its keys.
	</p>

	{#if made}
		<div class="made" role="region" aria-label="New key">
			<p><strong>{made.name}</strong>: copy the key now. It is not shown again; if it is lost, revoke it and make another.</p>
			<div class="keyrow">
				<code class="key">{made.key}</code>
				<button class="btn primary" type="button" onclick={copy}>{copied ? 'Copied' : 'Copy'}</button>
			</div>
			<p class="example">Try it: <code>curl -H "Authorization: Bearer {made.key.slice(0, 12)}…" {origin}/api/v1/auth/me</code></p>
			<div><button class="btn" type="button" onclick={() => (made = null)}>Done</button></div>
		</div>
	{/if}

	{#if adding}
		<form class="add" onsubmit={create}>
			<div class="pair">
				<label class="field">
					<span>Name</span>
					<!-- svelte-ignore a11y_autofocus -->
					<input bind:value={name} maxlength="100" required autofocus placeholder="Where it is used" />
				</label>
				<label class="field">
					<span>Expires</span>
					<input type="date" bind:value={expires} min={today} />
				</label>
			</div>
			<p class="hint">Leave Expires empty for a key that works until it is revoked.</p>
			<label class="check">
				<input type="checkbox" bind:checked={narrow} disabled={permissions.length === 0} />
				Only some of what this account can do
			</label>
			{#if narrow}
				<fieldset class="scopes">
					<legend>The key may</legend>
					{#each permissions as p (p)}
						<label class="check"><input type="checkbox" value={p} bind:group={scopes} /> <code>{p}</code></label>
					{/each}
				</fieldset>
			{/if}
			<div class="buttons">
				<button class="btn primary" disabled={busy || !name.trim() || (narrow && scopes.length === 0)}>Make key</button>
				<button class="btn" type="button" onclick={() => (adding = false)}>Cancel</button>
			</div>
		</form>
	{/if}

	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if keys.length === 0}
		<p class="msg">No keys yet.</p>
	{:else}
		<ul class="keys">
			{#each keys as k (k.id)}
				<li class:off={keyState(k)}>
					<div class="what">
						<span class="name">{k.name}</span>
						<code class="prefix">{k.prefix}…</code>
						{#if keyState(k)}<span class="state">{keyState(k)}</span>{/if}
					</div>
					<div class="meta">
						<span>Made {day(k.created_at)}{k.created_by_name ? ` by ${k.created_by_name}` : ''}</span>
						<span>{k.last_used_at ? `Last used ${when(k.last_used_at)}` : 'Never used'}</span>
						{#if k.expires_at && !k.revoked_at}<span>Expires {day(k.expires_at)}</span>{/if}
						<span>{k.scopes ? `Only: ${k.scopes.join(', ')}` : 'Everything the account can do'}</span>
						{#if !k.revoked_at}<span>{limitText(k)}</span>{/if}
					</div>
					{#if limiting === k.id}
						<form class="limit" onsubmit={(e) => (e.preventDefault(), saveLimit(k))} aria-label="Rate limit for {k.name}">
							<label class="opt"><input type="radio" bind:group={limitMode} value="setting" /> The setting for keys (Admin → Rate limits)</label>
							<label class="opt">
								<input type="radio" bind:group={limitMode} value="own" /> Its own:
								<input class="num" type="number" min="10" max="1000000" step="1" bind:value={limitValue} disabled={limitMode !== 'own'} aria-label="Requests a minute" />
								a minute
							</label>
							<label class="opt"><input type="radio" bind:group={limitMode} value="none" /> No limit</label>
							<div class="act">
								<button class="btn primary" disabled={busy || !limitOk}>Save</button>
								<button class="btn" type="button" onclick={() => (limiting = '')}>Cancel</button>
							</div>
						</form>
					{/if}
					{#if !k.revoked_at && !disabled}
						<div class="act">
							{#if confirming === k.id}
								<button class="btn reject" type="button" disabled={busy} onclick={() => revoke(k)}>Revoke {k.name}</button>
								<button class="btn" type="button" onclick={() => (confirming = '')}>Keep it</button>
							{:else}
								{#if limiting !== k.id}<button class="btn quiet-btn" type="button" onclick={() => editLimit(k)} aria-label="Rate limit for {k.name}">Rate limit</button>{/if}
								<button class="btn quiet-btn" type="button" onclick={() => (confirming = k.id)} aria-label="Revoke {k.name}">Revoke</button>
							{/if}
						</div>
					{/if}
				</li>
			{/each}
		</ul>
	{/if}
</section>

<style>
	.block {
		display: grid;
		gap: 0.75rem;
		margin-top: 1.5rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	h2 {
		font: 650 1.0625rem/1.3 var(--font-display);
	}
	.head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
	}
	.hint,
	.msg,
	.meta {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	code {
		font-family: var(--font-mono);
		font-size: 0.8125rem;
	}
	.made {
		display: grid;
		gap: 0.6rem;
		padding: 0.75rem;
		border-left: 3px solid var(--marker);
		background: var(--raised);
	}
	.keyrow {
		display: flex;
		gap: 0.5rem;
		align-items: center;
	}
	.key {
		flex: 1;
		min-width: 0;
		padding: 0.5rem 0.6rem;
		border: 1px solid var(--rule);
		border-radius: 8px;
		background: var(--surface);
		overflow-wrap: anywhere;
		user-select: all;
	}
	.example {
		font-size: 0.8125rem;
		color: var(--ink-2);
		overflow-wrap: anywhere;
	}
	.add {
		display: grid;
		gap: 0.6rem;
	}
	.pair {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 0.75rem;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		font-size: 0.9rem;
	}
	.scopes {
		display: grid;
		gap: 0.3rem;
		max-height: 16rem;
		overflow-y: auto;
		margin: 0;
		padding: 0.5rem 0.75rem;
		border: 1px solid var(--rule);
		border-radius: 8px;
	}
	.scopes legend {
		font-weight: 600;
		font-size: 0.85rem;
	}
	.buttons,
	.act {
		display: flex;
		gap: 0.5rem;
	}
	.keys {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.keys li {
		display: grid;
		gap: 0.25rem;
		padding: 0.6rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.keys li:last-child {
		border-bottom: 0;
	}
	.what {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.5rem;
	}
	.name {
		font-weight: 650;
	}
	.prefix {
		color: var(--ink-2);
	}
	.state {
		font-size: 0.8125rem;
		font-weight: 600;
		color: var(--red);
	}
	.off .name {
		color: var(--ink-2);
	}
	.meta {
		display: flex;
		flex-wrap: wrap;
		gap: 0 1rem;
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
	.limit {
		display: grid;
		gap: 0.4rem;
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--rule);
		background: var(--raised);
	}
	.opt {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: 0.45rem;
		font-size: 0.875rem;
	}
	.num {
		width: 7.5rem;
		min-height: 2rem;
		padding: 0.25rem 0.5rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
		color: var(--ink);
		font: inherit;
		font-variant-numeric: tabular-nums;
	}
</style>
