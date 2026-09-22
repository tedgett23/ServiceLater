<script lang="ts">
	import type { RunStatus } from '$lib/types';

	let { status }: { status: RunStatus } = $props();

	const label: Record<RunStatus, string> = {
		queued: 'Queued',
		running: 'Running',
		waiting: 'Waiting',
		done: 'Done',
		failed: 'Failed',
		cancelled: 'Cancelled'
	};
</script>

<span class="status">
	<i class={status} aria-hidden="true"></i>
	{label[status]}
</span>

<style>
	.status {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		white-space: nowrap;
	}
	/* A shape for each state as well as a colour, so it never depends on hue alone. */
	i {
		width: 0.65rem;
		height: 0.65rem;
		border: 2px solid var(--blue);
		border-radius: 50%;
		flex: none;
	}
	i.queued {
		background: transparent;
	}
	i.running {
		background: var(--blue);
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
	i.failed {
		border-color: var(--red);
		background: var(--red);
		clip-path: polygon(50% 0, 100% 100%, 0 100%);
		border-radius: 0;
	}
	i.cancelled {
		border-color: var(--rule);
		background: var(--rule);
	}
</style>
