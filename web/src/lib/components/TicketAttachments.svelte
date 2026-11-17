<script lang="ts">
	import { api } from '$lib/api';
	import { openAttachment, preview, previews } from '$lib/attachments.svelte';
	import { age, bytes, when } from '$lib/format';
	import type { Attachment, Ticket } from '$lib/types';
	import { ui } from '$lib/ui.svelte';
	import { uploads } from '$lib/uploads.svelte';

	/**
	 * The files attached to a record: everyone who sees it downloads them; its requester and whoever may update it
	 * attach more, by choosing them or dropping them here, and remove what they may.
	 */
	let {
		ticket,
		onchange
	}: {
		ticket: Ticket;
		onchange: (attachments: Attachment[]) => void;
	} = $props();

	const items = $derived(ticket.attachments ?? []);
	const inFlight = $derived(uploads.for(ticket.id));
	const canAttach = $derived(!!ticket.can_attach);

	let over = $state(false);
	let error = $state('');
	let confirming = $state('');
	let busy = $state(false);
	$effect(() => items.forEach(preview));

	function add(files: FileList | File[] | null | undefined) {
		if (!files?.length || !canAttach) return;
		error = '';
		const list = [...files];
		uploads.start(ticket.id, list, (a) => {
			// A live refetch may already have brought it.
			const now = ticket.attachments ?? [];
			if (!now.some((x) => x.id === a.id)) onchange([...now, a]);
			ui.announce = `${a.filename} attached`;
		});
		ui.announce = list.length === 1 ? `Uploading ${list[0].name}` : `Uploading ${list.length} files`;
	}

	async function open(a: Attachment) {
		error = '';
		try {
			await openAttachment(a);
		} catch (e) {
			error = e instanceof Error ? e.message : 'The file could not be fetched';
		}
	}

	async function remove(a: Attachment) {
		busy = true;
		error = '';
		try {
			await api(`/api/v1/attachments/${a.id}`, { method: 'DELETE' });
			onchange(items.filter((x) => x.id !== a.id));
			ui.announce = `${a.filename} removed`;
			confirming = '';
		} catch (e) {
			error = e instanceof Error ? e.message : 'The file wasn’t removed';
		} finally {
			busy = false;
		}
	}

	function dragging(e: DragEvent) {
		if (!canAttach || !e.dataTransfer?.types.includes('Files')) return;
		e.preventDefault();
		e.dataTransfer.dropEffect = 'copy';
		over = true;
	}
</script>

