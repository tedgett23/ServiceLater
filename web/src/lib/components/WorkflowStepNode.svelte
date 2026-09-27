<script lang="ts">
	import { Handle, Position, type NodeProps } from '@xyflow/svelte';

	type StepState = 'visited' | 'current' | 'failed' | 'pending';

	let { data }: NodeProps = $props();
	const d = $derived(data as { label: string; type: string; state: StepState; visits: number });

	const stateText: Record<StepState, string> = { visited: 'Done', current: 'Here now', failed: 'Failed', pending: 'Not reached' };
</script>

<div class="step {d.state}" aria-current={d.state === 'current' ? 'step' : undefined}>
	<Handle type="target" position={Position.Left} />
	<span class="id">{d.label}</span>
	<span class="type">{d.type}</span>
	<span class="state">{stateText[d.state]}{d.visits > 1 ? `, ${d.visits}×` : ''}</span>
	<Handle type="source" position={Position.Right} />
</div>

<style>
	/* Fixed to match the width/height given to xyflow (WorkflowGraph.svelte), so the layout it fits the view to is the
	   layout that actually renders: a size guessed wrong throws fitView off, worse the smaller the screen. */
	.step {
		display: flex;
		flex-direction: column;
		justify-content: center;
		gap: 0.15rem;
		width: 170px;
		height: 62px;
		padding: 0.4rem 0.7rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		font-size: 0.8rem;
		overflow: hidden;
	}
	.id,
	.type,
	.state {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.id {
		font-weight: 700;
		font-size: 0.9rem;
	}
	.type {
		color: var(--ink-2);
		text-transform: lowercase;
	}
	.state {
		color: var(--ink-2);
	}
	/* Where the run is now is the one thing to look at, so it gets the marker, same as the ticket queue. */
	.step.current {
		background: var(--marker-soft);
		border-color: var(--marker);
		box-shadow: inset 4px 0 0 var(--marker);
	}
	.step.failed {
		box-shadow: inset 4px 0 0 var(--red);
	}
	.step.failed .state {
		color: var(--red);
		font-weight: 700;
	}
	.step.pending {
		opacity: 0.6;
	}
	:global(.svelte-flow__handle) {
		width: 6px;
		height: 6px;
		background: var(--ink-2);
		border: none;
	}
</style>
