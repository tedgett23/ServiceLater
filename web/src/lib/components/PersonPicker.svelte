<script lang="ts">
	import { api } from '$lib/api';
	import type { User } from '$lib/types';

	/**
	 * Chooses one person from the directory by name or email: a manager, or who a request is for. Shows who is chosen,
	 * with a way to change or clear it.
	 */
	let {
		id,
		label,
		value,
		name,
		exclude = [],
		disabled = false,
		clearable = true,
		onchange
	}: {
		id: string;
		label: string;
		value: string | null;
		name: string | null;
		exclude?: string[];
		disabled?: boolean;
		clearable?: boolean;
		onchange: (id: string | null, name: string | null) => void;
	} = $props();

	let search = $state('');
	let found = $state<User[]>([]);
	let looked = $state(false);
	let choosing = $state(false);
	let timer: ReturnType<typeof setTimeout>;
	const shown = $derived(found.filter((u) => !exclude.includes(u.id)));

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
				found = (await api<{ items: User[] }>(`/api/v1/users?${new URLSearchParams({ q, limit: '8' })}`)).items;
			} catch {
				found = [];
			}
			looked = true;
		}, 200);
	}

	function choose(u: User | null) {
		onchange(u?.id ?? null, u?.name ?? null);
		choosing = false;
		search = '';
		found = [];
		looked = false;
	}
</script>

<div class="picker">
	{#if value && !choosing}
		<span class="label" id="{id}-label">{label}</span>
		<div class="chosen" role="group" aria-labelledby="{id}-label">
			<strong>{name}</strong>
			{#if !disabled}
				<button type="button" class="btn quiet" onclick={() => (choosing = true)} aria-label="Change {label.toLowerCase()}">Change</button>
				{#if clearable}<button type="button" class="btn quiet" onclick={() => choose(null)} aria-label="Remove {label.toLowerCase()}">Remove</button>{/if}
			{/if}
		</div>
	{:else}
		<label class="label" for={id}>{label}</label>
		<input
			{id}
			type="search"
			bind:value={search}
			oninput={onSearch}
			onkeydown={(e) => {
				if (e.key === 'Enter') e.preventDefault();
				if (e.key === 'Escape' && value) choosing = false;
			}}
			placeholder="Search by name or email"
			autocomplete="off"
			{disabled}
		/>
		{#if looked && shown.length === 0}<p class="none">Nobody matches.</p>{/if}
		{#if shown.length}
			<ul class="found" aria-label="People found">
				{#each shown as u (u.id)}
					<li><button type="button" onclick={() => choose(u)}><strong>{u.name}</strong> <span>{u.email}</span></button></li>
				{/each}
			</ul>
		{/if}
	{/if}
</div>

<style>
	.picker {
		display: grid;
		gap: 0.35rem;
	}
	.label {
		font-weight: 600;
		font-size: 0.85rem;
	}
	.chosen {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		min-height: 2.5rem;
	}
	.quiet {
		min-height: 2rem;
		font-weight: 500;
	}
	.found {
		list-style: none;
		margin: 0;
		padding: 0;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
	}
	.found li + li {
		border-top: 1px solid var(--rule);
	}
	.found button {
		display: flex;
		gap: 0.5rem;
		align-items: baseline;
		width: 100%;
		padding: 0.45rem 0.6rem;
		border: 0;
		background: none;
		color: var(--ink);
		font: inherit;
		text-align: left;
		cursor: pointer;
		overflow-wrap: anywhere;
	}
	.found button:hover,
	.found button:focus-visible {
		background: var(--blue-soft);
	}
	.found span {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.none {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
</style>
