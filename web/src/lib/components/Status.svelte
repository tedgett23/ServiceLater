<script lang="ts">
	import { statusLabel } from '$lib/format';
	import type { Status } from '$lib/types';

	let { status }: { status: Status } = $props();
</script>

<span class="status">
	<i class={status} aria-hidden="true"></i>
	{statusLabel[status]}
</span>

<style>
	.status {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		white-space: nowrap;
	}
	/* Shape as well as colour, so status never depends on hue alone. */
	i {
		width: 0.65rem;
		height: 0.65rem;
		border-radius: 50%;
		border: 2px solid var(--blue);
		flex: none;
	}
	i.new {
		background: transparent;
	}
	i.in_progress {
		background: var(--blue);
	}
	i.on_hold {
		border-color: var(--ink);
		background: var(--marker);
		border-radius: 2px;
	}
	i.resolved {
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
