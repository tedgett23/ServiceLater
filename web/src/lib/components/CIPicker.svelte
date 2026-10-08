<script lang="ts">
	import { api } from '$lib/api';
	import type { CISummary } from '$lib/types';

	/**
	 * Finds configuration items by name and offers each with a button. Items in `exclude` (already linked, or the item
	 * itself) are left out.
	 */
	let {
		label,
		action,
		exclude = [],
		disabled = false,
		onpick
	}: {
		label: string;
		/** The button's verb, such as Link or Add. */
		action: string;
		exclude?: string[];
		disabled?: boolean;
		onpick: (ci: CISummary) => void;
	} = $props();

	let search = $state('');
	let found = $state<CISummary[]>([]);
	let looked = $state(false);
	let timer: ReturnType<typeof setTimeout>;
	const id = `ci-pick-${Math.random().toString(36).slice(2, 8)}`;
	const shown = $derived(found.filter((c) => !exclude.includes(c.id)));

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
				found = (await api<{ items: CISummary[] }>(`/api/v1/cis?${new URLSearchParams({ q, limit: '8' })}`)).items;
			} catch {
				found = [];
			}
			looked = true;
		}, 200);
	}

	function pick(c: CISummary) {
		onpick(c);
		search = '';
		found = [];
		looked = false;
	}
</script>

<div class="picker">
	<div class="field">
		<label for={id}>{label}</label>
		<input {id} type="search" bind:value={search} oninput={onSearch} placeholder="Search by name" autocomplete="off" />
	</div>
	{#if looked && shown.length === 0}
		<p class="none">{found.length ? 'Everything that matches is already here.' : 'Nothing matches.'}</p>
	{/if}
	{#if shown.length}
		<ul>
			{#each shown as c (c.id)}
				<li>
					<span class="what"><strong>{c.name}</strong> <span class="cls">{c.class_label}</span></span>
					<button type="button" class="btn" {disabled} onclick={() => pick(c)} aria-label="{action} {c.name}">{action}</button>
				</li>
			{/each}
		</ul>
	{/if}
</div>

<style>
	.picker {
		display: grid;
		gap: 0.35rem;
	}
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	li {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
		padding: 0.4rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.what {
		min-width: 0;
		overflow-wrap: anywhere;
	}
	.cls,
	.none {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
</style>
