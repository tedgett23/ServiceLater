<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import Signal from '$lib/components/Signal.svelte';
	import Status from '$lib/components/Status.svelte';
	import { clock } from '$lib/clock.svelte';
	import { age, span } from '$lib/format';
	import Avatar from '$lib/components/Avatar.svelte';
	import { queue, queueViews } from '$lib/queue.svelte';
	import { canRecordAny, type Ticket } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let { data, children } = $props();

	const staff = $derived(canRecordAny(data.user, 'read'));
	const filters = $derived(queueViews(staff));
	const viewLabel = $derived(filters.find((f) => f.id === queue.filter)?.label ?? 'Tickets');

	onMount(() => queue.start(data.user));
	$effect(() => clock.watch());

	/** A row's SLA, for open work only: breached in red, otherwise how long until the soonest clock is due. */
	function sla(t: Ticket, now: number): { text: string; tone: string } | null {
		if (t.state_category !== 'open' && t.state_category !== 'waiting') return null;
		if (t.sla_breached) return { text: 'SLA breached', tone: 'bad' };
		if (!t.sla_due_at) return null;
		const left = new Date(t.sla_due_at).getTime() - now;
		if (left <= 60_000) return { text: 'Due now', tone: 'soon' };
		return { text: `Due in ${span(left).split(' ')[0]}`, tone: left < 3_600_000 ? 'soon' : '' };
	}

	const detailOpen = $derived(!!page.params.id);
	// A bare number in the address is an incident's (links from before there were several types).
	const isActive = (t: Ticket) =>
		page.params.id?.toUpperCase() === t.reference ||
		page.params.id === t.id ||
		(t.type === 'incident' && page.params.id === String(t.number));

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
			void goto(`/tickets/${items[next].reference}`, { keepFocus: true, noScroll: true });
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
			<h2>{viewLabel}</h2>
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
			<!-- The views live in the navigation at the left; here only when that is collapsed or on a phone. -->
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
						<a id="row-{t.id}" class="row" class:active={isActive(t)} href="/tickets/{t.reference}" aria-current={isActive(t) ? 'page' : undefined}>
							<Signal priority={t.priority} />
							<span class="title">{t.title}</span>
							<time class="age" datetime={t.updated_at}>{age(t.updated_at)}</time>
							<span class="sub">
								<span class="ref">{t.reference}</span>
								<Status status={t.status} category={t.state_category} type={t.type} />
								{#if sla(t, clock.now)}
									{@const s = sla(t, clock.now)!}
									<span class="sla {s.tone}">{s.text}</span>
								{/if}
								<span class="who"><span class="sr-only">Requested by </span>{t.requester_name}</span>
							</span>
							<span class="owner">
								{#if t.assignment_group_name}
									<span class="group"><span class="sr-only">Group </span>{t.assignment_group_name}</span>
								{/if}
								{#if t.assignee_name}
									<span class="assignee" title="Assigned to {t.assignee_name}">
										<Avatar name={t.assignee_name} size="sm" /><span class="sr-only">Assigned to {t.assignee_name}</span>
									</span>
								{:else if t.state_category === 'open' || t.state_category === 'waiting'}
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
		grid-template-columns: minmax(20rem, 26.5rem) 1fr;
	}
	.queue {
		display: flex;
		flex-direction: column;
		min-height: 0;
		background: var(--ground);
		border-right: 1px solid var(--rule);
	}
	.pane {
		min-width: 0;
		min-height: 0;
		overflow-y: auto;
	}

	.tools {
		display: grid;
		grid-template-columns: auto minmax(0, 1fr);
		align-items: center;
		gap: 0.6rem 0.9rem;
		padding: 0.6rem 1rem;
		min-height: 3.25rem;
		border-bottom: 1px solid var(--rule);
	}
	.tools h2 {
		font-size: 0.9375rem;
		font-weight: 650;
		white-space: nowrap;
	}
	.tools input {
		min-height: 2rem;
		padding-block: 0.25rem;
	}
	.filters {
		grid-column: 1 / -1;
		display: none;
		gap: 0.125rem;
		overflow-x: auto; /* a last resort on the narrowest phones; the labels never wrap */
	}
	:global(.shell.collapsed) .filters {
		display: flex;
	}
	.filters button {
		flex: none;
		white-space: nowrap;
		padding: 0.25rem 0.6rem;
		border: 0;
		border-radius: 7px;
		background: transparent;
		font-size: 0.8125rem;
		font-weight: 550;
		color: var(--ink-2);
		cursor: pointer;
	}
	.filters button:hover {
		background: var(--hover);
	}
	.filters button[aria-pressed='true'] {
		background: color-mix(in srgb, var(--ink) 8%, transparent);
		color: var(--ink);
		font-weight: 600;
	}
	@media (max-width: 40rem) {
		.filters {
			display: flex;
		}
		.tools h2 {
			display: none;
		}
		.tools {
			grid-template-columns: minmax(0, 1fr);
		}
	}
	/* Six views must fit a phone without scrolling. */
	@media (max-width: 26rem) {
		.filters {
			gap: 0;
		}
		.filters button {
			padding: 0.25rem 0.45rem;
		}
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
		border-bottom: 1px solid color-mix(in srgb, var(--rule) 70%, transparent);
	}

	.row {
		display: grid;
		grid-template-columns: 1.1rem minmax(0, 1fr) auto;
		grid-template-areas:
			'sig title age'
			'.   sub   owner';
		align-items: center;
		column-gap: 0.65rem;
		row-gap: 0.2rem;
		padding: 0.65rem 1rem;
		text-decoration: none;
	}
	.row:hover {
		background: var(--hover);
	}
	.row.active {
		background: var(--blue-soft);
		box-shadow: inset 2px 0 0 var(--primary);
	}
	.row :global(.signal) {
		grid-area: sig;
		align-self: center;
	}
	.title {
		grid-area: title;
		font-weight: 550;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.age {
		grid-area: age;
		justify-self: end;
		font-size: 0.75rem;
		color: var(--ink-2);
	}
	.sub {
		grid-area: sub;
		display: flex;
		align-items: center;
		gap: 0.7rem;
		font-size: 0.78rem;
		color: var(--ink-2);
		min-width: 0;
	}
	.ref {
		font: 500 0.72rem var(--font-mono);
		white-space: nowrap;
	}
	.sla {
		white-space: nowrap;
	}
	/* Yellow: due within the hour. Red: missed. */
	.sla.soon {
		padding: 0 0.35rem;
		background: var(--marker-soft);
		color: var(--marker-ink);
		font-weight: 600;
		border-radius: 5px;
	}
	.sla.bad {
		color: var(--red);
		font-weight: 600;
	}
	.who {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		min-width: 0;
	}
	.owner {
		grid-area: owner;
		display: flex;
		align-items: center;
		justify-content: flex-end;
		gap: 0.5rem;
		min-width: 0;
		font-size: 0.78rem;
		color: var(--ink-2);
	}
	/* The team, quieter than the person, beside them on the right. */
	.group {
		max-width: 8rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.assignee {
		display: inline-flex;
		white-space: nowrap;
	}
	/* Highlighter, so unowned work stands out in a scan. */
	.assignee.unassigned {
		padding: 0 0.4rem;
		background: var(--marker-soft);
		color: var(--marker-ink);
		font-size: 0.7rem;
		font-weight: 600;
		border-radius: 5px;
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
		font-size: 0.8125rem;
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
