<script lang="ts">
	import type { Snippet } from 'svelte';
	import { untrack } from 'svelte';
	import { Background, Controls, MarkerType, Position, SvelteFlow, type Connection, type Edge, type Node, type Viewport } from '@xyflow/svelte';
	import '@xyflow/svelte/dist/style.css';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import StepNode from './StepNode.svelte';
	import StepForm from './StepForm.svelte';
	import { autoLayout, NODE_H, NODE_W } from './layout';
	import { when } from '$lib/format';
	import { exitsOf, freshId, linkFields, stepTypes, tidy, typeInfo, type Step, type StepType, type WorkflowActivity, type WorkflowDef } from './workflow';

	/**
	 * The workflow builder: the steps as a diagram to drag, connect and delete, and the selected step's fields beside it.
	 * It edits a copy; save sends the whole definition, which the server checks and installs (or answers with every
	 * problem). Positions are saved with it, so the diagram stays as it was arranged.
	 */
	let {
		def,
		editable,
		activity = null,
		onsave,
		tools
	}: {
		def: WorkflowDef;
		editable: boolean;
		/** Where the workflow's runs are, live; null hides it. */
		activity?: WorkflowActivity | null;
		onsave: (body: { record_type: string; start: string; steps: Step[]; layout: Record<string, { x: number; y: number }> }) => Promise<boolean>;
		/** More controls for the toolbar, before Save (the page's "Show where its runs are"). */
		tools?: Snippet;
	} = $props();

	let steps = $state<Step[]>(untrack(() => structuredClone($state.snapshot(def.steps) as Step[])));
	let start = $state(untrack(() => def.start));
	let selectedId = $state<string | null>(null);
	let dirty = $state(false);
	let saving = $state(false);
	let nodes = $state.raw<Node[]>([]);
	let edges = $state.raw<Edge[]>([]);

	const nodeTypes = { step: StepNode };
	const selected = $derived(steps.find((s) => s.id === selectedId) ?? null);

	/** Where each step is now: as dragged, else as saved, else laid out from the graph. */
	function positions(renamed?: { from: string; to: string }) {
		const auto = autoLayout(steps, start);
		const current = new Map(nodes.map((n) => [n.id === renamed?.from ? renamed.to : n.id, n.position]));
		return (id: string) => current.get(id) ?? def.layout[id] ?? auto.get(id) ?? { x: 0, y: 0 };
	}

	function rebuild(renamed?: { from: string; to: string }) {
		const at = positions(renamed);
		nodes = steps.map((s) => ({
			id: s.id,
			type: 'step',
			position: { ...at(s.id) },
			width: NODE_W,
			height: NODE_H,
			selected: s.id === selectedId,
			draggable: editable,
			connectable: editable,
			deletable: editable,
			sourcePosition: Position.Right,
			targetPosition: Position.Left,
			data: {
				id: s.id,
				type: s.type,
				label: typeInfo(s.type)?.label ?? s.type,
				start: s.id === start,
				exits: exitsOf(s),
				editable,
				activity: activity?.steps[s.id]
			}
		}));
		const ids = new Set(steps.map((s) => s.id));
		const out: Edge[] = [];
		for (const s of steps) {
			for (const e of exitsOf(s)) {
				const to = s[e.field];
				if (!to || !ids.has(to)) continue;
				out.push({
					id: `${s.id}:${e.field}`,
					source: s.id,
					sourceHandle: e.field,
					target: to,
					type: 'smoothstep',
					deletable: editable,
					markerEnd: { type: MarkerType.ArrowClosed },
					style: 'stroke: var(--ink-2);'
				});
			}
		}
		edges = out;
	}
	rebuild();
	// Runs move on their own: show them where they are now, keeping the boxes where they are.
	$effect(() => {
		void activity;
		untrack(() => rebuild());
	});
	// Where each step started out, so saving keeps only the positions someone set (moved here, or saved before), and an
	// edit of a shipped workflow does not carry a position for every step.
	const startedAt = new Map(untrack(() => nodes.map((n) => [n.id, { ...n.position }])));

	function changed(renamed?: { from: string; to: string }) {
		dirty = true;
		rebuild(renamed);
	}

	// ---- editing the graph ----

	function connect(c: Connection) {
		const s = steps.find((x) => x.id === c.source);
		if (!s || !c.sourceHandle || c.source === c.target) return;
		(s as unknown as Record<string, string>)[c.sourceHandle] = c.target;
		changed();
	}

	function removed({ nodes: gone, edges: cut }: { nodes: Node[]; edges: Edge[] }) {
		const goneIds = new Set(gone.map((n) => n.id));
		for (const e of cut) {
			const [source, field] = e.id.split(':');
			const s = steps.find((x) => x.id === source);
			if (s && (linkFields as readonly string[]).includes(field)) delete (s as unknown as Record<string, unknown>)[field];
		}
		if (goneIds.size) removeSteps(goneIds);
		changed();
	}

	function removeSteps(ids: Set<string>) {
		steps = steps.filter((s) => !ids.has(s.id));
		for (const s of steps) for (const f of linkFields) if (s[f] && ids.has(s[f]!)) delete s[f];
		if (ids.has(start)) start = steps[0]?.id ?? '';
		if (selectedId && ids.has(selectedId)) selectedId = null;
	}

	let adding = $state<StepType | ''>('');
	function add(type: StepType) {
		const id = freshId(steps, type);
		const near = selected ? nodes.find((n) => n.id === selected.id)?.position : undefined;
		const step: Step = { id, type };
		if (type === 'comment.add') step.body = { literal: '' };
		steps = [...steps, step];
		// A new step after the selected one, joined to it when its way out is free.
		if (selected) {
			const exit = exitsOf(selected).find((e) => !selected[e.field]);
			if (exit) (selected as unknown as Record<string, string>)[exit.field] = id;
		}
		if (!start) start = id;
		selectedId = id;
		rebuild();
		if (near) nodes = nodes.map((n) => (n.id === id ? { ...n, position: { x: near.x + 230, y: near.y } } : n));
		dirty = true;
		adding = '';
	}

	function update(next: Step) {
		const prev = steps.find((s) => s.id === selectedId);
		if (!prev) return;
		const renamed = next.id !== prev.id ? { from: prev.id, to: next.id } : undefined;
		steps = steps.map((s) => (s.id === prev.id ? next : s));
		if (renamed) {
			for (const s of steps) for (const f of linkFields) if (s[f] === renamed.from) s[f] = renamed.to;
			if (start === renamed.from) start = renamed.to;
			selectedId = renamed.to;
		}
		changed(renamed);
	}

	// ---- saving ----

	let problems = $state<string[]>([]);
	async function save() {
		saving = true;
		problems = [];
		const layout: Record<string, { x: number; y: number }> = {};
		for (const n of nodes) {
			const was = startedAt.get(n.id);
			const moved = !was || Math.round(was.x) !== Math.round(n.position.x) || Math.round(was.y) !== Math.round(n.position.y);
			if (moved || def.layout[n.id]) layout[n.id] = { x: Math.round(n.position.x), y: Math.round(n.position.y) };
		}
		try {
			if (await onsave({ record_type: def.record_type, start, steps: steps.map(tidy), layout })) dirty = false;
		} finally {
			saving = false;
		}
	}
	export function showProblems(list: string[]) {
		problems = list;
	}

	// The diagram is fitted once the canvas has a size; the run viewer explains why it does not trust fitView.
	// Below TOO_SMALL the whole diagram cannot be read, so it opens at READABLE instead.
	const TOO_SMALL = 0.45;
	const READABLE = 0.75;
	let cw = $state(0);
	let ch = $state(0);
	const initialViewport = $derived.by((): Viewport | undefined => {
		if (!cw || !ch) return undefined;
		const ns = untrack(() => nodes);
		if (!ns.length) return { x: 40, y: 40, zoom: 1 };
		const xs = ns.map((n) => n.position.x);
		const ys = ns.map((n) => n.position.y);
		const w = Math.max(...xs) + NODE_W - Math.min(...xs);
		const h = Math.max(...ys) + NODE_H - Math.min(...ys);
		const zoom = Math.min((cw - 64) / Math.max(w, 1), (ch - 64) / Math.max(h, 1), 1.1);
		if (zoom < TOO_SMALL) {
			// Too wide to read whole (a phone): open on the start step at a size that can be read, and pan from there.
			const first = ns.find((n) => n.id === untrack(() => start)) ?? ns[0];
			return { zoom: READABLE, x: 24 - first.position.x * READABLE, y: ch / 2 - (first.position.y + NODE_H / 2) * READABLE };
		}
		return { zoom, x: (cw - w * zoom) / 2 - Math.min(...xs) * zoom, y: (ch - h * zoom) / 2 - Math.min(...ys) * zoom };
	});
	$effect(() => {
		void lifecycle.ensure();
	});
