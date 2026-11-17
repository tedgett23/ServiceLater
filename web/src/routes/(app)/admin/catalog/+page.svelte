<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import type { ItemEntry } from '$lib/catalogEditor';
	import CatalogIcon from '$lib/components/CatalogIcon.svelte';

	let items = $state<ItemEntry[]>([]);
	let error = $state('');
	let loaded = $state(false);
	const categories = $derived([...new Set(items.map((i) => i.category))]);

	const audience = (i: ItemEntry) =>
		i.available_to ? [...(i.available_to.groups ?? []), ...(i.available_to.roles ?? []).map((r) => `role ${r}`)].join(', ') : 'Everyone';

	onMount(() => {
		api<{ items: ItemEntry[] }>('/api/v1/catalog-items')
			.then((r) => (items = r.items))
			.catch((e) => (error = e instanceof Error ? e.message : 'The catalog could not be loaded'))
			.finally(() => (loaded = true));
	});
</script>

<svelte:head><title>Catalog – ServiceLater</title></svelte:head>

<div class="page">
	<div class="head">
		<div>
			<h1>Catalog</h1>
			<p class="lede">What people can request, and how each is fulfilled. Changes made here are kept apart from the apps that ship the items, so upgrades keep them.</p>
		</div>
		<a class="btn primary" href="/admin/catalog/new">New item</a>
	</div>
	{#if error}<p class="error" role="alert">{error}</p>{/if}
	{#if loaded && items.length === 0}<p class="lede">The catalog is empty.</p>{/if}
	{#each categories as cat (cat)}
		<section aria-labelledby="c-{cat}">
			<h2 id="c-{cat}">{cat}</h2>
			<ul>
				{#each items.filter((i) => i.category === cat) as it (it.name)}
					<li>
						<span class="icon"><CatalogIcon name={it.icon} /></span>
						<a href="/admin/catalog/{it.name}"><strong>{it.label}</strong></a>
						<span class="who">{audience(it)}</span>
						<span class="from">
							{it.app}{#if it.edited}<span class="mark">{it.shipped ? 'changed here' : 'added here'}</span>{/if}
						</span>
					</li>
				{/each}
			</ul>
		</section>
	{/each}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 56rem;
	}
	.head {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: 1rem;
		margin-bottom: 1rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	.head .btn {
		flex: none;
		white-space: nowrap;
	}
	h2 {
		font: 650 1.0625rem/1.3 var(--font-display);
		padding-bottom: 0.35rem;
		border-bottom: 1px solid var(--rule);
	}
	section + section {
		margin-top: 1.25rem;
	}
	.lede,
	.who,
	.from {
		color: var(--ink-2);
		font-size: 0.9rem;
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	li {
		display: grid;
		grid-template-columns: auto minmax(0, 1fr) auto auto;
		align-items: center;
		gap: 0.9rem;
		padding: 0.55rem 0;
		border-bottom: 1px solid var(--rule);
	}
	li a {
		color: var(--ink);
		overflow-wrap: anywhere;
	}
	.icon {
		display: grid;
		place-items: center;
		width: 2.3rem;
		height: 2.3rem;
		border-radius: var(--radius);
		background: var(--blue-soft);
		color: var(--blue);
	}
	.mark {
		margin-left: 0.5rem;
		padding: 0.05rem 0.4rem;
		border-radius: var(--radius);
		background: var(--marker);
		color: #1a1a1a;
		font-size: 0.8rem;
		font-weight: 600;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
	@media (max-width: 40rem) {
		.head {
			flex-direction: column;
		}
		li {
			grid-template-columns: auto minmax(0, 1fr);
		}
		.who,
		.from {
			grid-column: 2;
		}
	}
</style>
