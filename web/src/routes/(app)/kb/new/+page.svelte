<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import ArticleEditor from '$lib/components/ArticleEditor.svelte';
	import type { Article, Ticket } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	// Written from a ticket (?from=INC42): it starts with the ticket's problem and how it was fixed, and is linked to it.
	const from = page.url.searchParams.get('from');
	let initial = $state<{ title?: string; body?: string } | null>(from ? null : {});

	onMount(async () => {
		if (!from) return;
		try {
			const t = await api<Ticket>(`/api/v1/tickets/${encodeURIComponent(from)}`);
			const parts = [`## Problem\n\n${t.description || t.title}`];
			if (t.resolution) parts.push(`## Fix\n\n${t.resolution}`);
			initial = { title: t.title, body: parts.join('\n\n') + '\n' };
		} catch {
			initial = {};
		}
	});

	async function saved(a: Article) {
		if (from) {
			try {
				await api(`/api/v1/tickets/${encodeURIComponent(from)}/articles/${a.id}`, { method: 'PUT' });
			} catch {
				/* the article is saved; linking it can be done from the ticket */
			}
		}
		ui.announce = `${a.reference} saved as a draft`;
		await goto(`/kb/${a.reference}`);
	}
</script>

<svelte:head><title>New article – ServiceLater</title></svelte:head>

<div class="page">
	<a class="back" href="/kb">
		<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
		Knowledge
	</a>
	<h1>New article</h1>
	{#if from}<p class="lede">From {from}: it starts with the ticket's problem and fix, and is linked to it when saved.</p>{/if}
	{#if initial}
		<ArticleEditor {initial} onsaved={saved} oncancel={() => goto(from ? `/tickets/${from}` : '/kb')} />
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
</style>