</script>

<div class="builder">
	{#if editable || tools}
		<div class="toolbar" role="toolbar" aria-label="Workflow">
			{#if editable}
				<label class="sr-only" for="add-step">Add a step</label>
				<select id="add-step" bind:value={adding} onchange={() => adding && add(adding)}>
					<option value="">Add a step…</option>
					{#each stepTypes as t (t.type)}<option value={t.type}>{t.label}</option>{/each}
				</select>
				<span class="hint">Drag from a step’s right-hand dot to another step to connect them. Select a step or connection and press Delete to remove it.</span>
			{/if}
			<div class="end">
				{@render tools?.()}
				{#if editable}<button class="btn primary" disabled={!dirty || saving} onclick={save}>{saving ? 'Saving…' : 'Save'}</button>{/if}
			</div>
		</div>
	{/if}

	{#if problems.length}
		<div class="problems" role="alert">
			<p>It was not saved:</p>
			<ul>
				{#each problems as p, i (i)}<li>{p}</li>{/each}
			</ul>
		</div>
	{/if}

	<div class="work">
		<div class="canvas" bind:clientWidth={cw} bind:clientHeight={ch} aria-label="Workflow diagram" role="figure">
			{#if initialViewport}
				<SvelteFlow
					bind:nodes
					bind:edges
					{nodeTypes}
					{initialViewport}
					minZoom={0.1}
					colorMode="system"
					nodesDraggable={editable}
					nodesConnectable={editable}
					elementsSelectable
					deleteKey={editable ? ['Backspace', 'Delete'] : null}
					onconnect={connect}
					ondelete={removed}
					onnodeclick={({ node }) => (selectedId = node.id)}
					onpaneclick={() => (selectedId = null)}
					onnodedragstop={() => (dirty = true)}
					proOptions={{ hideAttribution: true }}
				>
					<Background />
					<Controls showLock={false} />
				</SvelteFlow>
			{/if}
		</div>

		<aside class="panel" aria-label={selected ? `Step ${selected.id}` : 'Workflow settings'}>
			{#if selected}
				{#key selected.id}
					{#if activity}
						{@const here = activity.in_flight.filter((r) => r.step === selected!.id)}
						{#if here.length}
							<p class="here">
								{here.length} {here.length === 1 ? 'run is' : 'runs are'} here now:
								{#each here.slice(0, 5) as r, i (r.id)}{i ? ', ' : ''}<a href="/runs/{r.id}">{r.reference ?? 'a run'}</a>{/each}{here.length > 5 ? '…' : ''}
							</p>
						{/if}
					{/if}
					<StepForm
						step={selected}
						{steps}
						recordType={def.record_type}
						isStart={selected.id === start}
						{editable}
						onchange={update}
						onstart={() => ((start = selected!.id), changed())}
						ondelete={() => (removeSteps(new Set([selected!.id])), changed())}
					/>
				{/key}
			{:else}
				<h2>{def.name}</h2>
				<dl>
					<div><dt>App</dt><dd>{def.app}</dd></div>
					<div><dt>For</dt><dd>{lifecycle.typeLabel(def.record_type)} records</dd></div>
					<div><dt>Steps</dt><dd>{steps.length}</dd></div>
				</dl>
				<div class="field">
					<label for="wf-start">Starts with</label>
					<select id="wf-start" bind:value={start} disabled={!editable} onchange={() => changed()}>
						{#each steps as s (s.id)}<option value={s.id}>{s.id}</option>{/each}
					</select>
				</div>
				<p class="hint">
					Its steps act as the {def.app} app for whoever started it, so they can do only what the app may:
					{def.app_permissions?.length ? def.app_permissions.join(', ') : 'nothing on records'}.
				</p>
				<p class="hint">Select a step to change it.</p>
				{#if activity}
					<section class="live" aria-labelledby="live-h">
						<h3 id="live-h">Running now</h3>
						{#if activity.in_flight.length === 0}
							<p class="hint">No runs are going.</p>
						{:else}
							<ul>
								{#each activity.in_flight as r (r.id)}
									<li>
										<a href="/runs/{r.id}">{r.reference ? `${r.reference} ${r.title}` : 'A run'}</a>
										<span class="hint">at {r.step}, {r.status === 'waiting' && r.wake_at ? `waiting until ${when(r.wake_at)}` : r.status}</span>
									</li>
								{/each}
							</ul>
							{#if activity.more}<p class="hint">And more: see <a href="/runs">Runs</a>.</p>{/if}
						{/if}
						<p class="hint">
							In the last 7 days: {Object.values(activity.runs).reduce((a, b) => a + b, 0)} started{activity.runs.failed
								? `, ${activity.runs.failed} failed`
								: ''}.
						</p>
					</section>
				{/if}
			{/if}
		</aside>
	</div>
</div>

<style>
	/* The builder fills what the page leaves it: the toolbar, then the canvas and panel to the bottom of the window. */
	.builder {
		flex: 1 1 auto;
		min-height: 30rem;
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}
	.toolbar {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.6rem 0.75rem;
		padding: 0.45rem 0.5rem 0.45rem 0.45rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
	}
	.toolbar .hint {
		flex: 1 1 16rem;
		min-width: 0;
	}
	.end {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		margin-left: auto;
	}
	.toolbar select {
		min-height: 2.25rem;
		padding: 0.4rem 0.6rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.problems {
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
	}
	.problems p {
		font-weight: 700;
		color: var(--red);
	}
	.problems ul {
		margin: 0.3rem 0 0;
		padding-left: 1.2rem;
		font-size: 0.9rem;
	}
	.work {
		flex: 1 1 auto;
		display: grid;
		grid-template-columns: minmax(0, 1fr) 22rem;
		grid-template-rows: minmax(0, 1fr);
		gap: 1rem;
		min-height: 0;
	}
	.canvas {
		min-height: 22rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
		/* Connections that read in both themes. */
		--xy-edge-stroke-default: color-mix(in srgb, var(--ink-2) 75%, transparent);
		--xy-edge-stroke-width-default: 1.4;
	}
	.panel {
		display: grid;
		gap: 0.75rem;
		padding: 0.9rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
		max-height: 100%;
		overflow-y: auto;
		align-self: start;
	}
	.panel h2 {
		font: 650 1.0625rem/1.3 var(--font-display);
		overflow-wrap: anywhere;
	}
	.live {
		display: grid;
		gap: 0.4rem;
		padding-top: 0.75rem;
		border-top: 1px solid var(--rule);
	}
	.live h3 {
		font-size: 0.9rem;
		font-weight: 700;
	}
	.live ul {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 0.35rem;
	}
	.live li {
		display: grid;
		overflow-wrap: anywhere;
	}
	.here {
		padding: 0.45rem 0.6rem;
		border-left: 3px solid var(--marker);
		background: var(--marker-soft);
		font-size: 0.85rem;
	}
	dl {
		display: grid;
		gap: 0.3rem;
		margin: 0;
	}
	dl div {
		display: flex;
		gap: 0.5rem;
	}
	dt {
		color: var(--ink-2);
		min-width: 4rem;
	}
	dd {
		margin: 0;
	}
	@media (max-width: 62rem) {
		.work {
			flex: none;
			grid-template-columns: minmax(0, 1fr);
			grid-template-rows: auto;
		}
		.canvas {
			height: min(32rem, calc(100dvh - 12rem));
		}
		.panel {
			max-height: none;
		}
	}
</style>
