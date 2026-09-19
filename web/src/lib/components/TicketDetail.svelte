<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import Signal from '$lib/components/Signal.svelte';
	import Status from '$lib/components/Status.svelte';
	import { nextStatuses, priorityLabel, statusLabel, transitionVerb, when } from '$lib/format';
	import { queue } from '$lib/queue.svelte';
	import { session } from '$lib/session.svelte';
	import { staff } from '$lib/staff.svelte';
	import type { Comment, Status as StatusT, Ticket } from '$lib/types';
	import { isStaff } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let props: { ticket: Ticket; comments: Comment[] } = $props();

	// Local working copies: edits apply instantly, then reconcile with the server's answer.
	let ticket = $state(untrack(() => props.ticket));
	let comments = $state(untrack(() => props.comments));
	let error = $state('');

	const canEdit = $derived(isStaff(session.user));
	const path = $derived(`/api/v1/tickets/${ticket.id}`);
	const forward: Partial<Record<StatusT, StatusT>> = { new: 'in_progress', in_progress: 'resolved', on_hold: 'in_progress', resolved: 'closed' };
	const primary = $derived(forward[ticket.status]);
	// The natural next step leads; the alternatives follow.
	const moves = $derived([...nextStatuses[ticket.status]].sort((a, b) => Number(b === primary) - Number(a === primary)));

	$effect(() => {
		if (canEdit) void staff.ensure();
	});

	/** Apply `optimistic` now; keep the server's version on success, restore the old one on failure. */
	async function change(optimistic: Partial<Ticket>, request: () => Promise<Ticket>, done: string) {
		const before = ticket;
		error = '';
		ticket = { ...ticket, ...optimistic };
		queue.patch(ticket.id, optimistic);
		try {
			ticket = await request();
			queue.upsert(ticket);
			ui.announce = done;
		} catch (e) {
			ticket = before;
			queue.upsert(before);
			error = e instanceof Error ? e.message : 'That change didn’t save';
		}
	}

	const moveTo = (to: StatusT) =>
		change({ status: to }, () => api<Ticket>(`${path}/transitions`, { method: 'POST', body: { to } }), `Ticket moved to ${statusLabel[to]}`);

	function assign(id: string | null) {
		const name = id ? (staff.list.find((s) => s.id === id)?.name ?? null) : null;
		return change(
			{ assignee_id: id, assignee_name: name },
			() => api<Ticket>(path, { method: 'PATCH', body: { assignee_id: id } }),
			id ? `Assigned to ${name}` : 'Unassigned'
		);
	}

	const setPriority = (p: 1 | 2 | 3 | 4) =>
		change({ priority: p }, () => api<Ticket>(path, { method: 'PATCH', body: { priority: p } }), `Priority set to ${priorityLabel[p]}`);

	// Comments
	let draft = $state('');
	let internal = $state(false);
	let posting = $state(false);

	async function post(e?: Event) {
		e?.preventDefault();
		const body = draft.trim();
		if (!body || posting) return;
		posting = true;
		error = '';
		const temp: Comment = {
			id: `pending-${Date.now()}`,
			ticket_id: ticket.id,
			author_id: session.user!.id,
			author_name: session.user!.name,
			body,
			internal,
			created_at: new Date().toISOString(),
			pending: true
		};
		comments = [...comments, temp];
		const saved = { draft, internal };
		draft = '';
		try {
			const c = await api<Comment>(`${path}/comments`, { method: 'POST', body: { body, internal: saved.internal } });
			comments = comments.map((x) => (x.id === temp.id ? c : x));
		} catch (err) {
			comments = comments.filter((x) => x.id !== temp.id);
			draft = saved.draft; // give the text back
			error = err instanceof Error ? err.message : 'Your comment didn’t send';
		} finally {
			posting = false;
		}
	}

	function onComposerKey(e: KeyboardEvent) {
		if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) void post();
	}
</script>

