<script lang="ts">
	import { SvelteFlow, Background, Controls, MarkerType, Position, type Node, type Edge, type Viewport } from '@xyflow/svelte';
	import '@xyflow/svelte/dist/style.css';
	import WorkflowStepNode from './WorkflowStepNode.svelte';
	import type { GraphStep } from '$lib/types';

	type StepState = 'visited' | 'current' | 'failed' | 'pending';

	let {
		graph,
		start,
		standing
	}: {
		graph: GraphStep[];
		start?: string;
		standing: (id: string) => { visits: number; state: StepState };
	} = $props();

	const nodeTypes = { step: WorkflowStepNode };
	const kindLabel: Record<string, string> = { then: 'then', else: 'else', timeout: 'on timeout' };

	const NODE_W = 170;
	const NODE_H = 62;
	const COL_W = 210;
	const ROW_H = 90;

	let cw = $state(0);
	let ch = $state(0);

	/** Left-to-right columns by distance from the start step, so the diagram reads the way the engine walks it. Every
	 * step is reachable from start (checked at install), so a fixed-point pass over the small DAG settles quickly. */
	function layout(steps: GraphStep[], startId?: string) {
		const byId = new Set(steps.map((s) => s.id));
		const level = new Map<string, number>();
		const root = startId && byId.has(startId) ? startId : steps[0]?.id;
		if (root) level.set(root, 0);

		let changed = true;
		for (let guard = 0; changed && guard < steps.length + 1; guard++) {
			changed = false;
			for (const s of steps) {
				const l = level.get(s.id);
				if (l === undefined) continue;
				for (const e of s.next) {
					if ((level.get(e.to) ?? -1) < l + 1) {
						level.set(e.to, l + 1);
						changed = true;
					}
				}
			}
		}
		let stray = 0;
		for (const s of steps) if (!level.has(s.id)) level.set(s.id, stray++);

		const columns = new Map<number, string[]>();
		for (const s of steps) {
			const l = level.get(s.id) ?? 0;
			columns.set(l, [...(columns.get(l) ?? []), s.id]);
		}
		const pos = new Map<string, { x: number; y: number }>();
		for (const [l, ids] of columns) ids.forEach((id, i) => pos.set(id, { x: l * COL_W, y: i * ROW_H }));
		return pos;
	}

	const nodes = $derived.by((): Node[] => {
		const pos = layout(graph, start);
		return graph.map((s) => ({
			id: s.id,
			type: 'step',
			position: pos.get(s.id) ?? { x: 0, y: 0 },
			width: NODE_W,
			height: NODE_H,
			draggable: false,
			connectable: false,
			deletable: false,
			sourcePosition: Position.Right,
			targetPosition: Position.Left,
			data: { label: s.id, type: s.type, ...standing(s.id) }
		}));
	});

	const edges = $derived.by((): Edge[] => {
		const out: Edge[] = [];
		for (const s of graph) {
			for (const e of s.next) {
				out.push({
					id: `${s.id}->${e.to}:${e.kind ?? 'next'}`,
					source: s.id,
					target: e.to,
					label: e.kind ? kindLabel[e.kind] : undefined,
					type: 'smoothstep',
					deletable: false,
					markerEnd: { type: MarkerType.ArrowClosed },
					style: 'stroke: var(--ink-2);'
				});
			}
		}
		return out;
	});

	// Computed once the canvas has a real size, instead of xyflow's own fitView: that measures custom nodes
	// asynchronously and can settle on a zoom from a stale size, which crops the diagram worst on a phone (small
	// canvas, so the same pixel error eats a whole column). We already know every node's position and size, so we
	// can fit them ourselves and skip the race entirely.
	const initialViewport = $derived.by((): Viewport | undefined => {
		if (!cw || !ch || !nodes.length) return undefined;
		let minX = Infinity,
			minY = Infinity,
			maxX = -Infinity,
			maxY = -Infinity;
		for (const n of nodes) {
			minX = Math.min(minX, n.position.x);
			minY = Math.min(minY, n.position.y);
			maxX = Math.max(maxX, n.position.x + NODE_W);
			maxY = Math.max(maxY, n.position.y + NODE_H);
		}
		const w = Math.max(maxX - minX, 1);
		const h = Math.max(maxY - minY, 1);
		const pad = 32;
		const zoom = Math.min((cw - pad * 2) / w, (ch - pad * 2) / h, 1.25);
		return { zoom, x: (cw - w * zoom) / 2 - minX * zoom, y: (ch - h * zoom) / 2 - minY * zoom };
	});
</script>

<div class="canvas" bind:clientWidth={cw} bind:clientHeight={ch}>
	{#if initialViewport}
		<SvelteFlow
			{nodes}
			{edges}
			{nodeTypes}
			{initialViewport}
			minZoom={0.1}
			colorMode="system"
			nodesDraggable={false}
			nodesConnectable={false}
			elementsSelectable={false}
			panOnScroll
			proOptions={{ hideAttribution: true }}
		>
			<Background />
			<Controls showLock={false} />
		</SvelteFlow>
	{/if}
</div>

<style>
	.canvas {
		height: 24rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
		overflow: hidden;
	}
	:global(.canvas .svelte-flow__edge-text) {
		fill: var(--ink-2);
	}
	:global(.canvas .svelte-flow__edge-textbg) {
		fill: var(--surface);
	}
	:global(.canvas .svelte-flow__controls-button) {
		background: var(--raised);
		border-bottom-color: var(--rule);
		fill: var(--ink);
	}
	:global(.canvas .svelte-flow__controls-button:hover) {
		background: var(--blue-soft);
	}
</style>
