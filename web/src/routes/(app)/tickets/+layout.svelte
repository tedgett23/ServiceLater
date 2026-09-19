<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import Signal from '$lib/components/Signal.svelte';
	import Status from '$lib/components/Status.svelte';
	import { age } from '$lib/format';
	import { queue, type Filter } from '$lib/queue.svelte';
	import { isStaff } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let { data, children } = $props();

	const staff = $derived(isStaff(data.user));
	const filters = $derived<{ id: Filter; label: string }[]>(
		staff
			? [
					{ id: 'mine', label: 'Mine' },
					{ id: 'unassigned', label: 'Unassigned' },
					{ id: 'open', label: 'Open' },
					{ id: 'all', label: 'All' }
				]
			: [
					{ id: 'open', label: 'Open' },
					{ id: 'all', label: 'All' }
				]
	);

	onMount(() => queue.start(data.user));

	const detailOpen = $derived(!!page.params.id);
	const isActive = (t: { id: string; number: number }) => page.params.id === String(t.number) || page.params.id === t.id;

	let searchBox: HTMLInputElement;
	let timer: ReturnType<typeof setTimeout>;
	function onSearch() {
		clearTimeout(timer);
		timer = setTimeout(() => queue.load(), 200);
	}

	// j/k move through the queue, c creates, / searches. Ignored while typing or in a dialog.
	function onKey(e: KeyboardEvent) {
		if (e.metaKey || e.ctrlKey || e.altKey) return;
		const el = e.target as HTMLElement;
		if (el.closest('input, textarea, select, dialog, [contenteditable]')) return;
		if (e.key === 'c') {
			e.preventDefault();
			ui.newTicketOpen = true;
		} else if (e.key === '/') {
			e.preventDefault();
			searchBox.focus();
		} else if (e.key === 'j' || e.key === 'k') {
			const items = queue.items;
			if (!items.length) return;
			const cur = items.findIndex(isActive);
			const next = e.key === 'j' ? Math.min(items.length - 1, cur + 1) : Math.max(0, cur < 0 ? 0 : cur - 1);
			void goto(`/tickets/${items[next].number}`, { keepFocus: true, noScroll: true });
			document.getElementById(`row-${items[next].id}`)?.scrollIntoView({ block: 'nearest' });
		}
	}

	// Load the next page when the end of the list scrolls into view.
	function whenVisible(node: HTMLElement) {
		const io = new IntersectionObserver((entries) => entries[0].isIntersecting && queue.more(), { rootMargin: '200px' });
		io.observe(node);
		return { destroy: () => io.disconnect() };
	}
</script>

<svelte:window onkeydown={onKey} />
<svelte:head><title>Tickets – ServiceLater</title></svelte:head>

