<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import Status from '$lib/components/Status.svelte';
	import { when } from '$lib/format';
	import { renderMarkdown } from '$lib/markdown';
	import { can, P, type Article, type ArticleStatus, type ArticleVersion } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let { data } = $props();
	const writer = $derived(can(data.user, P.knowledgeWrite));
	const publisher = $derived(can(data.user, P.knowledgePublish));

	let article = $state<Article | null>(null);
	let versions = $state<ArticleVersion[]>([]);
	/** An older version being read (?version=n). */
	let old = $state<ArticleVersion | null>(null);
	let loadError = $state('');
	let error = $state('');
	let busy = $state(false);
	let confirmingDelete = $state(false);

	const ref = $derived(page.params.ref);
	const versionParam = $derived(Number(page.url.searchParams.get('version')) || 0);
	const canEdit = $derived(!!article && (publisher || (writer && (article.status === 'draft' || article.status === 'review'))));

	async function load() {
		try {
			article = await api<Article>(`/api/v1/kb/articles/${ref}`);
			loadError = '';
		} catch (e) {
			loadError = e instanceof ApiError && e.status === 404 ? 'That article does not exist, or is not for you.' : 'Could not load the article.';
			return;
		}
		if (writer || publisher) {
			try {
				versions = (await api<{ items: ArticleVersion[] }>(`/api/v1/kb/articles/${ref}/versions`)).items;
			} catch {
				versions = [];
			}
		}
	}

	$effect(() => {
		void ref;
		void load();
	});
	$effect(() => {
		const n = versionParam;
		old = null;
		if (!n) return;
		void api<ArticleVersion>(`/api/v1/kb/articles/${ref}/versions/${n}`)
			.then((v) => (old = v))
			.catch(() => (old = null));
	});

	/** What a move is called, from where the article is. */
	function verb(to: ArticleStatus): string {
		switch (to) {
			case 'review':
				return 'Send for review';
			case 'published':
				return article?.status === 'retired' ? 'Publish again' : 'Publish';
			case 'retired':
				return 'Retire';
			case 'draft':
				return article?.status === 'review' ? 'Send back to draft' : 'Back to draft';
		}
	}
	const statusLabel: Record<ArticleStatus, string> = { draft: 'Draft', review: 'In review', published: 'Published', retired: 'Retired' };

	async function move(to: ArticleStatus) {
		if (!article) return;
		busy = true;
		error = '';
		try {
			article = await api<Article>(`/api/v1/kb/articles/${article.id}/status`, { method: 'POST', body: { to } });
			ui.announce = `${article.reference} is ${statusLabel[article.status].toLowerCase()}`;
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : 'That didn’t work';
		} finally {
			busy = false;
		}
	}

	async function destroy() {
		if (!article) return;
		busy = true;
		error = '';
		try {
			await api(`/api/v1/kb/articles/${article.id}`, { method: 'DELETE' });
			ui.announce = `${article.reference} deleted`;
			await goto('/kb');
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not delete it';
			confirmingDelete = false;
		} finally {
			busy = false;
		}
	}

	const shown = $derived(old ? { title: old.title, body: old.body ?? '' } : article);
</script>

<svelte:head><title>{article ? `${article.reference} ${article.title}` : 'Article'} – ServiceLater</title></svelte:head>

