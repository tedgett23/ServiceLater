<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import { cmdb } from '$lib/cmdb.svelte';
	import CIPicker from '$lib/components/CIPicker.svelte';
	import FieldInput from '$lib/components/FieldInput.svelte';
	import Status from '$lib/components/Status.svelte';
	import { display } from '$lib/fields';
	import { groups } from '$lib/groups.svelte';
	import { realtime } from '$lib/realtime.svelte';
	import { long, what, who } from '$lib/history';
	import { can, P, type CIDetail, type CIImpact, type CIRelation, type CISummary, type FieldValue, type FormField, type HistoryEntry } from '$lib/types';
	import { when } from '$lib/format';
	import { ui } from '$lib/ui.svelte';

	let { data } = $props();
	const canManage = $derived(can(data.user, P.cmdbManage));

	let ci = $state<CIDetail | null>(null);
	let impact = $state<CIImpact | null>(null);
	let history = $state<HistoryEntry[]>([]);
	let loadError = $state('');
	let error = $state('');
	let busy = $state(false);
	let confirmingDelete = $state(false);
	const path = $derived(`/api/v1/cis/${page.params.id}`);
	const klass = $derived(ci ? cmdb.classOf(ci.class) : undefined);

	// The item's own fields as form fields, so a manager edits them with the same controls as the class's.
	const nameField: FormField = { name: 'name', type: 'text', label: 'Name', core: true, required: true, max_length: 200 };
	const descField: FormField = { name: 'description', type: 'textarea', label: 'Description', core: true, max_length: 10000 };
	const statusField = $derived<FormField>({
		name: 'status',
		type: 'choice',
		label: 'Status',
		core: true,
		required: true,
		options: cmdb.model?.statuses ?? []
	});

	async function load() {
		try {
			ci = await api<CIDetail>(path);
			loadError = '';
		} catch (e) {
			loadError = e instanceof ApiError && e.status === 404 ? 'That item does not exist.' : 'Could not load this item.';
			return;
		}
		try {
			impact = await api<CIImpact>(`${path}/impact`);
		} catch {
			impact = null;
		}
		try {
			history = (await api<{ items: HistoryEntry[] }>(`${path}/history`)).items;
		} catch {
			history = [];
		}
	}

	let timer: ReturnType<typeof setTimeout>;
	function later() {
		clearTimeout(timer);
		timer = setTimeout(() => void load(), 250);
	}

	onMount(() => {
		void cmdb.ensure();
		if (canManage) void groups.ensure();
		return realtime.on((e) => {
			// This item, or one at the end of a relationship, changed; or a record was linked to it or changed.
			if ((e.type === 'ci.changed' && (e.id === ci?.id || ci?.relationships.some((r) => r.item.id === e.id))) || e.type === 'resync')
				later();
			else if (e.type === 'ticket.changed') later();
		});
	});

	// Load again whenever the page moves to another item (following a relationship).
	$effect(() => {
		void path;
		ci = null;
		impact = null;
		confirmingDelete = false;
		void load();
	});

	/** Run a change, then show the item as it is now. */
	async function act(request: () => Promise<unknown>, done: string) {
		busy = true;
		error = '';
		try {
			await request();
			await load();
			ui.announce = done;
		} catch (e) {
			error = e instanceof Error ? e.message : 'That change didn’t save';
		} finally {
			busy = false;
		}
	}

	const patch = (body: Record<string, unknown>, done: string) => act(() => api(path, { method: 'PATCH', body }), done);

	function said(f: FormField, v: FieldValue | null) {
		if (v === null || v === '') return `${f.label} cleared`;
		return f.type === 'text' || f.type === 'textarea' ? `${f.label} saved` : `${f.label} set to ${display(f, v)}`;
	}

	// Adding a relationship: which way round, then the other item.
	let how = $state('');
	const hows = $derived(
		(cmdb.model?.relationships ?? []).flatMap((r) => [
			{ value: `${r.name}:to`, label: r.label },
			{ value: `${r.name}:from`, label: r.reverse }
		])
	);
	const howLabel = $derived(hows.find((h) => h.value === how)?.label ?? '');

	function relate(other: CISummary) {
		const [type, dir] = how.split(':');
		void act(
			() => api(`${path}/relationships`, { method: 'POST', body: { type, [dir]: other.id } }),
			`${howLabel} ${other.name} added`
		);
	}

	const unrelate = (r: CIRelation) =>
		act(() => api(`${path}/relationships/${r.id}`, { method: 'DELETE' }), `${r.label} ${r.item.name} removed`);

	// Relationships under one heading per way they read from this item.
	const grouped = $derived.by(() => {
		const out: { label: string; items: CIRelation[] }[] = [];
		for (const r of ci?.relationships ?? []) {
			const g = out.find((x) => x.label === r.label);
			if (g) g.items.push(r);
			else out.push({ label: r.label, items: [r] });
		}
		return out;
	});

	async function destroy() {
		busy = true;
		error = '';
		try {
			await api(path, { method: 'DELETE' });
			ui.announce = `${ci?.name} removed`;
			await goto('/cmdb');
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not remove the item';
			confirmingDelete = false;
		} finally {
			busy = false;
		}
	}

	const shownFields = $derived((klass?.fields ?? []).filter((f) => canManage || display(f, ci?.fields[f.name] ?? null) !== ''));
