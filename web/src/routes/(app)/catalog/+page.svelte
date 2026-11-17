<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import CatalogIcon from '$lib/components/CatalogIcon.svelte';
	import Status from '$lib/components/Status.svelte';
	import { age } from '$lib/format';
	import { realtime } from '$lib/realtime.svelte';
	import type { CatalogItem, Ticket } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	/**
	 * The self-service portal: report a problem, or request something from the catalog, and follow what you asked for.
	 * End users land here; staff reach it from the sections bar.
	 */
	let items = $state<CatalogItem[]>([]);
	let loaded = $state(false);
	let error = $state('');
	let search = $state('');
	let mine = $state<Ticket[]>([]);

	const shown = $derived.by(() => {
		const q = search.trim().toLowerCase();
		return q ? items.filter((i) => `${i.label} ${i.summary ?? ''} ${i.category}`.toLowerCase().includes(q)) : items;
	});
	const categories = $derived([...new Set(shown.map((i) => i.category))]);

	async function loadMine() {
		try {
			// Their own records; the tasks under their requests are followed on the request itself.
			const r = await api<{ items: Ticket[] }>(`/api/v1/tickets?${new URLSearchParams({ requester: 'me', open: 'true', limit: '20' })}`);
			mine = r.items.filter((t) => !t.parent_id);
		} catch {
			/* keep what is shown */
		}
	}

	onMount(() => {
		api<{ items: CatalogItem[] }>('/api/v1/catalog')
			.then((r) => (items = r.items))
			.catch((e) => (error = e instanceof Error ? e.message : 'The catalog could not be loaded'))
			.finally(() => (loaded = true));
		void loadMine();
		return realtime.on((e) => {
			if (e.type === 'ticket.changed' || e.type === 'resync') void loadMine();
		});
	});
</script>

<svelte:head><title>Request something – ServiceLater</title></svelte:head>

<div class="portal">
	<header class="intro">
		<h1>How can we help?</h1>
		<div class="ways">
			<button class="btn primary" type="button" onclick={() => (ui.newTicketOpen = true)}>Report a problem</button>
			<label class="search">
				<span class="sr-only">Search the catalog</span>
				<input type="search" bind:value={search} placeholder="Search the catalog: laptop, access, new starter…" />
			</label>
		</div>
	</header>

	<div class="cols">
		<section class="catalog" aria-labelledby="catalog-h">
			<h2 id="catalog-h" class="sr-only">Catalog</h2>
			{#if error}
				<p class="error" role="alert">{error}</p>
			{:else if loaded && items.length === 0}
				<p class="hint">There is nothing to request yet.</p>
			{:else if loaded && shown.length === 0}
				<p class="hint">Nothing in the catalog matches “{search.trim()}”. Report a problem instead, or ask for something else.</p>
			{/if}
			{#each categories as cat (cat)}
				<section class="category" aria-labelledby="cat-{cat}">
					<h3 id="cat-{cat}">{cat}</h3>
					<ul>
						{#each shown.filter((i) => i.category === cat) as it (it.name)}
							<li>
								<a href="/catalog/{it.name}">
									<span class="icon"><CatalogIcon name={it.icon} /></span>
									<span class="what">
										<strong>{it.label}</strong>
										{#if it.summary}<span class="summary">{it.summary}</span>{/if}
									</span>
								</a>
							</li>
						{/each}
					</ul>
				</section>
			{/each}
		</section>

		<aside class="mine" aria-labelledby="mine-h">
			<h2 id="mine-h">What you asked for</h2>
			{#if mine.length === 0}
				<p class="hint">Nothing open. Requests and problems you report show here until they are done.</p>
			{:else}
				<ul>
					{#each mine as t (t.id)}
						<li>
							<a href="/tickets/{t.reference}">
								<span class="ref">{t.reference}</span>
								<span class="title">{t.title}</span>
							</a>
							<span class="meta"><Status status={t.status} category={t.state_category} type={t.type} /> · {age(t.updated_at)}</span>
						</li>
					{/each}
				</ul>
			{/if}
			<p><a href="/tickets">Everything you have asked for</a></p>
		</aside>
	</div>
</div>

<style>
	.portal {
		height: 100%;
		overflow-y: auto;
		padding: 1.5rem 1.25rem 3rem;
	}
	.intro {
		max-width: 64rem;
		margin: 0 auto 1.5rem;
		display: grid;
		gap: 0.9rem;
	}
	h1 {
		font: 650 1.5rem/1.3 var(--font-display);
	}
	.ways {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem;
	}
	.search {
		flex: 1 1 18rem;
	}
	.search input {
		width: 100%;
		min-height: 2.5rem;
		padding: 0.45rem 0.75rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		color: var(--ink);
		font: inherit;
	}
	.cols {
		max-width: 64rem;
		margin: 0 auto;
		display: grid;
		grid-template-columns: minmax(0, 1fr) 20rem;
		gap: 2.5rem;
		align-items: start;
	}
	.category + .category {
		margin-top: 1.5rem;
	}
	h3 {
		font: 650 1.0625rem/1.3 var(--font-display);
		padding-bottom: 0.4rem;
		border-bottom: 1px solid var(--rule);
	}
	.catalog ul,
	.mine ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.catalog li a {
		display: flex;
		align-items: center;
		gap: 0.9rem;
		padding: 0.75rem 0.25rem;
		border-bottom: 1px solid var(--rule);
		color: var(--ink);
		text-decoration: none;
	}
	.catalog li a:hover strong {
		color: var(--blue);
		text-decoration: underline;
	}
	.icon {
		flex: none;
		display: grid;
		place-items: center;
		width: 2.75rem;
		height: 2.75rem;
		border-radius: var(--radius);
		background: var(--blue-soft);
		color: var(--blue);
	}
	.what {
		display: grid;
		gap: 0.1rem;
		min-width: 0;
	}
	.summary,
	.hint,
	.meta {
		color: var(--ink-2);
		font-size: 0.9rem;
	}
	.mine {
		display: grid;
		gap: 0.6rem;
		padding: 1rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
	}
	.mine h2 {
		font: 650 1.0625rem/1.3 var(--font-display);
	}
	.mine li {
		display: grid;
		gap: 0.15rem;
		padding: 0.5rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.mine li:last-child {
		border-bottom: 0;
	}
	.mine li a {
		color: var(--ink);
		overflow-wrap: anywhere;
	}
	.ref {
		font-weight: 700;
		margin-right: 0.3rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
	@media (max-width: 52rem) {
		.cols {
			grid-template-columns: minmax(0, 1fr);
		}
		.mine {
			order: -1;
		}
	}
</style>
