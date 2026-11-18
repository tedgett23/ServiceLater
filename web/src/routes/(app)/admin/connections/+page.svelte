<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { when } from '$lib/format';
	import { ui } from '$lib/ui.svelte';

	/** What apps' workflows may reach outside ServiceLater: the hosts they may call, and their secrets' values. */
	interface Secret {
		key: string;
		label: string;
		set: boolean;
		updated_at?: string;
		updated_by?: string;
	}
	interface Connection {
		app: string;
		hosts: string[];
		secrets: Secret[];
	}

	let items = $state<Connection[] | null>(null);
	let keyConfigured = $state(true);
	let error = $state('');
	let editing = $state<string | null>(null);
	let value = $state('');
	let busy = $state(false);

	async function load() {
		try {
			const r = await api<{ items: Connection[]; key_configured: boolean }>('/api/v1/connections');
			items = r.items;
			keyConfigured = r.key_configured;
		} catch (e) {
			error = e instanceof Error ? e.message : 'The connections could not be loaded';
		}
	}
	onMount(load);

	async function save(app: string, key: string, v: string | null) {
		busy = true;
		error = '';
		try {
			await api(`/api/v1/connections/${app}/secrets/${key}`, { method: 'PUT', body: { value: v } });
			ui.announce = v === null ? `${key} cleared` : `${key} saved`;
			editing = null;
			value = '';
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : 'That did not save';
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>Connections – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Connections</h1>
	<p class="lede">
		What apps’ workflows may reach outside ServiceLater. An app’s manifest lists the hosts its HTTP steps may call (approved when it
		was installed) and the secrets it needs; their values are set here, kept encrypted and never shown again.
	</p>
	{#if !keyConfigured}<p class="warn">This server has no secrets key, so it cannot store secrets: set SL_SECRETS_KEY and restart.</p>{/if}
	{#if error}<p class="error" role="alert">{error}</p>{/if}
	{#if items && items.length === 0}<p class="hint">No installed app calls out.</p>{/if}
	{#each items ?? [] as c (c.app)}
		<section class="app" aria-labelledby="app-{c.app}">
			<h2 id="app-{c.app}">{c.app}</h2>
			<p class="hint">May call: {c.hosts.length ? c.hosts.join(', ') : 'no hosts'}</p>
			{#if c.secrets.length}
				<ul class="secrets">
					{#each c.secrets as s (s.key)}
						{@const id = `${c.app}/${s.key}`}
						<li>
							<div class="what">
								<strong>{s.label}</strong>
								<code>{s.key}</code>
								<span class="state" class:set={s.set}>{s.set ? `Set${s.updated_by ? ` by ${s.updated_by}` : ''}${s.updated_at ? `, ${when(s.updated_at)}` : ''}` : 'Not set'}</span>
							</div>
							{#if editing === id}
								<form
									class="edit"
									onsubmit={(e) => {
										e.preventDefault();
										if (value) void save(c.app, s.key, value);
									}}
								>
									<input type="password" bind:value aria-label="New value for {s.label}" autocomplete="new-password" placeholder="The new value" />
									<button class="btn primary" disabled={busy || !value}>Save</button>
									<button type="button" class="btn" onclick={() => ((editing = null), (value = ''))}>Cancel</button>
								</form>
							{:else}
								<div class="actions">
									<button type="button" class="btn" onclick={() => ((editing = id), (value = ''))}>{s.set ? 'Replace' : 'Set'}</button>
									{#if s.set}<button type="button" class="btn danger" disabled={busy} onclick={() => save(c.app, s.key, null)}>Clear</button>{/if}
								</div>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}
		</section>
	{/each}
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
		font: 650 1.0625rem/1.3 var(--font-mono);
	}
	.lede {
		margin: 0.25rem 0 0.5rem;
		color: var(--ink-2);
	}
	.app {
		display: grid;
		gap: 0.5rem;
		margin-top: 1.25rem;
		padding-top: 1rem;
		border-top: 1px solid var(--rule);
	}
	.secrets {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 0.5rem;
	}
	.secrets li {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		justify-content: space-between;
		gap: 0.6rem;
		padding: 0.6rem 0.75rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--ground);
	}
	.what {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.5rem;
	}
	code {
		font: 0.8125rem var(--font-mono);
		color: var(--ink-2);
	}
	.state {
		font-size: 0.8125rem;
		color: var(--ink-2);
	}
	.state.set {
		color: var(--green);
		font-weight: 600;
	}
	.actions,
	.edit {
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem;
	}
	.edit input {
		min-width: 12rem;
		padding: 0.35rem 0.6rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		color: var(--ink);
	}
	.danger {
		color: var(--red);
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.warn {
		padding: 0.45rem 0.6rem;
		border-left: 3px solid var(--marker);
		background: var(--marker-soft);
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
</style>
