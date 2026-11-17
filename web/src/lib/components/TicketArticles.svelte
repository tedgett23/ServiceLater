<script lang="ts">
	import { api } from '$lib/api';
	import { session } from '$lib/session.svelte';
	import { can, P, type ArticleHit, type ArticleRef, type Ticket } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	/**
	 * The knowledge articles linked to a record: the requester reads the ones meant for them, staff link and unlink
	 * them, and someone who writes articles can start one from a resolved record.
	 */
	let {
		ticket,
		canEdit,
		onchange
	}: {
		ticket: Ticket;
		canEdit: boolean;
		onchange: (articles: ArticleRef[]) => void;
	} = $props();

	const items = $derived(ticket.articles ?? []);
	const writer = $derived(can(session.user, P.knowledgeWrite) || can(session.user, P.knowledgePublish));
	const settled = $derived(ticket.state_category === 'done' || ticket.state_category === 'closed');

	let search = $state('');
	let found = $state<ArticleHit[]>([]);
	let looked = $state(false);
	let busy = $state(false);
	let error = $state('');
	let timer: ReturnType<typeof setTimeout>;
	const shown = $derived(found.filter((a) => !items.some((x) => x.id === a.id)));

	function onSearch() {
		clearTimeout(timer);
		const q = search.trim();
		if (!q) {
			found = [];
			looked = false;
			return;
		}
		timer = setTimeout(async () => {
			try {
				found = (await api<{ items: ArticleHit[] }>(`/api/v1/kb/articles?${new URLSearchParams({ q, limit: '6' })}`)).items;
			} catch {
				found = [];
			}
			looked = true;
		}, 200);
	}

	async function change(method: 'PUT' | 'DELETE', id: string, done: string) {
		busy = true;
		error = '';
		try {
			const r = await api<{ items: ArticleRef[] }>(`/api/v1/tickets/${ticket.id}/articles/${id}`, { method });
			onchange(r.items);
			ui.announce = done;
			search = '';
			found = [];
			looked = false;
		} catch (e) {
			error = e instanceof Error ? e.message : 'That change didn’t save';
		} finally {
			busy = false;
		}
	}
</script>

{#if canEdit || items.length}
	<section class="articles" aria-labelledby="articles-h">
		<h2 id="articles-h">Articles</h2>
		{#if error}<p class="error" role="alert">{error}</p>{/if}
		{#if items.length === 0}
			<p class="none">No knowledge articles are linked yet.</p>
		{:else}
			<ul class="rows">
				{#each items as a (a.id)}
					<li>
						<a href="/kb/{a.reference}"><strong>{a.reference}</strong> {a.title}</a>
						{#if canEdit}
							<button class="btn quiet-btn" disabled={busy} onclick={() => change('DELETE', a.id, `${a.reference} unlinked`)} aria-label="Unlink {a.reference}">
								Unlink
							</button>
						{/if}
					</li>
				{/each}
			</ul>
		{/if}
		{#if canEdit}
			<div class="add">
				<div class="field">
					<label for="article-search">Link an article</label>
					<input
						id="article-search"
						type="search"
						bind:value={search}
						oninput={onSearch}
						onkeydown={(e) => e.key === 'Enter' && e.preventDefault()}
						placeholder="Search the knowledge base"
						autocomplete="off"
					/>
				</div>
				{#if looked && shown.length === 0}<p class="none">Nothing matches.</p>{/if}
				{#if shown.length}
					<ul class="rows">
						{#each shown as a (a.id)}
							<li>
								<span><strong>{a.reference}</strong> {a.title}</span>
								<button class="btn" disabled={busy} onclick={() => change('PUT', a.id, `${a.reference} linked`)} aria-label="Link {a.reference}">Link</button>
							</li>
						{/each}
					</ul>
				{/if}
			</div>
		{/if}
		{#if writer && canEdit && settled}
			<p><a class="btn" href="/kb/new?from={ticket.reference}">Write an article from this ticket</a></p>
		{/if}
	</section>
{/if}

<style>
	.articles {
		display: grid;
		gap: 0.6rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	h2 {
		font: 650 1.0625rem/1.3 var(--font-display);
		letter-spacing: -0.015em;
	}
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.rows li {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
		padding: 0.45rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.rows li:last-child {
		border-bottom: 0;
	}
	.rows a,
	.rows span {
		color: var(--ink);
		min-width: 0;
		overflow-wrap: anywhere;
	}
	.add {
		max-width: 28rem;
	}
	.none {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.quiet-btn {
		min-height: 2rem;
		font-weight: 500;
	}
	.error {
		color: var(--red);
		font-size: 0.9rem;
	}
</style>
