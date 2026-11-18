<script lang="ts">
	import { Handle, Position, type NodeProps } from '@xyflow/svelte';
	import type { Exit, StepActivity } from './workflow';

	/**
	 * One step in the builder: its id and type, a dot to connect into on the left, and one dot per way out on the right
	 * (next; then and else; next and on timeout), each labelled, so a connection says which way it goes.
	 */
	let { data, selected }: NodeProps = $props();
	const d = $derived(
		data as { id: string; type: string; label: string; start: boolean; exits: Exit[]; editable: boolean; activity?: StepActivity }
	);
	// Runs at this step now are the thing to look at, so they get the marker, as in the run viewer.
	const live = $derived(!!d.activity?.now);
	const top = (i: number, n: number) => `${((i + 1) * 100) / (n + 1)}%`;
</script>

<div class="step" class:selected class:start={d.start} class:live class:has-activity={!!d.activity}>
	<Handle type="target" position={Position.Left} isConnectable={d.editable} />
	<span class="id">{d.id}</span>
	<span class="type">{d.start ? `Start · ${d.label}` : d.label}</span>
	{#if d.activity}
		<span class="activity">
			{#if d.activity.now}<strong>{d.activity.now} here now</strong>{/if}
			{#if d.activity.visits}<span>{d.activity.visits} in 7 days</span>{/if}
			{#if d.activity.failed}<span class="failed">{d.activity.failed} failed</span>{/if}
		</span>
	{/if}
	{#each d.exits as e, i (e.key)}
		<Handle type="source" id={e.key} position={Position.Right} style="top: {top(i, d.exits.length)}" isConnectable={d.editable} />
		{#if d.exits.length > 1}<span class="exit" style="top: {top(i, d.exits.length)}">{e.label}</span>{/if}
	{/each}
</div>

<style>
	/* The same size as the run viewer's steps (WorkflowStepNode.svelte), which the canvas lays out by. */
	.step {
		position: relative;
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
		color: var(--ink);
		font-size: 0.8rem;
	}
	.step.start {
		box-shadow: inset 4px 0 0 var(--blue);
	}
	.step.has-activity {
		gap: 0.05rem;
	}
	.step.live {
		background: var(--marker-soft);
		border-color: var(--marker);
		box-shadow: inset 4px 0 0 var(--marker);
	}
	.activity {
		display: flex;
		gap: 0.4rem;
		font-size: 0.7rem;
		color: var(--ink-2);
		white-space: nowrap;
		overflow: hidden;
	}
	.activity strong {
		color: var(--ink);
	}
	.activity .failed {
		color: var(--red);
		font-weight: 700;
	}
	.step.selected {
		border-color: var(--blue);
		outline: 2px solid var(--blue);
	}
	.id {
		font-weight: 700;
		font-size: 0.9rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.type {
		color: var(--ink-2);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	/* Outside the box, just above its dot, so it never covers the step's own text. */
	.exit {
		position: absolute;
		left: calc(100% + 6px);
		transform: translateY(-115%);
		font-size: 0.65rem;
		line-height: 1;
		color: var(--ink-2);
		white-space: nowrap;
		pointer-events: none;
	}
	.step :global(.svelte-flow__handle) {
		width: 9px;
		height: 9px;
		background: var(--blue);
		border: 2px solid var(--surface);
	}
</style>
