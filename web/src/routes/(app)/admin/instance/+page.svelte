<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { environmentLabel, instance, type Environment } from '$lib/instance.svelte';
	import { ui } from '$lib/ui.svelte';

	let name = $state('');
	let environment = $state<Environment>('production');
	let saved = $state<{ name: string; environment: Environment } | null>(null);
	let saving = $state(false);
	let error = $state('');

	onMount(async () => {
		try {
			saved = await api<{ name: string; environment: Environment }>('/api/v1/settings/instance');
			name = saved.name;
			environment = saved.environment;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the instance settings';
		}
	});

	const dirty = $derived(!!saved && (name.trim() !== saved.name || environment !== saved.environment));

	async function save(e: Event) {
		e.preventDefault();
		saving = true;
		error = '';
		try {
			saved = await api<{ name: string; environment: Environment }>('/api/v1/settings/instance', { method: 'PUT', body: { name: name.trim(), environment } });
			name = saved.name;
			await instance.load();
			ui.announce = 'Instance saved';
		} catch (err) {
			error = err instanceof Error ? err.message : 'The instance settings didn’t save';
		} finally {
			saving = false;
		}
	}
</script>

<svelte:head><title>Instance – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Instance</h1>
	<p class="lede">What this instance is called and which environment it is. Anything but production shows a banner on every page, so nobody mistakes it for the real one.</p>
	{#if error}<p class="error" role="alert">{error}</p>{/if}
	{#if saved}
		<form onsubmit={save}>
			<div class="field">
				<label for="in-name">Name</label>
				<input id="in-name" bind:value={name} maxlength="60" placeholder="Acme production" />
				<small class="hint">Shown in the banner, in snapshot file names, and typed to confirm a restore.</small>
			</div>
			<div class="field">
				<label for="in-env">Environment</label>
				<select id="in-env" bind:value={environment}>
					{#each Object.entries(environmentLabel) as [value, label] (value)}<option {value}>{label}</option>{/each}
				</select>
			</div>
			<div><button class="btn primary" disabled={!dirty || saving}>{saving ? 'Saving…' : 'Save'}</button></div>
		</form>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 36rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	form {
		display: grid;
		gap: 0.9rem;
		padding-top: 1rem;
		border-top: 1px solid var(--rule);
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
</style>
