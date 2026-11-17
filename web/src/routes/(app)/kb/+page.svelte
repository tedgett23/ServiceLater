<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import Snippet from '$lib/components/Snippet.svelte';
	import { when } from '$lib/format';
	import { can, P, type ArticleHit } from '$lib/types';

	let { data } = $props();
	const writer = $derived(can(data.user, P.knowledgeWrite) || can(data.user, P.knowledgePublish));

	// The search, category and status live in the address, so a search can be shared and Back works.
	const q = $derived(page.url.searchParams.get('q') ?? '');
	const category = $derived(page.url.searchParams.get('category') ?? '');
	const status = $derived(page.url.searchParams.get('status') ?? 'published');
	let typed = $state('');
	let items = $state<ArticleHit[]>([]);
	let more = $state(false);
	let loading = $state(true);
	let error = $state('');
	let categories = $state<{ name: string; articles: number }[]>([]);

	const views = [
		{ value: 'published', label: 'Published' },
		{ value: 'draft', label: 'Drafts' },
		{ value: 'review', label: 'In review' },
		{ value: 'retired', label: 'Retired' }
	];

	function go(change: Record<string, string>) {
		const next = new URLSearchParams(page.url.searchParams);
		for (const [k, v] of Object.entries(change)) {
			if (v) next.set(k, v);
			else next.delete(k);
		}
		void goto(`/kb${next.size ? `?${next}` : ''}`, { keepFocus: true, noScroll: true, replaceState: true });
	}

	async function load(append = false) {
		const params = new URLSearchParams({ limit: '25', offset: String(append ? items.length : 0) });
		if (q) params.set('q', q);
		if (category) params.set('category', category);
		if (status !== 'published') params.set('status', status);
		try {
			const r = await api<{ items: ArticleHit[]; more: boolean }>(`/api/v1/kb/articles?${params}`);
			items = append ? [...items, ...r.items] : r.items;
			more = r.more;
			error = '';
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load articles';
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void [q, category, status];
		typed = q;
		void load();
	});

	onMount(() => {
		void api<{ items: { name: string; articles: number }[] }>('/api/v1/kb/categories')
			.then((r) => (categories = r.items))
			.catch(() => {});
	});

	function search(e: Event) {
		e.preventDefault();
		go({ q: typed.trim() });
	}
</script>

<svelte:head><title>Knowledge – ServiceLater</title></svelte:head>

<div class="page">
	<div class="top">
		<div>
			<h1>Knowledge</h1>
			<p class="lede">Answers to common questions and how to fix common problems.</p>
		</div>
		{#if writer}<a class="btn primary" href="/kb/new">New article</a>{/if}
	</div>

	<form class="search" role="search" onsubmit={search}>
		<label class="sr-only" for="kb-q">Search articles</label>
		<input id="kb-q" type="search" bind:value={typed} placeholder="Search articles" />
		<button class="btn">Search</button>
	</form>

	{#if writer}
		<div class="views" role="tablist" aria-label="Which articles">
			{#each views as v (v.value)}
				<button role="tab" aria-selected={status === v.value} class:on={status === v.value} onclick={() => go({ status: v.value === 'published' ? '' : v.value })}>
					{v.label}
				</button>
			{/each}
		</div>
	{/if}

	{#if categories.length && status === 'published'}
		<div class="cats" aria-label="Categories">
			<button class="chip" class:on={!category} onclick={() => go({ category: '' })}>All</button>
			{#each categories as c (c.name)}
				<button class="chip" class:on={category === c.name} onclick={() => go({ category: category === c.name ? '' : c.name })}>
					{c.name} <span class="count">{c.articles}</span>
				</button>
			{/each}
		</div>
	{/if}

	{#if error}
		<p class="error" role="alert">{error}</p>
	{:else if !loading && items.length === 0}
		<p class="msg">{q ? `No articles match “${q}”.` : 'No articles here yet.'}</p>
	{/if}

	<ul class="hits" aria-label="Articles">
		{#each items as a (a.id)}
			<li>
				<a href="/kb/{a.reference}">
					<span class="title">{a.title}</span>
					<span class="meta">
						{a.reference}{a.category ? ` · ${a.category}` : ''}{a.audience === 'internal' ? ' · Internal' : ''} · updated {when(a.updated_at)}
					</span>
					{#if a.snippet}<span class="snip"><Snippet text={a.snippet} /></span>{/if}
				</a>
			</li>
		{/each}
	</ul>
	{#if more}<button class="btn" onclick={() => load(true)}>Show more</button>{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 52rem;
	}
	.top {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: 1rem;
	}
	.top .btn {
		flex: none;
		white-space: nowrap;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	.search {
		display: flex;
		gap: 0.5rem;
		margin-bottom: 0.75rem;
	}
	.search input {
		flex: 1;
		min-width: 0;
		min-height: 2.5rem;
		padding: 0.5rem 0.65rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
	}
	.views {
		display: flex;
		gap: 0.25rem;
		margin-bottom: 0.75rem;
		overflow-x: auto;
	}
	.views button {
		padding: 0.35rem 0.75rem;
		border: 0;
		border-radius: var(--radius);
		background: none;
		color: var(--ink-2);
		font-weight: 600;
		white-space: nowrap;
		cursor: pointer;
	}
	.views button.on {
		background: var(--raised);
		color: var(--ink);
		box-shadow: inset 0 -2px 0 var(--blue);
	}
	.cats {
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem;
		margin-bottom: 0.75rem;
	}
	.chip {
		padding: 0.25rem 0.65rem;
		border: 1px solid var(--rule);
		border-radius: 999px;
		background: var(--surface);
		color: var(--ink);
		font-size: 0.875rem;
		cursor: pointer;
	}
	.chip.on {
		border-color: var(--blue);
		background: var(--blue-soft);
	}
	.count {
		color: var(--ink-2);
	}
	.hits {
		list-style: none;
		margin: 0 0 1rem;
		padding: 0;
	}
	.hits li {
		border-bottom: 1px solid var(--rule);
	}
	.hits a {
		display: grid;
		gap: 0.2rem;
		padding: 0.75rem 0.25rem;
		color: var(--ink);
		text-decoration: none;
	}
	.hits a:hover .title {
		text-decoration: underline;
	}
	.title {
		font-weight: 700;
		overflow-wrap: anywhere;
	}
	.meta {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.snip {
		color: var(--ink-2);
		font-size: 0.9rem;
		overflow-wrap: anywhere;
	}
	.msg {
		padding: 0.5rem 0;
		color: var(--ink-2);
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
</style>
