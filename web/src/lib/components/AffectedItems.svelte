<script lang="ts">
	import { api } from '$lib/api';
	import { cmdb } from '$lib/cmdb.svelte';
	import CIPicker from '$lib/components/CIPicker.svelte';
	import { realtime } from '$lib/realtime.svelte';
	import type { CIImpact, CISummary, Ticket } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	/**
	 * The configuration items a record is about, and what trouble with them reaches. Those who may edit the record link
	 * and unlink items; the list comes back from the server after each change.
	 */
	let {
		ticket,
		canEdit,
		onchange
	}: {
		ticket: Ticket;
		canEdit: boolean;
		onchange: (cis: CISummary[]) => void;
	} = $props();

	const items = $derived(ticket.cis ?? []);
	let impact = $state<CIImpact | null>(null);
	let busy = $state(false);
	let error = $state('');

	async function loadImpact() {
		if (items.length === 0) {
			impact = null;
			return;
		}
		try {
			impact = await api<CIImpact>(`/api/v1/tickets/${ticket.id}/impact`);
		} catch {
			impact = null;
		}
	}

	// What the items reach changes when the items do.
	$effect(() => {
		void items.map((c) => c.id).join();
		void loadImpact();
	});
	$effect(() => {
		void cmdb.ensure();
		return realtime.on((e) => {
			if (e.type === 'ci.changed' && (items.some((c) => c.id === e.id) || impact?.items.some((a) => a.id === e.id))) void loadImpact();
		});
	});

	const describe = (c: CISummary) =>
		[c.class_label, c.status !== 'active' && cmdb.statusLabel(c.status), c.support_group_name && `Supported by ${c.support_group_name}`]
			.filter(Boolean)
			.join(' · ');

	async function change(method: 'PUT' | 'DELETE', ci: CISummary, done: string) {
		busy = true;
		error = '';
		try {
			const r = await api<{ items: CISummary[] }>(`/api/v1/tickets/${ticket.id}/cis/${ci.id}`, { method });
			onchange(r.items);
			ui.announce = done;
		} catch (e) {
			error = e instanceof Error ? e.message : 'That change didn’t save';
		} finally {
			busy = false;
		}
	}
</script>

{#if canEdit || items.length}
	<section class="affected" aria-labelledby="affected-h">
		<h2 id="affected-h">Affected items</h2>
		{#if error}<p class="error" role="alert">{error}</p>{/if}
		{#if items.length === 0}
			<p class="none">No configuration items are linked yet.</p>
		{:else}
			<ul class="rows">
				{#each items as c (c.id)}
					<li>
						<span class="what">
							<a href="/cmdb/{c.id}"><strong>{c.name}</strong></a>
							<span class="sub">{describe(c)}</span>
						</span>
						{#if canEdit}
							<button class="btn quiet-btn" disabled={busy} onclick={() => change('DELETE', c, `${c.name} unlinked`)} aria-label="Unlink {c.name}">
								Unlink
							</button>
						{/if}
					</li>
				{/each}
			</ul>
		{/if}

		{#if impact && impact.items.length}
			<h3>Also affected</h3>
			<ul class="rows" aria-label="Also affected">
				{#each impact.items as a (a.id)}
					<li>
						<span class="what">
							<a href="/cmdb/{a.id}"><strong>{a.name}</strong></a>
							<span class="sub">{a.class_label} · through {a.via_name}</span>
						</span>
					</li>
				{/each}
			</ul>
			{#if impact.truncated}<p class="none">There is more than this.</p>{/if}
		{/if}

		{#if canEdit}
			<div class="add">
				<CIPicker label="Link an item" action="Link" exclude={items.map((c) => c.id)} disabled={busy} onpick={(c) => change('PUT', c, `${c.name} linked`)} />
			</div>
		{/if}
	</section>
{/if}

<style>
	.affected {
		display: grid;
		gap: 0.6rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	h2 {
		font: 700 1.5rem/1 var(--font-display);
		letter-spacing: 0.01em;
	}
	h3 {
		margin-top: 0.4rem;
		font-size: 0.85rem;
		font-weight: 600;
		color: var(--ink-2);
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
		padding: 0.45rem 0;
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
	.none {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.add {
		max-width: 28rem;
	}
	.quiet-btn {
		min-height: 2rem;
		font-weight: 500;
	}
	.error {
		padding: 0.5rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
		color: var(--red);
		font-size: 0.9rem;
	}
</style>
