<script lang="ts">
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { ui } from '$lib/ui.svelte';
	import Avatar from './Avatar.svelte';
	import Icon, { type IconName } from './Icon.svelte';

	// What GET /search returns: each result comes ready to show and to open, so nothing is worked out here.
	type Result = { kind: 'record' | 'article' | 'item' | 'person'; id: string; reference?: string; title: string; detail?: string; link: string };
	type Found = { query: string; mode: 'reference' | 'text'; prefix?: string; items: Result[] };

	/** The server searches from three characters: enough to be a reference prefix (INC) or the start of a word. */
	const MIN = 3;
	const icons: Record<Result['kind'], IconName> = { record: 'tickets', article: 'knowledge', item: 'cmdb', person: 'tickets' };

	let dialog: HTMLDialogElement;
	let q = $state('');
	let found = $state<Found | null>(null);
	let active = $state(0);
	let busy = $state(false);
	let failed = $state(false);
	let timer: ReturnType<typeof setTimeout>;
	let seq = 0; // only the latest request's answer is shown
	let openWhenFound = false; // Enter was pressed while the search was still running

	const text = $derived(q.trim());
	const items = $derived(found?.items ?? []);

	// The dialog element owns focus trapping and Escape; we just mirror its open state.
	$effect(() => {
		if (ui.searchOpen && !dialog.open) {
			q = '';
			found = null;
			failed = false;
			openWhenFound = false;
			dialog.showModal();
		}
		if (!ui.searchOpen && dialog.open) dialog.close();
	});

	function onInput() {
		clearTimeout(timer);
		seq++;
		openWhenFound = false;
		if (text.length < MIN) {
			found = null;
			busy = false;
			return;
		}
		busy = true;
		const wanted = text;
		const mine = seq;
		timer = setTimeout(() => void run(wanted, mine), 150);
	}

	async function run(wanted: string, mine: number) {
		try {
			const r = await api<Found>(`/api/v1/search?q=${encodeURIComponent(wanted)}`);
			if (mine !== seq) return;
			found = r;
			active = 0;
			failed = false;
			// Someone who types INC42 and Enter in one go gets INC42, not whatever the list showed a moment ago.
			if (openWhenFound && r.items[0]) open(r.items[0]);
		} catch {
			if (mine !== seq) return;
			found = null;
			failed = true;
		} finally {
			if (mine === seq) {
				busy = false;
				openWhenFound = false;
			}
		}
	}

	function open(r: Result) {
		ui.searchOpen = false;
		void goto(r.link);
	}

	function onKey(e: KeyboardEvent) {
		if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
			e.preventDefault();
			if (!items.length) return;
			active = (active + (e.key === 'ArrowDown' ? 1 : items.length - 1)) % items.length;
			document.getElementById(`found-${active}`)?.scrollIntoView({ block: 'nearest' });
		} else if (e.key === 'Enter') {
			e.preventDefault();
			if (busy) openWhenFound = true;
			else if (items[active]) open(items[active]);
		}
	}

	function onWindowKey(e: KeyboardEvent) {
		if ((e.metaKey || e.ctrlKey) && !e.altKey && e.key.toLowerCase() === 'k') {
			e.preventDefault();
			ui.searchOpen = true;
		}
	}
</script>

<svelte:window onkeydown={onWindowKey} />

<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_noninteractive_element_interactions -->
<dialog
	bind:this={dialog}
	aria-label="Search"
	onclose={() => (ui.searchOpen = false)}
	onclick={(e) => e.target === dialog && (ui.searchOpen = false)}
>
	<div class="box">
		<label class="bar">
			<Icon name="search" size={17} />
			<input
				bind:value={q}
				oninput={onInput}
				onkeydown={onKey}
				type="text"
				role="combobox"
				aria-label="Search everything"
				aria-expanded={items.length > 0}
				aria-controls="found"
				aria-activedescendant={items.length ? `found-${active}` : undefined}
				aria-autocomplete="list"
				placeholder="Search records, articles and items"
				autocomplete="off"
				spellcheck="false"
				maxlength="100"
			/>
			<kbd aria-hidden="true">esc</kbd>
		</label>

		{#if text.length < MIN}
			<p class="hint">Type at least three characters. A prefix such as <b>INC</b> or <b>INC42</b> jumps straight to a record.</p>
		{:else if failed}
			<p class="hint" role="alert">The search could not be run. Try again.</p>
		{:else if found && items.length === 0}
			<p class="hint" role="status">Nothing matches “{found.query}”.</p>
		{:else if found}
			{#if found.mode === 'reference'}
				<p class="how">{found.query.length > (found.prefix?.length ?? 0) ? `References starting with ${found.query.toUpperCase().replace(/[\s-]/g, '')}` : `Latest ${found.prefix} records`}</p>
			{/if}
			<ul id="found" role="listbox" aria-label="Results">
				{#each items as r, i (r.kind + r.id)}
					<!-- svelte-ignore a11y_click_events_have_key_events -->
					<li id="found-{i}" role="option" aria-selected={i === active} class:active={i === active} onclick={() => open(r)} onmousemove={() => (active = i)}>
						{#if r.kind === 'person'}<Avatar name={r.title} size="sm" />{:else}<Icon name={icons[r.kind]} />{/if}
						{#if r.reference}<span class="ref">{r.reference}</span>{/if}
						<span class="title">{r.title}</span>
						{#if r.detail}<span class="detail">{r.detail}</span>{/if}
					</li>
				{/each}
			</ul>
		{:else}
			<p class="hint" role="status">Searching…</p>
		{/if}
	</div>
</dialog>

<style>
	dialog {
		width: min(38rem, calc(100vw - 1.5rem));
		margin-top: min(18vh, 9rem);
		padding: 0;
		border: 1px solid var(--rule);
		border-radius: 12px;
		background: var(--raised);
		color: var(--ink);
		box-shadow: var(--shadow-pop);
		overflow: hidden;
	}
	dialog::backdrop {
		background: color-mix(in srgb, #0b0d13 45%, transparent);
	}
	.bar {
		display: flex;
		align-items: center;
		gap: 0.6rem;
		padding: 0 0.9rem;
		color: var(--ink-2);
		border-bottom: 1px solid var(--rule);
	}
	input {
		flex: 1;
		min-width: 0;
		height: 3rem;
		border: 0;
		outline: 0;
		background: none;
		color: var(--ink);
		font-size: 0.9375rem;
	}
	.hint,
	.how {
		padding: 0.9rem 1rem;
		color: var(--ink-2);
		font-size: 0.8125rem;
	}
	.hint b {
		font: 600 0.75rem var(--font-mono);
		color: var(--ink);
	}
	.how {
		padding: 0.6rem 1rem 0.2rem;
		font-size: 0.75rem;
		font-weight: 600;
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0.35rem;
		max-height: min(26rem, 55vh);
		overflow-y: auto;
	}
	li {
		display: flex;
		align-items: center;
		gap: 0.6rem;
		min-height: 2.25rem;
		padding: 0.3rem 0.6rem;
		border-radius: 7px;
		color: var(--ink-2);
		cursor: pointer;
	}
	li.active {
		background: var(--hover);
	}
	.ref {
		flex: none;
		font: 500 0.75rem var(--font-mono);
	}
	.title {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		color: var(--ink);
		font-weight: 500;
	}
	.detail {
		flex: none;
		max-width: 40%;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		font-size: 0.78rem;
	}
</style>
