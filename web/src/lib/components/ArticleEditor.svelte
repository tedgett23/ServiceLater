<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { renderMarkdown } from '$lib/markdown';
	import type { Article } from '$lib/types';

	/**
	 * Writes an article: a new one (no `article`), or changes to one. The body is Markdown, with a preview rendered the
	 * way readers will see it. Saving answers with the saved article.
	 */
	let {
		article = null,
		initial = {},
		onsaved,
		oncancel
	}: {
		article?: Article | null;
		/** What a new article starts with (from a ticket, say). */
		initial?: { title?: string; body?: string };
		onsaved: (a: Article) => void;
		oncancel: () => void;
	} = $props();

	function start() {
		return {
			title: article?.title ?? initial.title ?? '',
			body: article?.body ?? initial.body ?? '',
			category: article?.category ?? '',
			tags: (article?.tags ?? []).join(', '),
			audience: article?.audience ?? 'everyone'
		};
	}
	const draft = $state(start());
	let preview = $state(false);
	let saving = $state(false);
	let error = $state('');
	let categories = $state<string[]>([]);

	onMount(() => {
		void api<{ items: { name: string }[] }>('/api/v1/kb/categories')
			.then((r) => (categories = r.items.map((c) => c.name)))
			.catch(() => {});
	});

	async function save(e: Event) {
		e.preventDefault();
		if (!draft.title.trim() || saving) return;
		saving = true;
		error = '';
		const body = {
			title: draft.title,
			body: draft.body,
			category: draft.category,
			tags: draft.tags.split(',').map((t) => t.trim()).filter(Boolean),
			audience: draft.audience
		};
		try {
			const saved = article
				? await api<Article>(`/api/v1/kb/articles/${article.id}`, { method: 'PATCH', body })
				: await api<Article>('/api/v1/kb/articles', { method: 'POST', body });
			onsaved(saved);
		} catch (err) {
			error = err instanceof Error ? err.message : 'The article didn’t save';
		} finally {
			saving = false;
		}
	}
</script>

<form class="editor" onsubmit={save} aria-label={article ? `Edit ${article.reference}` : 'New article'}>
	<label class="field wide">
		<span>Title</span>
		<input bind:value={draft.title} maxlength="200" required />
	</label>
	<label class="field">
		<span>Category</span>
		<input bind:value={draft.category} maxlength="100" list="kb-categories" placeholder="Printing" />
		<datalist id="kb-categories">
			{#each categories as c (c)}<option value={c}></option>{/each}
		</datalist>
	</label>
	<label class="field">
		<span>Tags</span>
		<input bind:value={draft.tags} placeholder="printer, jam" />
	</label>
	<label class="field">
		<span>Who can read it</span>
		<select bind:value={draft.audience}>
			<option value="everyone">Everyone</option>
			<option value="internal">Staff only</option>
		</select>
	</label>

	<div class="body wide">
		<div class="tabs" role="tablist" aria-label="Article text">
			<button type="button" role="tab" aria-selected={!preview} class:on={!preview} onclick={() => (preview = false)}>Write</button>
			<button type="button" role="tab" aria-selected={preview} class:on={preview} onclick={() => (preview = true)}>Preview</button>
		</div>
		{#if preview}
			<!-- eslint-disable-next-line svelte/no-at-html-tags -- sanitised by renderMarkdown -->
			<div class="prose preview">{@html renderMarkdown(draft.body || '*Nothing written yet.*')}</div>
		{:else}
			<label class="sr-only" for="kb-body">Article text (Markdown)</label>
			<textarea id="kb-body" bind:value={draft.body} maxlength="100000" placeholder="Write in Markdown: **bold**, lists, `code`, [links](https://example.com)"></textarea>
		{/if}
	</div>

	{#if error}<p class="error wide" role="alert">{error}</p>{/if}
	<div class="actions wide">
		<button class="btn primary" disabled={saving || !draft.title.trim()}>{article ? 'Save changes' : 'Save draft'}</button>
		<button type="button" class="btn" onclick={oncancel}>Cancel</button>
	</div>
</form>

<style>
	.editor {
		display: grid;
		grid-template-columns: repeat(3, minmax(0, 1fr));
		gap: 0.9rem 1rem;
	}
	.wide {
		grid-column: 1 / -1;
	}
	.tabs {
		display: flex;
		gap: 0.25rem;
		margin-bottom: 0.4rem;
	}
	.tabs button {
		padding: 0.3rem 0.75rem;
		border: 0;
		border-radius: var(--radius);
		background: none;
		color: var(--ink-2);
		font-weight: 600;
		cursor: pointer;
	}
	.tabs button.on {
		background: var(--raised);
		color: var(--ink);
		box-shadow: inset 0 -2px 0 var(--blue);
	}
	textarea {
		width: 100%;
		min-height: 22rem;
		padding: 0.65rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		font: 0.95rem/1.5 ui-monospace, SFMono-Regular, Menlo, monospace;
		resize: vertical;
	}
	.preview {
		min-height: 22rem;
		padding: 0.65rem 0.9rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
	}
	.actions {
		display: flex;
		gap: 0.5rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
	@media (max-width: 40rem) {
		.editor {
			grid-template-columns: minmax(0, 1fr);
		}
	}
</style>