<div class="split" class:detail-open={detailOpen}>
	<section class="queue" aria-label="Ticket queue">
		<div class="tools">
			<input
				bind:this={searchBox}
				bind:value={queue.search}
				oninput={onSearch}
				onkeydown={(e) => e.key === 'Escape' && searchBox.blur()}
				type="search"
				class="select"
				placeholder="Search titles"
				aria-label="Search ticket titles"
			/>
			<div class="filters" role="group" aria-label="Show">
				{#each filters as f (f.id)}
					<button aria-pressed={queue.filter === f.id} onclick={() => queue.setFilter(f.id)}>{f.label}</button>
				{/each}
			</div>
		</div>

		<div class="list">
			{#if queue.error}
				<p class="msg" role="alert">
					{queue.error}. <button class="link" onclick={() => queue.load()}>Try again</button>
				</p>
			{:else if !queue.loading && queue.items.length === 0}
				<div class="msg">
					{#if queue.search}
						<p>No tickets match “{queue.search}”.</p>
					{:else if staff}
						<p>Nothing here. The queue is clear.</p>
					{:else}
						<p>You haven’t filed a ticket yet.</p>
						<button class="btn primary" onclick={() => (ui.newTicketOpen = true)}>Create your first ticket</button>
					{/if}
				</div>
			{/if}

			<ul>
				{#each queue.items as t (t.id)}
					<li>
						<a id="row-{t.id}" class="row" class:active={isActive(t)} href="/tickets/{t.number}" aria-current={isActive(t) ? 'page' : undefined}>
							<Signal priority={t.priority} />
							<span class="num">{t.number}</span>
							<span class="title">{t.title}</span>
							<time class="age" datetime={t.updated_at}>{age(t.updated_at)}</time>
							<span class="sub">
								<Status status={t.status} />
								<span class="who"><span class="sr-only">Requested by </span>{t.requester_name}</span>
								{#if t.assignee_name}
									<span class="assignee"><span class="sr-only">Assigned to </span>{t.assignee_name}</span>
								{:else if t.status !== 'closed' && t.status !== 'resolved'}
									<span class="assignee unassigned">Unassigned</span>
								{/if}
							</span>
						</a>
					</li>
				{/each}
			</ul>

			{#if queue.loading}
				<p class="msg quiet-msg" aria-live="polite">Loading…</p>
			{/if}
			{#if queue.hasMore}
				<div use:whenVisible class="end"></div>
			{/if}
		</div>
	</section>

	<section class="pane" aria-label="Ticket">
		{@render children()}
	</section>
</div>

<style>
	.split {
		height: 100%;
		display: grid;
		grid-template-columns: minmax(20rem, 27rem) 1fr;
	}
	.queue {
		display: flex;
		flex-direction: column;
		min-height: 0;
		background: var(--surface);
		border-right: 1px solid var(--rule);
	}
	.pane {
		min-width: 0;
		min-height: 0;
		overflow-y: auto;
	}

	.tools {
		display: grid;
		gap: 0.6rem;
		padding: 0.75rem;
		border-bottom: 1px solid var(--rule);
	}
	.filters {
		display: flex;
		gap: 0.25rem;
	}
	.filters button {
		padding: 0.25rem 0.7rem;
		border: 1px solid transparent;
		border-radius: var(--radius);
		background: transparent;
		font-size: 0.875rem;
		font-weight: 600;
		color: var(--ink-2);
		cursor: pointer;
	}
	.filters button:hover {
		background: var(--blue-soft);
	}
	.filters button[aria-pressed='true'] {
		background: var(--ink);
		color: var(--surface);
	}

	.list {
		flex: 1;
		min-height: 0;
		overflow-y: auto;
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	li {
		border-bottom: 1px solid var(--rule);
	}

	.row {
		display: grid;
		grid-template-columns: 1.1rem 3.1rem 1fr auto;
		grid-template-areas:
			'sig num title age'
			'.   sub sub   sub';
		align-items: center;
		column-gap: 0.5rem;
		row-gap: 0.15rem;
		padding: 0.6rem 0.75rem 0.6rem 0.7rem;
		border-left: 3px solid transparent;
		text-decoration: none;
	}
	.row:hover {
		background: var(--raised);
	}
	.row.active {
		background: var(--raised);
		border-left-color: var(--blue);
	}
	.row :global(.signal) {
		grid-area: sig;
		align-self: center;
	}
	.num {
		grid-area: num;
		font: 700 1.3rem/1 var(--font-display);
		color: var(--ink-2);
		letter-spacing: 0.02em;
		padding-top: 0.1rem;
	}
	.title {
		grid-area: title;
		font-weight: 600;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.age {
		grid-area: age;
		font-size: 0.8rem;
		color: var(--ink-2);
	}
	.sub {
		grid-area: sub;
		display: flex;
		align-items: center;
		gap: 0.9rem;
		font-size: 0.8125rem;
		color: var(--ink-2);
		min-width: 0;
	}
	.who {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		min-width: 0;
	}
	.assignee {
		margin-left: auto;
		white-space: nowrap;
		color: var(--ink);
	}
	/* Highlighter, so unowned work stands out in a scan. */
	.assignee.unassigned {
		padding: 0 0.3rem;
		background: var(--marker);
		color: var(--on-marker);
		font-weight: 600;
		border-radius: 2px;
	}

	.msg {
		display: grid;
		justify-items: start;
		gap: 0.75rem;
		padding: 1.5rem 1rem;
		color: var(--ink-2);
	}
	.quiet-msg {
		padding: 0.75rem 1rem;
		font-size: 0.85rem;
	}
	.link {
		border: 0;
		background: none;
		padding: 0;
		color: var(--blue);
		font-weight: 600;
		text-decoration: underline;
		cursor: pointer;
	}
	.end {
		height: 1px;
	}

	/* Narrow screens: one pane at a time. */
	@media (max-width: 62rem) {
		.split {
			grid-template-columns: 1fr;
		}
		.split.detail-open .queue,
		.split:not(.detail-open) .pane {
			display: none;
		}
		.queue {
			border-right: 0;
		}
	}
</style>