<article class="detail">
	<a class="back" href="/tickets">
		<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
		Queue
	</a>

	<header class="head">
		<div class="stub"><span class="sr-only">Ticket number </span>{ticket.number}</div>
		<div class="headtext">
			<h1>{ticket.title}</h1>
			<p class="byline">
				Opened by <strong>{ticket.requester_name}</strong> on <time datetime={ticket.created_at}>{when(ticket.created_at)}</time>
			</p>
		</div>
	</header>

	<div class="body">
		{#if ticket.description}
			<p class="desc">{ticket.description}</p>
		{:else}
			<p class="desc none">No details were added.</p>
		{/if}

		<section class="activity" aria-labelledby="activity-h">
				<h2 id="activity-h">Activity</h2>

				{#if comments.length === 0}
					<p class="none">No comments yet.</p>
				{/if}
				<ol>
					{#each comments as c (c.id)}
						<li class:internal={c.internal} class:pending={c.pending}>
							<div class="cmeta">
								<strong>{c.author_name}</strong>
								{#if c.internal}<span class="tag">Internal note</span>{/if}
								<time datetime={c.created_at}>{c.pending ? 'Sending…' : when(c.created_at)}</time>
							</div>
							<p>{c.body}</p>
						</li>
					{/each}
				</ol>

				<form class="composer" onsubmit={post}>
					<label class="field">
						<span class="sr-only">{internal ? 'Internal note' : 'Comment'}</span>
						<textarea
							bind:value={draft}
							onkeydown={onComposerKey}
							placeholder={internal ? 'Write a note only agents can see' : 'Write a comment'}
							class:internal
							maxlength="10000"
						></textarea>
					</label>
					<div class="composer-row">
						{#if canEdit}
							<label class="check"><input type="checkbox" bind:checked={internal} /> Internal note</label>
						{/if}
						<button class="btn primary" disabled={!draft.trim() || posting}>{internal ? 'Add internal note' : 'Add comment'}</button>
					</div>
				</form>
		</section>

		<aside class="meta" aria-label="Ticket details">
			{#if error}<p class="error" role="alert">{error}</p>{/if}

			<dl>
				<div>
					<dt>Status</dt>
					<dd><Status status={ticket.status} /></dd>
				</div>

				{#if canEdit && moves.length}
					<div class="actions">
						{#each moves as to (to)}
							<button class="btn" class:primary={to === primary} onclick={() => moveTo(to)}>{transitionVerb(ticket.status, to)}</button>
						{/each}
					</div>
				{/if}

				<div>
					<dt>Priority</dt>
					<dd>
						{#if canEdit}
							<select class="select" value={ticket.priority} onchange={(e) => setPriority(+e.currentTarget.value as 1 | 2 | 3 | 4)} aria-label="Priority">
								{#each [1, 2, 3, 4] as const as p (p)}<option value={p}>{priorityLabel[p]}</option>{/each}
							</select>
						{:else}
							<span class="inline"><Signal priority={ticket.priority} /> {priorityLabel[ticket.priority]}</span>
						{/if}
					</dd>
				</div>

				<div>
					<dt>Assigned to</dt>
					<dd>
						{#if canEdit}
							<select class="select" value={ticket.assignee_id ?? ''} onchange={(e) => assign(e.currentTarget.value || null)} aria-label="Assignee">
								<option value="">Unassigned</option>
								{#each staff.list as s (s.id)}<option value={s.id}>{s.name}</option>{/each}
							</select>
							{#if ticket.assignee_id !== session.user?.id}
								<button class="btn quiet-btn" onclick={() => assign(session.user!.id)}>Assign to me</button>
							{/if}
						{:else}
							{ticket.assignee_name ?? 'Not assigned yet'}
						{/if}
					</dd>
				</div>

				<div>
					<dt>Requested by</dt>
					<dd>{ticket.requester_name}</dd>
				</div>
				<div>
					<dt>Last updated</dt>
					<dd><time datetime={ticket.updated_at}>{when(ticket.updated_at)}</time></dd>
				</div>
			</dl>
		</aside>
	</div>
</article>

<style>
	.detail {
		container-type: inline-size;
		padding: 1.25rem 1.5rem 3rem;
		max-width: 68rem;
	}

	.back {
		display: none;
		align-items: center;
		gap: 0.4rem;
		margin-bottom: 1rem;
		font-weight: 600;
		text-decoration: none;
	}
	@media (max-width: 62rem) {
		.back {
			display: inline-flex;
		}
	}

	.head {
		display: flex;
		align-items: center;
		gap: 1.5rem;
		margin-bottom: 1.75rem;
	}

	/* The take-a-number stub: the one loud thing on the screen. */
	.stub {
		flex: none;
		position: relative;
		min-width: 6.5rem;
		text-align: center;
		padding: 0.3rem 1.6rem 0.4rem;
		background: var(--marker);
		color: var(--on-marker);
		font: 900 clamp(3.4rem, 8cqi, 5.6rem) / 1 var(--font-display);
		letter-spacing: 0.01em;
		--notch: 0.8rem;
		-webkit-mask:
			radial-gradient(circle var(--notch) at 0 50%, #0000 98%, #000) left / 51% 100% no-repeat,
			radial-gradient(circle var(--notch) at 100% 50%, #0000 98%, #000) right / 51% 100% no-repeat;
		mask:
			radial-gradient(circle var(--notch) at 0 50%, #0000 98%, #000) left / 51% 100% no-repeat,
			radial-gradient(circle var(--notch) at 100% 50%, #0000 98%, #000) right / 51% 100% no-repeat;
	}
	.headtext {
		min-width: 0;
	}
	h1 {
		font: 700 clamp(1.6rem, 3.6cqi, 2.4rem) / 1.1 var(--font-display);
		letter-spacing: 0.005em;
		overflow-wrap: anywhere;
	}
	.byline {
		margin-top: 0.4rem;
		color: var(--ink-2);
		font-size: 0.9rem;
	}
	.byline strong {
		color: var(--ink);
	}

	.body {
		display: grid;
		grid-template-columns: minmax(0, 1fr) 15rem;
		grid-template-areas:
			'desc meta'
			'activity meta';
		grid-template-rows: auto 1fr;
		column-gap: 2.5rem;
		align-items: start;
	}
	.desc {
		grid-area: desc;
	}
	.activity {
		grid-area: activity;
	}
	.meta {
		grid-area: meta;
	}
	/* Narrow: read the problem, then act on it, then the thread. */
	@container (max-width: 44rem) {
		.body {
			grid-template-columns: minmax(0, 1fr);
			grid-template-areas:
				'desc'
				'meta'
				'activity';
			grid-template-rows: auto;
			row-gap: 1.5rem;
		}
		.activity {
			margin-top: 0;
		}
		.head {
			flex-direction: column;
			align-items: flex-start;
			gap: 1rem;
		}
	}

	.desc {
		white-space: pre-wrap;
		max-width: 68ch;
		overflow-wrap: anywhere;
	}
	.none {
		color: var(--ink-2);
	}

	.activity {
		margin-top: 2.25rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
		max-width: 68ch;
	}
	h2 {
		font: 700 1.5rem/1 var(--font-display);
		margin-bottom: 1rem;
		letter-spacing: 0.01em;
	}
	ol {
		list-style: none;
		margin: 0 0 1.25rem;
		padding: 0;
		display: grid;
		gap: 1rem;
	}
	li {
		padding-left: 0.9rem;
		border-left: 3px solid var(--rule);
	}
	li.internal {
		border-left-color: var(--marker);
		background: var(--marker-soft);
		padding: 0.5rem 0.75rem 0.6rem 0.9rem;
		border-radius: 0 var(--radius) var(--radius) 0;
	}
	li.pending {
		opacity: 0.6;
	}
	.cmeta {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.6rem;
		font-size: 0.85rem;
		margin-bottom: 0.2rem;
	}
	.cmeta time {
		color: var(--ink-2);
	}
	.tag {
		font-weight: 600;
		padding: 0 0.35rem;
		background: var(--marker);
		color: var(--on-marker);
		border-radius: 2px;
	}
	li p {
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}

	.composer {
		display: grid;
		gap: 0.6rem;
	}
	.composer textarea.internal {
		background: var(--marker-soft);
		border-color: var(--marker);
	}
	.composer-row {
		display: flex;
		align-items: center;
		justify-content: flex-end;
		gap: 1rem;
	}
	.check {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		margin-right: auto;
		font-size: 0.9rem;
	}

	.meta dl {
		margin: 0;
		display: grid;
		gap: 1rem;
	}
	.meta dl > div {
		display: grid;
		gap: 0.25rem;
	}
	dt {
		font-size: 0.8125rem;
		color: var(--ink-2);
		font-weight: 600;
	}
	dd {
		margin: 0;
		display: grid;
		gap: 0.4rem;
		justify-items: start;
	}
	dd :global(.select) {
		min-height: 2.25rem;
	}
	dd select {
		width: 100%;
	}
	.inline {
		display: inline-flex;
		align-items: center;
		gap: 0.5rem;
	}
	.actions {
		display: grid;
		gap: 0.4rem;
	}
	.quiet-btn {
		min-height: 2rem;
		font-weight: 500;
	}
	.error {
		margin-bottom: 1rem;
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
		color: var(--red);
		font-weight: 600;
		font-size: 0.9rem;
	}
</style>
