<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import Status from '$lib/components/Status.svelte';
	import { forms } from '$lib/forms.svelte';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import { realtime } from '$lib/realtime.svelte';
	import type { Ticket } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	/**
	 * The problems: the open ones being worked, the known errors (with their workarounds, for the service desk), or all
	 * of them. Each row says how many of its incidents are still open and how many changes were made to fix it.
	 */
	type Show = 'open' | 'known' | 'all';
	const shows: { id: Show; label: string }[] = [
		{ id: 'open', label: 'Open' },
		{ id: 'known', label: 'Known errors' },
		{ id: 'all', label: 'All' }
	];
	const show = $derived((['open', 'known', 'all'].includes(page.url.searchParams.get('show') ?? '') ? page.url.searchParams.get('show') : 'open') as Show);

	// The known-error flag and the workaround are the problem form's fields; the page offers them only while the form
	// has them, with the form's own labels.
	const form = $derived(forms.of('problem'));
	const knownField = $derived(form?.fields.find((f) => f.name === 'known_error'));
	const workaroundField = $derived(form?.fields.find((f) => f.name === 'workaround'));
	const canFile = $derived(lifecycle.types.some((t) => t.type === 'problem' && t.can_file));

	let search = $state('');
	let items = $state<Ticket[]>([]);
	let more = $state(false);
	let loading = $state(true);
	let error = $state('');
	const pageSize = 50;
	let timer: ReturnType<typeof setTimeout>;

	function query(offset: number) {
		const q = new URLSearchParams({ type: 'problem', limit: String(pageSize), offset: String(offset) });
		if (search.trim()) q.set('q', search.trim());
		if (show === 'open') q.set('open', 'true');
		if (show === 'known') {
			q.set('open', 'true');
			q.set('fields.known_error', 'true');
		}
		return `/api/v1/tickets?${q}`;
	}

	async function load(append = false) {
		try {
			const r = await api<{ items: Ticket[]; has_more: boolean }>(query(append ? items.length : 0));
			items = append ? [...items, ...r.items] : r.items;
			more = r.has_more;
			error = '';
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the problems';
		} finally {
			loading = false;
		}
	}

	function later() {
		clearTimeout(timer);
		timer = setTimeout(() => void load(), 200);
	}

	function choose(s: Show) {
		const u = new URL(page.url);
		if (s === 'open') u.searchParams.delete('show');
		else u.searchParams.set('show', s);
		void goto(u, { replaceState: true, keepFocus: true, noScroll: true });
	}

	function plural(n: number, one: string, many: string) {
		return `${n} ${n === 1 ? one : many}`;
	}

	$effect(() => {
		void show;
		loading = true;
		void load();
	});

	onMount(() => {
		void forms.ensure('problem');
		// A problem, or one of its incidents or changes, changed: show the list as it is now.
		return realtime.on((e) => {
			if (e.type === 'ticket.changed' || e.type === 'resync') later();
		});
	});

	function newProblem() {
		ui.newTicketType = 'problem';
		ui.newTicketOpen = true;
	}
</script>

<svelte:head><title>Problems – ServiceLater</title></svelte:head>

<div class="page">
	<div class="top">
		<div>
			<h1>Problems</h1>
			<p class="lede">The causes behind incidents: what is being analysed, what has a workaround, and what is being fixed.</p>
		</div>
		{#if canFile}
			<div class="actions"><button class="btn primary" type="button" onclick={newProblem}>New problem</button></div>
		{/if}
	</div>

	<div class="filters" role="search">
		<div class="segmented" role="group" aria-label="Show">
			{#each shows as s (s.id)}
				{#if s.id !== 'known' || knownField}
					<button type="button" class:on={show === s.id} aria-pressed={show === s.id} onclick={() => choose(s.id)}>{s.label}</button>
				{/if}
			{/each}
		</div>
		<label class="field grow">
			<span>Title</span>
			<input type="search" bind:value={search} oninput={later} placeholder="Search problems" />
		</label>
	</div>

	{#if error}
		<p class="error" role="alert">{error}</p>
	{:else if !loading && items.length === 0}
		<p class="msg">
			{search ? 'Nothing matches.' : show === 'known' ? 'No open known errors.' : show === 'open' ? 'No open problems.' : 'No problems yet.'}
		</p>
	{/if}

	<ul class="items" aria-label="Problems">
		{#each items as p (p.id)}
			<li>
				<a href="/tickets/{p.reference}">
					<span class="name"><span class="ref">{p.reference}</span> {p.title}</span>
					<Status status={p.status} category={p.state_category} type={p.type} />
					<span class="sub">
						{#if knownField && p.fields?.known_error === true}<span class="known">{knownField.label}</span>{/if}
						<span>{p.open_incidents ? plural(p.open_incidents, 'open incident', 'open incidents') : 'No open incidents'}</span>
						{#if p.linked_changes}<span>{plural(p.linked_changes, 'change', 'changes')}</span>{/if}
						<span>{p.assignee_name ?? p.assignment_group_name ?? 'Unassigned'}</span>
					</span>
					{#if show === 'known' && workaroundField && typeof p.fields?.workaround === 'string'}
						<span class="workaround"><strong>{workaroundField.label}:</strong> {p.fields.workaround}</span>
					{/if}
				</a>
			</li>
		{/each}
	</ul>
	{#if more}
		<button class="btn" onclick={() => void load(true)}>Show more</button>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 56rem;
	}
	.top {
		display: flex;
		flex-wrap: wrap;
		align-items: flex-start;
		justify-content: space-between;
		gap: 0.75rem 1rem;
	}
	.top > div:first-child {
		flex: 1 1 18rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	.actions {
		display: flex;
		gap: 0.5rem;
		flex-shrink: 0;
	}
	.filters {
		display: flex;
		flex-wrap: wrap;
		align-items: flex-end;
		gap: 0.75rem 1rem;
		margin-bottom: 0.75rem;
	}
	.grow {
		flex: 1 1 14rem;
	}
	.items {
		list-style: none;
		margin: 0 0 1rem;
		padding: 0;
	}
	.items li {
		border-bottom: 1px solid var(--rule);
	}
	.items a {
		display: grid;
		grid-template-columns: minmax(0, 1fr) auto;
		column-gap: 1rem;
		row-gap: 0.2rem;
		padding: 0.7rem 0.25rem;
		color: var(--ink);
		text-decoration: none;
	}
	.items a:hover .name {
		text-decoration: underline;
	}
	.name {
		font-weight: 600;
		overflow-wrap: anywhere;
	}
	.ref {
		font-family: var(--font-mono);
		font-weight: 600;
		font-size: 0.8125rem;
		color: var(--ink-2);
		margin-right: 0.25rem;
	}
	.sub,
	.workaround {
		grid-column: 1 / -1;
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.sub {
		display: flex;
		flex-wrap: wrap;
		gap: 0 1rem;
	}
	.known {
		padding: 0 0.4rem;
		border-radius: 4px;
		background: var(--marker-soft);
		color: var(--marker-ink);
		font-weight: 600;
	}
	.workaround strong {
		color: var(--ink);
		font-weight: 600;
	}
	.msg {
		padding: 0.5rem 0;
		color: var(--ink-2);
	}
	.error {
		padding: 0.5rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
	}
</style>
