<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import CatalogIcon from '$lib/components/CatalogIcon.svelte';
	import FieldInput from '$lib/components/FieldInput.svelte';
	import PersonPicker from '$lib/components/PersonPicker.svelte';
	import { wide } from '$lib/fields';
	import { bytes } from '$lib/format';
	import { renderMarkdown } from '$lib/markdown';
	import { queue } from '$lib/queue.svelte';
	import { session } from '$lib/session.svelte';
	import type { CatalogItemDetail, FieldState, FieldValue, FormField, Ticket } from '$lib/types';
	import { ui } from '$lib/ui.svelte';
	import { uploads } from '$lib/uploads.svelte';

	/**
	 * Ordering one item: its description, its questions (shown, required and offered as the server says for the
	 * answers so far), who it is for, and files. Ordering files a request and starts the item's workflow.
	 */
	let item = $state<CatalogItemDetail | null>(null);
	let loadError = $state('');
	let values = $state<Record<string, FieldValue | null>>({});
	let states = $state<Record<string, FieldState>>({});
	let files = $state<File[]>([]);
	let forId = $state<string | null>(null);
	let forName = $state<string | null>(null);
	let busy = $state(false);
	let error = $state('');

	const fields = $derived(
		(item?.form.create ?? []).map((n) => item!.form.fields.find((f) => f.name === n)).filter((f): f is FormField => !!f)
	);

	onMount(async () => {
		try {
			item = await api<CatalogItemDetail>(`/api/v1/catalog/${page.params.name}`);
			values = Object.fromEntries(item.form.fields.filter((f) => f.default !== undefined).map((f) => [f.name, f.default!]));
			void askStates();
		} catch (e) {
			loadError = e instanceof ApiError && e.status === 404 ? 'That is not in the catalog, or not available to you.' : 'The item could not be loaded.';
		}
	});

	/** The order as filled in: answers to the item's questions, and the description when it asks for one. */
	function body(forSave: boolean) {
		const answers: Record<string, unknown> = {};
		let description = '';
		for (const f of fields) {
			const v = values[f.name];
			if (forSave && (states[f.name]?.hidden || states[f.name]?.read_only)) continue;
			if (v === null || v === undefined || v === '') continue;
			if (f.name === 'description') description = String(v);
			else if (!f.core) answers[f.name] = v;
		}
		return { fields: answers, description };
	}

	let timer: ReturnType<typeof setTimeout> | undefined;
	let asked = 0;
	async function askStates() {
		const n = ++asked;
		try {
			const res = await api<{ states: Record<string, FieldState> }>(`/api/v1/catalog/${page.params.name}/states`, { method: 'POST', body: body(false) });
			if (n === asked) states = res.states;
		} catch {
			/* keep the states we have: ordering checks again */
		}
	}

	function set(f: FormField, v: FieldValue | null) {
		values[f.name] = v;
		if (item?.form.reactive.includes(f.name)) {
			clearTimeout(timer);
			timer = setTimeout(askStates, 150);
		}
	}

	async function order(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			const t = await api<Ticket>(`/api/v1/catalog/${page.params.name}/request`, {
				method: 'POST',
				body: { ...body(true), requester_id: forId ?? undefined }
			});
			queue.prepend(t);
			if (files.length) uploads.start(t.id, files);
			ui.announce = `${t.reference} requested`;
			await goto(`/tickets/${t.reference}`);
		} catch (err) {
			error = err instanceof Error ? err.message : 'The request was not sent';
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>{item ? item.label : 'Request'} – ServiceLater</title></svelte:head>

<div class="page">
	<a class="back" href="/catalog">
		<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
		Catalog
	</a>

	{#if loadError}
		<p class="error" role="alert">{loadError}</p>
	{:else if item}
		<header>
			<span class="icon"><CatalogIcon name={item.icon} size={30} /></span>
			<div>
				<p class="cat">{item.category}</p>
				<h1>{item.label}</h1>
				{#if item.summary}<p class="summary">{item.summary}</p>{/if}
			</div>
		</header>
		{#if item.description}
			<!-- Rendered only through lib/markdown (sanitised), as knowledge articles are. -->
			<div class="prose">{@html renderMarkdown(item.description)}</div>
		{/if}

		<form onsubmit={order} aria-label="Request {item.label}">
			{#if item.can_order_for_others}
				<PersonPicker
					id="requested-for"
					label="Requested for"
					value={forId ?? session.user?.id ?? null}
					name={forName ?? `${session.user?.name ?? ''} (you)`}
					clearable={forId !== null}
					onchange={(id, name) => {
						forId = id && id !== session.user?.id ? id : null;
						forName = forId ? name : null;
					}}
				/>
			{/if}
			<div class="fields">
				{#each fields as f, i (f.name)}
					{@const st = states[f.name] ?? {}}
					{#if !st.hidden}
						<div class="field" class:wide={wide(f) || f.type === 'datetime'}>
							<label for="ci-{f.name}">{f.label}{#if st.required ?? f.required}<span class="req" aria-hidden="true"> *</span>{/if}</label>
							<FieldInput field={f} value={values[f.name] ?? null} fieldState={st} id="ci-{f.name}" disabled={!!st.read_only} live autofocus={i === 0} onchange={(v) => set(f, v)} />
						</div>
					{/if}
				{/each}
			</div>

			<div class="files">
				<label class="btn" for="ci-files">Attach files</label>
				<input
					id="ci-files"
					class="sr-only"
					type="file"
					multiple
					onchange={(e) => {
						files = [...files, ...(e.currentTarget.files ?? [])];
						e.currentTarget.value = '';
					}}
				/>
				{#if files.length}
					<ul aria-label="Files to attach">
						{#each files as f, i (i)}
							<li>
								<span>{f.name} <span class="size">{bytes(f.size)}</span></span>
								<button type="button" class="btn quiet" onclick={() => (files = files.filter((_, j) => j !== i))} aria-label="Don’t attach {f.name}">Remove</button>
							</li>
						{/each}
					</ul>
				{/if}
			</div>

			{#if error}<p class="error" role="alert">{error}</p>{/if}
			<div><button class="btn primary" disabled={busy}>{busy ? 'Sending…' : 'Send request'}</button></div>
		</form>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 3rem;
		max-width: 46rem;
	}
	.back {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		color: var(--ink-2);
		font-size: 0.9rem;
	}
	header {
		display: flex;
		gap: 1rem;
		align-items: flex-start;
		margin: 1rem 0;
	}
	.icon {
		flex: none;
		display: grid;
		place-items: center;
		width: 3.5rem;
		height: 3.5rem;
		border-radius: var(--radius);
		background: var(--blue-soft);
		color: var(--blue);
	}
	.cat,
	.summary,
	.size {
		color: var(--ink-2);
		font-size: 0.9rem;
	}
	h1 {
		font: 650 1.5rem/1.3 var(--font-display);
	}
	.prose {
		margin-bottom: 1.25rem;
	}
	form {
		display: grid;
		gap: 1.1rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	.fields {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(14rem, 1fr));
		gap: 1rem;
		align-items: start;
	}
	.field {
		display: grid;
		gap: 0.3rem;
	}
	.wide {
		grid-column: 1 / -1;
	}
	.req,
	.error {
		color: var(--red);
	}
	.error {
		font-weight: 600;
	}
	.files {
		display: grid;
		justify-items: start;
		gap: 0.5rem;
	}
	.files ul {
		justify-self: stretch;
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.files li {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
		padding: 0.25rem 0;
		overflow-wrap: anywhere;
	}
	.quiet {
		min-height: 2rem;
		font-weight: 500;
	}
	.files label:has(+ input:focus-visible) {
		outline: 2px solid var(--blue);
		outline-offset: 2px;
	}
</style>
