<script lang="ts">
	import { priorityLabel } from '$lib/format';

	let { priority }: { priority: 1 | 2 | 3 | 4 } = $props();

	// Urgency reads like signal strength: critical fills all four bars, low fills one.
	const filled = $derived(5 - priority);
	const heights = [4, 7, 10, 14];
</script>

<svg class="signal" class:critical={priority === 1} viewBox="0 0 17 14" role="img" aria-label="{priorityLabel[priority]} priority">
	<title>{priorityLabel[priority]} priority</title>
	{#each heights as h, i (i)}
		<rect x={i * 4.5} y={14 - h} width="3" height={h} rx="0.6" class:on={i < filled} />
	{/each}
</svg>

<style>
	.signal {
		width: 17px;
		height: 14px;
		flex: none;
	}
	rect {
		fill: var(--rule);
	}
	rect.on {
		fill: var(--ink);
	}
	.critical rect.on {
		fill: var(--red);
	}
</style>
