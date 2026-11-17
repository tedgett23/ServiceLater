<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import type { ItemDefResponse, ItemEntry } from '$lib/catalogEditor';
	import CatalogItemEditor from '$lib/components/CatalogItemEditor.svelte';

	let def = $state<ItemDefResponse | null>(null);
	let categories = $state<string[]>([]);
	let error = $state('');
	let version = $state(0);

	onMount(async () => {
		try {
			const [d, list] = await Promise.all([
				api<ItemDefResponse>(`/api/v1/catalog-items/${page.params.name}`),
				api<{ items: ItemEntry[] }>('/api/v1/catalog-items')
			]);
			def = d;
			categories = [...new Set(list.items.map((i) => i.category))];
		} catch (e) {
			error = e instanceof ApiError && e.status === 404 ? 'There is no such item.' : 'The item could not be loaded.';
		}
	});
</script>

<svelte:head><title>{def ? def.item.label : 'Item'} – Catalog – ServiceLater</title></svelte:head>

<div class="page">
	<a class="back" href="/admin/catalog">
		<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
		Catalog
	</a>
	{#if error}
		<p class="error" role="alert">{error}</p>
	{:else if def}
		<h1>{def.item.label}</h1>
		<p class="lede"><a href="/catalog/{def.item.name}">See it in the catalog</a></p>
		<!-- Keyed, so a reset starts the editor again from what the server has. -->
		{#key version}
			<CatalogItemEditor
				{def}
				apps={[]}
				{categories}
				onsaved={(s) => {
					def = s;
					version++;
				}}
				onreset={(after) => {
					if (after) {
						def = after;
						version++;
					} else void goto('/admin/catalog');
				}}
			/>
		{/key}
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 3rem;
		max-width: 80rem;
	}
	.back {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		color: var(--ink-2);
		font-size: 0.9rem;
	}
	h1 {
		margin-top: 0.75rem;
		font: 650 1.375rem/1.3 var(--font-display);
	}
	.lede {
		margin-bottom: 0.5rem;
		font-size: 0.9rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
</style>