{#if canAttach || items.length || inFlight.length}
	<section
		class="attachments"
		class:over
		aria-labelledby="attachments-h"
		ondragenter={dragging}
		ondragover={dragging}
		ondragleave={(e) => {
			if (!(e.currentTarget as HTMLElement).contains(e.relatedTarget as Node)) over = false;
		}}
		ondrop={(e) => {
			if (!canAttach) return;
			e.preventDefault();
			over = false;
			add(e.dataTransfer?.files);
		}}
	>
		<div class="head">
			<h2 id="attachments-h">Attachments</h2>
			{#if canAttach}
				<label class="btn" for="attach-files">Attach files</label>
				<input
					id="attach-files"
					class="sr-only"
					type="file"
					multiple
					onchange={(e) => {
						add(e.currentTarget.files);
						e.currentTarget.value = '';
					}}
				/>
			{/if}
		</div>
		{#if error}<p class="error" role="alert">{error}</p>{/if}

		{#if items.length === 0 && inFlight.length === 0}
			<p class="none">{canAttach ? 'No files yet. Drop files here to attach them.' : 'No files are attached.'}</p>
		{:else}
			<ul class="rows">
				{#each items as a (a.id)}
					<li>
						{#if a.inline}
							<span class="thumb">{#if previews[a.id]}<img src={previews[a.id]} alt="" loading="lazy" />{/if}</span>
						{/if}
						<span class="what">
							<button class="name" type="button" onclick={() => open(a)} title={a.inline ? 'Open in a new tab' : 'Download'}>{a.filename}</button>
							<span class="meta">
								{bytes(a.size)} · {a.created_by_name} ·
								<time datetime={a.attached_at ?? a.created_at} title={when(a.attached_at ?? a.created_at)}>{age(a.attached_at ?? a.created_at)}</time>
							</span>
						</span>
						{#if a.can_delete}
							{#if confirming === a.id}
								<span class="confirm" role="group" aria-label="Remove {a.filename}?">
									<button class="btn reject" type="button" disabled={busy} onclick={() => remove(a)}>Remove</button>
									<button class="btn" type="button" onclick={() => (confirming = '')}>Keep it</button>
								</span>
							{:else}
								<button class="btn quiet-btn" type="button" onclick={() => (confirming = a.id)} aria-label="Remove {a.filename}">Remove</button>
							{/if}
						{/if}
					</li>
				{/each}
				{#each inFlight as u (u.key)}
					<li class="uploading" class:failed={u.status === 'failed'}>
						<span class="what">
							<span class="name plain">{u.name}</span>
							{#if u.status === 'failed'}
								<span class="meta error" role="alert">{u.error}</span>
							{:else}
								<span class="progress">
									<progress max={u.size} value={u.sent} aria-label="Uploading {u.name}"></progress>
									<span class="meta">{bytes(u.sent)} of {bytes(u.size)}</span>
								</span>
							{/if}
						</span>
						<button class="btn quiet-btn" type="button" onclick={() => uploads.cancel(u.key)} aria-label="{u.status === 'failed' ? 'Dismiss' : 'Cancel'} {u.name}">
							{u.status === 'failed' ? 'Dismiss' : 'Cancel'}
						</button>
					</li>
				{/each}
			</ul>
		{/if}
	</section>
{/if}

<style>
	.attachments {
		display: grid;
		gap: 0.6rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
		border-radius: var(--radius);
	}
	.attachments.over {
		outline: 2px dashed var(--blue);
		outline-offset: 0.35rem;
		background: var(--blue-soft);
	}
	.head label:has(+ input:focus-visible) {
		outline: 2px solid var(--blue);
		outline-offset: 2px;
	}
	.head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
	}
	h2 {
		font: 650 1.0625rem/1.3 var(--font-display);
		letter-spacing: -0.015em;
	}
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.rows li {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		padding: 0.5rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.rows li:last-child {
		border-bottom: 0;
	}
	.thumb {
		flex: none;
		width: 2.75rem;
		height: 2.75rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		overflow: hidden;
	}
	.thumb img {
		width: 100%;
		height: 100%;
		object-fit: cover;
		display: block;
	}
	.what {
		flex: 1;
		min-width: 0;
		display: grid;
		gap: 0.15rem;
	}
	.name {
		justify-self: start;
		max-width: 100%;
		padding: 0;
		border: 0;
		background: none;
		color: var(--blue);
		font: inherit;
		font-weight: 600;
		text-align: left;
		cursor: pointer;
		overflow-wrap: anywhere;
	}
	.name:hover {
		text-decoration: underline;
	}
	.name.plain {
		color: var(--ink);
		cursor: default;
	}
	.meta {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.progress {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}
	progress {
		flex: 1;
		max-width: 16rem;
		height: 0.4rem;
		accent-color: var(--blue);
	}
	.btn.reject {
		color: var(--red);
	}
	.confirm {
		display: flex;
		gap: 0.4rem;
	}
	.quiet-btn {
		min-height: 2rem;
		font-weight: 500;
	}
	.none {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.error {
		color: var(--red);
		font-size: 0.9rem;
	}
	@media (max-width: 40rem) {
		.rows li {
			flex-wrap: wrap;
		}
		.confirm {
			width: 100%;
			justify-content: flex-end;
		}
	}
</style>
