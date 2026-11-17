<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import type { ItemEntry } from '$lib/catalogEditor';
	import CatalogItemEditor from '$lib/components/CatalogItemEditor.svelte';

	let apps = $state<{ name: string; workflows: string[] }[] | null>(null);
	let categories = $state<string[]>([]);
	let error = $state('');

	onMount(async () => {
		try {
			const list = await api<{ items: ItemEntry[]; apps: { name: string; workflows: string[] }[] }>('/api/v1/catalog-items');
			apps = list.apps;
			categories = [...new Set(list.items.map((i) => i.category))];
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the catalog';
		}
	});
</script>

<svelte:head><title>New item – Catalog – ServiceLater</title></svelte:head>

<div class="page">
	<a class="back" href="/admin/catalog">
		<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
		Catalog
	</a>
	<h1>New item</h1>
	{#if error}
		<p class="error" role="alert">{error}</p>
	{:else if apps}
		<CatalogItemEditor def={null} {apps} {categories} onsaved={(s) => goto(`/admin/catalog/${s.item.name}`)} onreset={() => {}} />
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
		margin: 0.75rem 0 0.5rem;
		font: 650 1.375rem/1.3 var(--font-display);
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
</style>