<div class="page">
	<a class="back" href="/kb">
		<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
		Knowledge
	</a>

	{#if loadError}
		<p class="msg" role="alert">{loadError}</p>
	{:else if article && shown}
		<article aria-labelledby="article-title">
			<p class="ref">
				{article.reference}{#if article.category}<span> · {article.category}</span>{/if}
				{#if article.audience === 'internal'}<span class="badge">Staff only</span>{/if}
				{#if article.status !== 'published'}<span class="badge status">{statusLabel[article.status]}</span>{/if}
			</p>
			<h1 id="article-title">{shown.title}</h1>
			<p class="byline">
				{#if article.updated_by_name}Updated by {article.updated_by_name}{:else}Updated{/if}
				<time datetime={article.updated_at}>{when(article.updated_at)}</time> · version {article.version}
			</p>

			{#if old}
				<p class="old" role="note">
					You are reading version {old.version}, saved {old.saved_by_name ? `by ${old.saved_by_name} ` : ''}{when(old.saved_at)}.
					<a href="/kb/{article.reference}">Read the current version</a>
				</p>
			{/if}

			{#if error}<p class="error" role="alert">{error}</p>{/if}
			{#if !old && (canEdit || article.moves?.length)}
				<div class="actions">
					{#if canEdit}<a class="btn" href="/kb/{article.reference}/edit">Edit</a>{/if}
					{#each article.moves ?? [] as to (to)}
						<button class="btn" class:primary={to === 'published' || to === 'review'} disabled={busy} onclick={() => move(to)}>{verb(to)}</button>
					{/each}
					{#if publisher && !article.published_at}
						{#if confirmingDelete}
							<button class="btn reject" disabled={busy} onclick={destroy}>Delete {article.reference}</button>
							<button class="btn" onclick={() => (confirmingDelete = false)}>Keep it</button>
						{:else}
							<button class="btn reject" onclick={() => (confirmingDelete = true)}>Delete</button>
						{/if}
					{/if}
				</div>
			{/if}

			<!-- eslint-disable-next-line svelte/no-at-html-tags -- sanitised by renderMarkdown -->
			<div class="prose body">{@html renderMarkdown(shown.body || '*This article has no text yet.*')}</div>

			{#if article.tags.length && !old}
				<ul class="tags" aria-label="Tags">
					{#each article.tags as t (t)}<li><a href="/kb?q={encodeURIComponent(t)}">{t}</a></li>{/each}
				</ul>
			{/if}
		</article>

		{#if article.tickets?.length}
			<section aria-labelledby="tickets-h">
				<h2 id="tickets-h">Linked tickets</h2>
				<ul class="rows">
					{#each article.tickets as t (t.id)}
						<li>
							<a href="/tickets/{t.reference}"><strong>{t.reference}</strong> {t.title}</a>
							<Status status={t.status} category={t.state_category} type={t.type} />
						</li>
					{/each}
				</ul>
			</section>
		{/if}

		{#if versions.length > 1}
			<section aria-labelledby="history-h">
				<h2 id="history-h">History</h2>
				<ol class="rows">
					{#each versions as v (v.version)}
						<li>
							<a href="/kb/{article.reference}{v.version === article.version ? '' : `?version=${v.version}`}" aria-current={(old ? old.version : article.version) === v.version ? 'page' : undefined}>
								Version {v.version}
							</a>
							<span class="hint">{v.saved_by_name ?? 'Someone'} · {when(v.saved_at)}</span>
						</li>
					{/each}
				</ol>
			</section>
		{/if}
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 48rem;
	}
	.back {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		margin-bottom: 0.75rem;
		font-weight: 600;
		text-decoration: none;
	}
	.ref {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.4rem;
		color: var(--ink-2);
		font-size: 0.9rem;
		font-weight: 600;
	}
	.badge {
		padding: 0.05rem 0.45rem;
		border-radius: var(--radius);
		background: var(--raised);
		color: var(--ink);
		font-size: 0.8rem;
	}
	.badge.status {
		background: var(--marker);
		color: var(--on-marker);
	}
	h1 {
		margin-top: 0.25rem;
		font: 800 2rem/1.15 var(--font-display);
		overflow-wrap: anywhere;
	}
	h2 {
		font: 700 1.25rem/1.2 var(--font-display);
		margin-bottom: 0.5rem;
	}
	.byline,
	.hint {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.byline {
		margin-top: 0.35rem;
	}
	.old {
		margin-top: 1rem;
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--marker);
		background: var(--raised);
	}
	.actions {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
		margin-top: 1rem;
	}
	.btn.reject {
		color: var(--red);
	}
	.body {
		margin-top: 1.25rem;
	}
	.tags {
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem;
		list-style: none;
		margin: 1.25rem 0 0;
		padding: 0;
	}
	.tags a {
		display: inline-block;
		padding: 0.15rem 0.6rem;
		border: 1px solid var(--rule);
		border-radius: 999px;
		color: var(--ink);
		font-size: 0.85rem;
		text-decoration: none;
	}
	section {
		margin-top: 1.5rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.rows li {
		display: flex;
		align-items: baseline;
		justify-content: space-between;
		gap: 0.75rem;
		padding: 0.45rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.rows li:last-child {
		border-bottom: 0;
	}
	.rows a {
		color: var(--ink);
		min-width: 0;
		overflow-wrap: anywhere;
	}
	.rows a[aria-current='page'] {
		font-weight: 700;
	}
	.msg {
		color: var(--ink-2);
	}
	.error {
		margin-top: 0.75rem;
		color: var(--red);
		font-weight: 600;
	}
</style>