</script>

<svelte:head><title>{ci ? ci.name : 'Item'} – ServiceLater</title></svelte:head>

<div class="page">
	<a class="back" href="/cmdb">
		<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
		Configuration items
	</a>

	{#if loadError}
		<p class="msg" role="alert">{loadError}</p>
	{:else if ci}
		<h1>{ci.name}</h1>
		<p class="lede">
			{ci.class_label}{#if ci.status !== 'active'}<span class="status">{cmdb.statusLabel(ci.status)}</span>{/if}
		</p>

		{#if error}<p class="error" role="alert">{error}</p>{/if}

		<section aria-labelledby="details-h">
			<h2 id="details-h">Details</h2>
			{#if canManage}
				<div class="grid">
					<div class="field">
						<label for="ci-name">Name</label>
						<FieldInput field={nameField} value={ci.name} id="ci-name" disabled={busy} onchange={(v) => v && patch({ name: v }, 'Name saved')} />
					</div>
					<div class="field">
						<label for="ci-status">Status</label>
						<FieldInput
							field={statusField}
							value={ci.status}
							id="ci-status"
							disabled={busy}
							onchange={(v) => v && patch({ status: v }, said(statusField, v))}
						/>
					</div>
					<div class="field">
						<label for="ci-support">Supported by</label>
						<select
							id="ci-support"
							value={ci.support_group_id ?? ''}
							disabled={busy}
							onchange={(e) => patch({ support_group_id: e.currentTarget.value || null }, 'Support group saved')}
						>
							<option value="">No group</option>
							{#each groups.list as g (g.id)}<option value={g.id}>{g.name}</option>{/each}
						</select>
					</div>
					<div class="field">
						<label for="ci-owner">Owned by</label>
						<select
							id="ci-owner"
							value={ci.owner_group_id ?? ''}
							disabled={busy}
							onchange={(e) => patch({ owner_group_id: e.currentTarget.value || null }, 'Owner saved')}
						>
							<option value="">No group</option>
							{#each groups.list as g (g.id)}<option value={g.id}>{g.name}</option>{/each}
						</select>
					</div>
					<div class="field wide">
						<label for="ci-description">Description</label>
						<FieldInput
							field={descField}
							value={ci.description}
							id="ci-description"
							disabled={busy}
							onchange={(v) => patch({ description: v ?? '' }, said(descField, v))}
						/>
					</div>
					{#each shownFields as f (f.name)}
						<div class="field" class:wide={f.type === 'textarea'}>
							<label for="ci-f-{f.name}">{f.label}{#if f.required}<span class="req" aria-hidden="true"> *</span>{/if}</label>
							<FieldInput
								field={f}
								value={ci.fields[f.name] ?? null}
								id="ci-f-{f.name}"
								disabled={busy}
								onchange={(v) => patch({ fields: { [f.name]: v === '' ? null : v } }, said(f, v))}
							/>
						</div>
					{/each}
				</div>
			{:else}
				<div class="grid">
					<div class="read"><h3>Supported by</h3><p>{ci.support_group_name ?? 'No group'}</p></div>
					{#if ci.owner_group_name}<div class="read"><h3>Owned by</h3><p>{ci.owner_group_name}</p></div>{/if}
					{#if ci.description}<div class="read wide"><h3>Description</h3><p>{ci.description}</p></div>{/if}
					{#each shownFields as f (f.name)}
						<div class="read" class:wide={f.type === 'textarea'}><h3>{f.label}</h3><p>{display(f, ci.fields[f.name] ?? null)}</p></div>
					{/each}
				</div>
			{/if}
		</section>

		<section aria-labelledby="rel-h">
			<h2 id="rel-h">Relationships</h2>
			{#if grouped.length === 0}
				<p class="msg">Not related to anything yet.</p>
			{/if}
			{#each grouped as g (g.label)}
				<h3 class="rel-label">{g.label}</h3>
				<ul class="rows">
					{#each g.items as r (r.id)}
						<li>
							<span class="what">
								<a href="/cmdb/{r.item.id}"><strong>{r.item.name}</strong></a>
								<span class="sub">{[r.item.class_label, r.item.status !== 'active' && cmdb.statusLabel(r.item.status)].filter(Boolean).join(' · ')}</span>
							</span>
							{#if canManage}
								<button class="btn quiet-btn" disabled={busy} onclick={() => unrelate(r)} aria-label="Remove {r.label} {r.item.name}">Remove</button>
							{/if}
						</li>
					{/each}
				</ul>
			{/each}
			{#if canManage}
				<div class="add">
					<div class="field">
						<label for="rel-how">Add a relationship: {ci.name}…</label>
						<select id="rel-how" bind:value={how}>
							<option value="" disabled>How it relates</option>
							{#each hows as h (h.value)}<option value={h.value}>{h.label}</option>{/each}
						</select>
					</div>
					{#if how}
						<CIPicker label="{howLabel}…" action="Add" exclude={[ci.id]} disabled={busy} onpick={relate} />
					{/if}
				</div>
			{/if}
		</section>

		<section aria-labelledby="impact-h">
			<h2 id="impact-h">If it fails</h2>
			{#if !impact || impact.items.length === 0}
				<p class="msg">Nothing depends on it, as far as the CMDB knows.</p>
			{:else}
				<p class="hint">What depends on it, directly or further along, nearest first.</p>
				<ul class="rows">
					{#each impact.items as a (a.id)}
						<li>
							<span class="what">
								<a href="/cmdb/{a.id}"><strong>{a.name}</strong></a>
								<span class="sub">{a.depth > 1 ? `${a.class_label} · through ${a.via_name}` : a.class_label}</span>
							</span>
						</li>
					{/each}
				</ul>
				{#if impact.truncated}<p class="hint">There is more: the list stops 10 relationships away or at 1000 items.</p>{/if}
			{/if}
		</section>

		<section aria-labelledby="tickets-h">
			<h2 id="tickets-h">Tickets</h2>
			{#if ci.tickets.length === 0}
				<p class="msg">No tickets are linked to it.</p>
			{:else}
				<ul class="rows">
					{#each ci.tickets as t (t.id)}
						<li>
							<span class="what">
								<a href="/tickets/{t.reference}"><strong>{t.reference}</strong> {t.title}</a>
							</span>
							<Status status={t.status} category={t.state_category} type={t.type} />
						</li>
					{/each}
				</ul>
			{/if}
		</section>

		<section aria-labelledby="history-h">
			<h2 id="history-h">History</h2>
			{#if history.length === 0}
				<p class="msg">Nothing is recorded yet.</p>
			{:else}
				<ol class="rows history">
					{#each [...history].reverse() as e (e.id)}
						<li>
							<span class="what"><span><strong>{who(e)}</strong> {what(e, 'this item')}</span></span>
							<time datetime={e.at}>{when(e.at)}</time>
							{#if long(e)}
								<details>
									<summary>Show the change</summary>
									<p class="was">{e.old_text || '(empty)'}</p>
									<p>{e.new_text || '(empty)'}</p>
								</details>
							{/if}
						</li>
					{/each}
				</ol>
			{/if}
		</section>

		{#if canManage}
			<section class="danger" aria-labelledby="delete-h">
				<h2 id="delete-h">Remove</h2>
				<p class="hint">An item linked to tickets cannot be removed: set its status to Retired instead.</p>
				{#if confirmingDelete}
					<div class="row">
						<button class="btn reject" disabled={busy} onclick={destroy}>Remove {ci.name}</button>
						<button class="btn" onclick={() => (confirmingDelete = false)}>Keep it</button>
					</div>
				{:else}
					<div><button class="btn reject" onclick={() => (confirmingDelete = true)}>Remove item</button></div>
				{/if}
			</section>
		{/if}
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 56rem;
	}
	.back {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		margin-bottom: 0.75rem;
		font-weight: 600;
		text-decoration: none;
	}
	h1 {
		font: 800 1.9rem/1.15 var(--font-display);
		overflow-wrap: anywhere;
	}
	h2 {
		font: 700 1.35rem/1.2 var(--font-display);
		margin-bottom: 0.75rem;
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	.status {
		margin-left: 0.6rem;
		padding: 0.05rem 0.45rem;
		border-radius: var(--radius);
		background: var(--raised);
		color: var(--ink);
		font-weight: 600;
		font-size: 0.85rem;
	}
	section {
		padding: 1.25rem 0;
		border-top: 1px solid var(--rule);
	}
	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(15rem, 1fr));
		gap: 0.9rem 1rem;
		align-items: start;
	}
	.wide {
		grid-column: 1 / -1;
	}
	.read h3,
	.rel-label {
		font-size: 0.85rem;
		font-weight: 600;
		margin-bottom: 0.25rem;
	}
	.rel-label {
		margin-top: 0.75rem;
		color: var(--ink-2);
	}
	.read p {
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}
	.req {
		color: var(--red);
	}
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.rows li {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
		padding: 0.5rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.rows li:last-child {
		border-bottom: 0;
	}
	.what {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0 0.6rem;
		min-width: 0;
		overflow-wrap: anywhere;
	}
	.what a {
		color: var(--ink);
	}
	.sub,
	.hint,
	.msg {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.hint {
		margin-bottom: 0.5rem;
	}
	.add {
		display: grid;
		gap: 0.75rem;
		margin-top: 1rem;
		max-width: 28rem;
	}
	.row {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
	}
	.history li {
		flex-wrap: wrap;
		font-size: 0.875rem;
		color: var(--ink-2);
	}
	.history strong {
		color: var(--ink);
		font-weight: 600;
	}
	.history time {
		font-size: 0.8rem;
		white-space: nowrap;
	}
	.history details {
		flex-basis: 100%;
	}
	.history details p {
		margin-top: 0.3rem;
		padding-left: 0.6rem;
		border-left: 3px solid var(--rule);
		white-space: pre-wrap;
		color: var(--ink);
	}
	.history details .was {
		opacity: 0.75;
	}
	.danger {
		display: grid;
		gap: 0.5rem;
	}
	.quiet-btn {
		min-height: 2rem;
		font-weight: 500;
	}
	.btn.reject {
		color: var(--red);
	}
	.error {
		margin-bottom: 0.5rem;
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
		color: var(--red);
		font-weight: 600;
		font-size: 0.9rem;
	}
</style>
