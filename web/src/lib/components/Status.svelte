<script lang="ts">
	import { lifecycle } from '$lib/lifecycle.svelte';
	import type { Category, Status } from '$lib/types';

	let { status, category }: { status: Status; category: Category } = $props();
</script>

<span class="status">
	<i class="{category} {lifecycle.isInitial(status) ? 'initial' : ''}" aria-hidden="true"></i>
	{lifecycle.label(status)}
</span>

<style>
	.status {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		white-space: nowrap;
	}
	/* Shape as well as colour, so status never depends on hue alone. The shape follows the category, which every
	   lifecycle has, not the status name, which is the app's to choose. */
	i {
		width: 0.65rem;
		height: 0.65rem;
		border-radius: 50%;
		border: 2px solid var(--blue);
		flex: none;
	}
	i.open {
		background: var(--blue);
	}
	i.open.initial {
		background: transparent; /* a ticket nobody has touched yet is hollow */
	}
	i.waiting {
		border-color: var(--ink);
		background: var(--marker);
		border-radius: 2px;
	}
	i.done {
		border-color: var(--ink-2);
		background: var(--ink-2);
		clip-path: polygon(50% 0, 100% 50%, 50% 100%, 0 50%);
		border-radius: 0;
	}
	i.closed {
		border-color: var(--rule);
		background: var(--rule);
	}
</style>
