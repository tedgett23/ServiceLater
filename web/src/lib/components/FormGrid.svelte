<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { FormSection } from '$lib/types';

	/**
	 * Lays out one section of a form. Without columns its fields flow into as many columns as fit, at most three, and
	 * wide ones (long text, or named in the section's `wide`) take a whole row. With columns, each field stays in the
	 * column it was put in, each column read down. Either way it folds by the room it has, not the window's: to one
	 * column when narrow, and three set columns to two in a middling space (the third goes under the first).
	 */
	let {
		section,
		show = () => true,
		isWide = () => false,
		min = '15rem',
		item
	}: {
		section: FormSection;
		/** Whether a field is shown now (hidden ones take no room). */
		show?: (name: string) => boolean;
		/** Whether a field takes a whole row of an automatic section, besides those the section names. */
		isWide?: (name: string) => boolean;
		/** The narrowest an automatic column may be. */
		min?: string;
		item: Snippet<[string]>;
	} = $props();

	const columns = $derived(section.columns?.length ? section.columns.map((c) => c.filter(show)) : null);
	const flow = $derived(section.fields.filter(show));
	const wide = (name: string) => isWide(name) || !!section.wide?.includes(name);
</script>

<div class="room">
	{#if columns}
		<div class="cols" class:three={columns.length === 3} style:--n={columns.length}>
			{#each columns as col, i (i)}
				<div class="col">
					{#each col as name (name)}
						<div class="cell">{@render item(name)}</div>
					{/each}
				</div>
			{/each}
		</div>
	{:else}
		<div class="auto" style:--min={min}>
			{#each flow as name (name)}
				<div class="cell" class:wide={wide(name)}>{@render item(name)}</div>
			{/each}
		</div>
	{/if}
</div>

<style>
	.room {
		container-type: inline-size;
		--gap: 1rem;
	}
	.auto {
		display: grid;
		/* As many columns as fit at --min, but never more than three. */
		grid-template-columns: repeat(auto-fill, minmax(max(var(--min), calc((100% - 2 * var(--gap)) / 3)), 1fr));
		gap: 0.9rem var(--gap);
		align-items: start;
	}
	.wide {
		grid-column: 1 / -1;
	}
	.cols {
		display: grid;
		grid-template-columns: repeat(var(--n), minmax(0, 1fr));
		gap: 0.9rem var(--gap);
		align-items: start;
	}
	.col {
		display: grid;
		gap: 0.9rem;
		align-content: start;
		min-width: 0;
	}
	.cell {
		min-width: 0;
	}
	@container (max-width: 40rem) {
		.cols.three {
			grid-template-columns: repeat(2, minmax(0, 1fr));
		}
	}
	@container (max-width: 22rem) {
		.cols,
		.cols.three {
			grid-template-columns: minmax(0, 1fr);
		}
	}
</style>
