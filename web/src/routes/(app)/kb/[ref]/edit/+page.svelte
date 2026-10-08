<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import ArticleEditor from '$lib/components/ArticleEditor.svelte';
	import type { Article } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let article = $state<Article | null>(null);
	let error = $state('');

	onMount(async () => {
		try {
			article = await api<Article>(`/api/v1/kb/articles/${page.params.ref}`);
		} catch (e) {
			error = e instanceof ApiError && e.status === 404 ? 'That article does not exist.' : 'Could not load the article.';
		}
	});

	async function saved(a: Article) {
		ui.announce = `${a.reference} saved`;
		await goto(`/kb/${a.reference}`);
	}
</script>

<svelte:head><title>Edit {article?.reference ?? 'article'} – ServiceLater</title></svelte:head>

<div class="page">
	<a class="back" href="/kb/{page.params.ref}">
		<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
		{page.params.ref}
	</a>
	<h1>Edit {article?.reference ?? ''}</h1>
	{#if error}<p class="error" role="alert">{error}</p>{/if}
	{#if article}
		{#if article.status === 'published'}<p class="lede">It stays published: readers see your changes when you save. The previous version is kept.</p>{/if}
		<ArticleEditor {article} onsaved={saved} oncancel={() => goto(`/kb/${article!.reference}`)} />
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 52rem;
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
		font: 800 1.6rem/1.2 var(--font-display);
		margin-bottom: 0.75rem;
	}
	.lede {
		margin: -0.25rem 0 1rem;
		color: var(--ink-2);
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
</style>
