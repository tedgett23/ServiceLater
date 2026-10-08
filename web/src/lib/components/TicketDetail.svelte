<script lang="ts">
	import { untrack } from 'svelte';
	import { api } from '$lib/api';
	import AffectedItems from '$lib/components/AffectedItems.svelte';
	import TicketArticles from '$lib/components/TicketArticles.svelte';
	import Signal from '$lib/components/Signal.svelte';
	import RecordFields from '$lib/components/RecordFields.svelte';
	import SlaClocks from '$lib/components/SlaClocks.svelte';
	import Status from '$lib/components/Status.svelte';
	import { display, patchFor } from '$lib/fields';
	import { priorityLabel, when } from '$lib/format';
	import { forms } from '$lib/forms.svelte';
	import { groups } from '$lib/groups.svelte';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import { queue } from '$lib/queue.svelte';
	import { realtime } from '$lib/realtime.svelte';
	import { session } from '$lib/session.svelte';
	import { staff } from '$lib/staff.svelte';
	import type { Comment, FieldValue, FormField, HistoryEntry, Status as StatusT, Ticket } from '$lib/types';
	import { long, what, who } from '$lib/history';
	import { can, canRecord, P, prefixOf } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let props: { ticket: Ticket; comments: Comment[] } = $props();

	// Local working copies: edits apply instantly, then reconcile with the server's answer.
	let ticket = $state(untrack(() => props.ticket));
	let comments = $state(untrack(() => props.comments));
	let error = $state('');
	let inflight = $state(0); // local edits awaiting the server; live refetches wait so they can't undo them, and actions pause
	let stale = false; // a live update arrived while an edit was in flight: refetch as soon as the edit settles

	const canEdit = $derived(canRecord(session.user, 'update', ticket.type));
	const canInternal = $derived(can(session.user, P.commentsInternal));
	const path = $derived(`/api/v1/tickets/${ticket.id}`);
	// The server says which moves to offer (the natural next step first) and which are blocked right now, and why.
	const moves = $derived(ticket.moves ?? []);

	// The type's form says which fields the record shows and in which sections; the ticket says their state now.
	$effect(() => void forms.ensure(ticket.type));
	const form = $derived(forms.of(ticket.type));
	const priorityField = $derived(form?.fields.find((f) => f.name === 'priority'));
	const priorityLocked = $derived(!!ticket.field_states?.priority?.read_only);

	function saveField(f: FormField, v: FieldValue | null) {
		const { body, optimistic } = patchFor(ticket, f, v);
		// Say what a choice became; text is too long to read back.
		const done =
			v === null || v === ''
				? `${f.label} cleared`
				: f.type === 'text' || f.type === 'textarea'
					? `${f.label} saved`
					: `${f.label} set to ${display(f, v)}`;
		void change(optimistic, () => api<Ticket>(path, { method: 'PATCH', body }), done);
	}

	$effect(() => {
		if (canEdit) {
			void staff.ensure(ticket.type);
			void groups.ensure();
		}
	});
	$effect(() => {
		const g = ticket.assignment_group_id;
		if (canEdit && g) void groups.ensureMembers(g);
	});

	// Who may be assigned: the group's members when the ticket has a group (the server refuses anyone else), else any
	// agent or admin. The current assignee stays listed so the picker never shows the wrong person.
	const groupMembers = $derived(ticket.assignment_group_id ? groups.members[ticket.assignment_group_id] : undefined);
	const assignable = $derived.by(() => {
		if (!groupMembers) return staff.of(ticket.type);
		const ids = new Set(groupMembers.map((m) => m.id));
		return staff.of(ticket.type).filter((s) => ids.has(s.id) || s.id === ticket.assignee_id);
	});
	const canTakeIt = $derived(
		ticket.assignee_id !== session.user?.id && (!groupMembers || groupMembers.some((m) => m.id === session.user?.id))
	);

	/** Apply `optimistic` now; keep the server's version on success, restore the old one on failure. */
	async function change(optimistic: Partial<Ticket>, request: () => Promise<Ticket>, done: string) {
		const before = ticket;
		error = '';
		inflight++;
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
		} finally {
			inflight--;
			// A change made by someone or something else (a rule, a workflow, another agent) can commit while our own edit
			// is still travelling. Its live hint was set aside, and our response predates it, so look again now.
			if (inflight === 0 && stale) {
				stale = false;
				void reloadTicket();
			}
		}
	}

	// Live updates: another person changed this ticket or commented. Refetch through the API.
	async function reloadTicket() {
		if (inflight > 0) {
			stale = true; // not dropped: change() looks again when the edit settles
			return;
		}
		try {
			const fresh = await api<Ticket>(path);
			if (inflight > 0) {
				stale = true; // an edit started meanwhile; this answer may predate it
				return;
			}
			if (fresh.updated_at !== ticket.updated_at) ui.announce = 'This ticket was updated';
			ticket = fresh;
			queue.upsert(fresh);
		} catch {
			/* keep showing what we have */
		}
	}

	async function reloadComments() {
		try {
			const fresh = (await api<{ items: Comment[] }>(`${path}/comments`)).items;
			const known = new Set(fresh.map((c) => c.id));
			// Keep comments still being sent; drop any the server already has, so nothing shows twice.
			comments = [...fresh, ...comments.filter((c) => c.pending && !known.has(c.id))];
		} catch {
			/* keep showing what we have */
		}
	}

	$effect(() =>
		realtime.on((e) => {
			if (e.type === 'resync') {
				void reloadTicket();
				void reloadComments();
			} else if (e.type === 'definitions.changed') {
				void reloadTicket(); // the flow may have changed: labels, buttons and guards
			} else if (e.type === 'ticket.changed' && e.id === ticket.id) {
				void reloadTicket();
			} else if (e.type === 'comment.added' && e.ticket_id === ticket.id) {
				void reloadComments();
			}
		})
	);

	const moveTo = (to: StatusT) =>
		change(
			{ status: to, state_category: lifecycle.categoryOf(to, ticket.type) ?? ticket.state_category },
			() => api<Ticket>(`${path}/transitions`, { method: 'POST', body: { to } }),
			`Ticket moved to ${lifecycle.label(to, ticket.type)}`
		);

	function assign(id: string | null) {
		const name = id ? (staff.of(ticket.type).find((s) => s.id === id)?.name ?? null) : null;
		return change(
			{ assignee_id: id, assignee_name: name },
			() => api<Ticket>(path, { method: 'PATCH', body: { assignee_id: id } }),
			id ? `Assigned to ${name}` : 'Unassigned'
		);
	}

	function route(id: string | null) {
		const name = id ? (groups.list.find((g) => g.id === id)?.name ?? null) : null;
		const members = id ? groups.members[id] : undefined;
		// Handing a ticket to another team lets go of an assignee who is not in it; the server does the same.
		const keep = !id || !ticket.assignee_id || !members || members.some((m) => m.id === ticket.assignee_id);
		return change(
			{ assignment_group_id: id, assignment_group_name: name, ...(keep ? {} : { assignee_id: null, assignee_name: null }) },
			() => api<Ticket>(path, { method: 'PATCH', body: { assignment_group_id: id } }),
			id ? `Routed to ${name}` : 'Removed from its group'
		);
	}

	const setPriority = (p: 1 | 2 | 3 | 4) =>
		change({ priority: p }, () => api<Ticket>(path, { method: 'PATCH', body: { priority: p } }), `Priority set to ${priorityLabel[p]}`);

	// What the Activity section shows: the conversation, the record's history (every change, by whom), or both in the
	// order they happened. The choice is remembered in this browser.
	type Show = 'comments' | 'history' | 'all';
	function remembered(): Show {
		try {
			const v = localStorage.getItem('activity.show');
			if (v === 'history' || v === 'all') return v;
		} catch {
			/* storage unavailable: comments */
		}
		return 'comments';
	}
	let show = $state<Show>(remembered());
	let history = $state<HistoryEntry[]>([]);
	async function loadHistory() {
		if (show === 'comments') return;
		try {
			history = (await api<{ items: HistoryEntry[] }>(`${path}/history`)).items;
		} catch {
			/* keep what is shown */
		}
	}
	$effect(() => {
		try {
			localStorage.setItem('activity.show', show);
		} catch {
			/* not remembered */
		}
	});
	// Whenever the ticket changes (an edit here, or a live update), its history has more in it.
	$effect(() => {
		void show;
		void ticket;
		void untrack(loadHistory);
	});
	type ActivityItem = { key: string; at: string; comment?: Comment; change?: HistoryEntry };
	const activity = $derived.by((): ActivityItem[] => {
		const cs: ActivityItem[] = show === 'history' ? [] : comments.map((c) => ({ key: `c${c.id}`, at: c.created_at, comment: c }));
		const hs: ActivityItem[] = show === 'comments' ? [] : history.map((e) => ({ key: `h${e.id}`, at: e.at, change: e }));
		return show === 'all' ? [...cs, ...hs].sort((a, b) => a.at.localeCompare(b.at)) : [...cs, ...hs];
	});

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
			// A live refetch may already have delivered this comment; never show it twice.
			comments = [...comments.filter((x) => x.id !== temp.id && x.id !== c.id), c].sort((a, b) => a.created_at.localeCompare(b.created_at));
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
		<div class="stub"><span class="sr-only">{lifecycle.typeLabel(ticket.type)} </span><span class="prefix" aria-hidden="true">{prefixOf(ticket)}</span>{ticket.number}</div>
		<div class="headtext">
			<h1>{ticket.title}</h1>
			<p class="byline">
				Opened by <strong>{ticket.requester_name}</strong> on <time datetime={ticket.created_at}>{when(ticket.created_at)}</time>
			</p>
		</div>
	</header>

	<div class="body">
		<div class="lead">
			{#if ticket.description}
				<p class="desc">{ticket.description}</p>
			{:else}
				<p class="desc none">No details were added.</p>
			{/if}

			<!-- The rest of the record's fields, as its form lays them out: staff edit them, everyone who can see it reads them. -->
			{#if form}
				<RecordFields {ticket} {form} {canEdit} save={saveField} />
			{/if}

			<!-- The configuration items it is about, for those who read the CMDB (the server sends cis only to them). -->
			{#if can(session.user, P.cmdbRead)}
				<AffectedItems {ticket} {canEdit} onchange={(cis) => (ticket = { ...ticket, cis })} />
			{/if}

			<!-- Knowledge articles that answer it: the requester reads the ones meant for them. -->
			<TicketArticles {ticket} {canEdit} onchange={(articles) => (ticket = { ...ticket, articles })} />
		</div>

		<section class="activity" aria-labelledby="activity-h">
				<div class="activity-head">
					<h2 id="activity-h">Activity</h2>
					<label class="show">
						<span>Show</span>
						<select bind:value={show}>
							<option value="comments">Comments</option>
							<option value="history">History</option>
							<option value="all">Everything</option>
						</select>
					</label>
				</div>

				{#if activity.length === 0}
					<p class="none">{show === 'history' ? 'Nothing has changed yet.' : 'No comments yet.'}</p>
				{/if}
				<ol>
					{#each activity as item (item.key)}
						{#if item.comment}
							{@const c = item.comment}
							<li class:internal={c.internal} class:pending={c.pending}>
								<div class="cmeta">
									<strong>{c.author_name}</strong>{#if c.on_behalf_of_name}{' '}<span class="behalf">for {c.on_behalf_of_name}</span>{/if}
									{#if c.internal}<span class="tag">Internal note</span>{/if}
									<time datetime={c.created_at}>{c.pending ? 'Sending…' : when(c.created_at)}</time>
								</div>
								<p>{c.body}</p>
							</li>
						{:else if item.change}
							{@const e = item.change}
							<li class="change">
								<span><strong>{who(e)}</strong> {what(e, `this ${lifecycle.typeLabel(ticket.type).toLowerCase()}`)}</span>
								<time datetime={e.at}>{when(e.at)}</time>
								{#if long(e)}
									<details>
										<summary>Show the change</summary>
										<div class="was"><span>Before</span><p>{e.old_text || '(empty)'}</p></div>
										<div><span>After</span><p>{e.new_text || '(empty)'}</p></div>
									</details>
								{/if}
							</li>
						{/if}
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
						{#if canInternal}
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
					<dd><Status status={ticket.status} category={ticket.state_category} type={ticket.type} /></dd>
				</div>

				{#if canEdit && moves.length}
					<div class="actions">
						{#each moves as move (move.to)}
							<button
								class="btn"
								class:primary={move.primary}
								class:blocked={!!move.blocked}
								disabled={!!move.blocked || inflight > 0}
								aria-describedby={move.blocked ? `why-${move.to}` : undefined}
								onclick={() => moveTo(move.to)}>{move.verb}</button
							>
							{#if move.blocked}<p class="why" id="why-{move.to}">{move.blocked}</p>{/if}
						{/each}
					</div>
				{/if}

				{#if ticket.slas?.length}
					<div>
						<dt>SLA</dt>
						<dd><SlaClocks clocks={ticket.slas} /></dd>
					</div>
				{/if}

				<div>
					<dt>{priorityField?.label ?? 'Priority'}</dt>
					<dd>
						{#if canEdit}
							<select
								class="select"
								value={ticket.priority}
								disabled={priorityLocked}
								onchange={(e) => setPriority(+e.currentTarget.value as 1 | 2 | 3 | 4)}
								aria-label={priorityField?.label ?? 'Priority'}
								aria-describedby={priorityLocked && priorityField?.help ? 'priority-help' : undefined}
							>
								{#each [1, 2, 3, 4] as const as p (p)}<option value={p}>{priorityLabel[p]}</option>{/each}
							</select>
							{#if priorityLocked && priorityField?.help}<p class="help" id="priority-help">{priorityField.help}</p>{/if}
						{:else}
							<span class="inline"><Signal priority={ticket.priority} /> {priorityLabel[ticket.priority]}</span>
						{/if}
					</dd>
				</div>

				{#if canEdit || ticket.assignment_group_name}
					<div>
						<dt>Assignment group</dt>
						<dd>
							{#if canEdit}
								<select class="select" value={ticket.assignment_group_id ?? ''} onchange={(e) => route(e.currentTarget.value || null)} aria-label="Assignment group">
									<option value="">No group</option>
									{#if ticket.assignment_group_id && !groups.list.some((g) => g.id === ticket.assignment_group_id)}
										<option value={ticket.assignment_group_id}>{ticket.assignment_group_name}</option>
									{/if}
									{#each groups.list as g (g.id)}<option value={g.id}>{g.name}</option>{/each}
								</select>
							{:else}
								{ticket.assignment_group_name}
							{/if}
						</dd>
					</div>
				{/if}

				<div>
					<dt>Assigned to</dt>
					<dd>
						{#if canEdit}
							<select class="select" value={ticket.assignee_id ?? ''} onchange={(e) => assign(e.currentTarget.value || null)} aria-label="Assignee">
								<option value="">Unassigned</option>
								{#each assignable as s (s.id)}<option value={s.id}>{s.name}</option>{/each}
							</select>
							{#if canTakeIt}
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
	/* The type's prefix is the small print above the number. */
	.stub .prefix {
		display: block;
		font-size: 0.2em;
		letter-spacing: 0.16em;
		margin-bottom: 0.15em;
	}
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
	.lead {
		grid-area: desc;
		display: grid;
		gap: 1.25rem;
		max-width: 68ch;
	}
	.activity-head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
	}
	.show {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		font-size: 0.85rem;
		color: var(--ink-2);
	}
	.show select {
		min-height: 2rem;
		padding: 0.2rem 0.4rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		color: var(--ink);
	}
	/* A change is a line, quieter than a comment. */
	.activity li.change {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		justify-content: space-between;
		gap: 0.15rem 0.75rem;
		padding: 0.4rem 0;
		border: 0;
		background: none;
		font-size: 0.875rem;
		color: var(--ink-2);
		overflow-wrap: anywhere;
	}
	.activity li.change strong {
		color: var(--ink);
		font-weight: 600;
	}
	.activity li.change time {
		font-size: 0.8rem;
	}
	.activity li.change details {
		flex-basis: 100%;
	}
	.activity li.change summary {
		cursor: pointer;
		font-size: 0.8rem;
	}
	.activity li.change details div {
		margin-top: 0.35rem;
		padding-left: 0.6rem;
		border-left: 3px solid var(--rule);
	}
	.activity li.change details .was {
		opacity: 0.75;
	}
	.activity li.change details span {
		font-size: 0.75rem;
		font-weight: 700;
	}
	.activity li.change details p {
		white-space: pre-wrap;
		color: var(--ink);
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
	.behalf {
		color: var(--ink-2);
		font-weight: 400;
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
	/* Why a move is unavailable, right under it, so nobody has to guess or try and fail. */
	.help {
		margin-top: 0.3rem;
		font-size: 0.8rem;
		color: var(--ink-2);
	}
	.why {
		margin: -0.15rem 0 0.35rem;
		padding-left: 0.6rem;
		border-left: 3px solid var(--marker);
		font-size: 0.8125rem;
		color: var(--ink-2);
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
