<script lang="ts">
	import { onMount, untrack } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { cmdb } from '$lib/cmdb.svelte';
	import FieldInput from '$lib/components/FieldInput.svelte';
	import { groups } from '$lib/groups.svelte';
	import type { CIDetail, CIStatus, FieldValue } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let cls = $state('');
	let name = $state('');
	let status = $state<CIStatus>('active');
	let description = $state('');
	let owner = $state('');
	let support = $state('');
	let values = $state<Record<string, FieldValue | null>>({});
	let busy = $state(false);
	let error = $state('');

	const klass = $derived(cmdb.classOf(cls));

	onMount(() => {
		void cmdb.ensure();
		void groups.ensure();
	});

	// A new class starts from its defaults; values the person typed for fields both classes have are kept.
	$effect(() => {
		const k = klass;
		if (!k) return;
		const prev = untrack(() => values);
		const next: Record<string, FieldValue | null> = {};
		for (const f of k.fields) next[f.name] = prev[f.name] ?? f.default ?? null;
		values = next;
	});

	async function create(e: Event) {
		e.preventDefault();
		if (!klass || !name.trim() || busy) return;
		busy = true;
		error = '';
		const fields: Record<string, FieldValue> = {};
		for (const [k, v] of Object.entries(values)) if (v !== null && v !== '') fields[k] = v;
		try {
			const ci = await api<CIDetail>('/api/v1/cis', {
				method: 'POST',
				body: {
					class: cls,
					name: name.trim(),
					status,
					description,
					owner_group_id: owner || null,
					support_group_id: support || null,
					fields
				}
			});
			ui.announce = `${ci.name} added`;
			await goto(`/cmdb/${ci.id}`);
		} catch (err) {
			error = err instanceof Error ? err.message : 'Could not add the item';
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>New item – ServiceLater</title></svelte:head>

<div class="page">
	<a class="back" href="/cmdb">
		<svg viewBox="0 0 8 12" width="8" height="12" aria-hidden="true"><path d="M6 1 1.5 6 6 11" fill="none" stroke="currentColor" stroke-width="2" /></svg>
		Configuration items
	</a>
	<h1>New item</h1>

	<form onsubmit={create}>
		<div class="grid">
			<div class="field">
				<label for="ci-class">Class</label>
				<select id="ci-class" bind:value={cls} required>
					<option value="" disabled>Choose a class</option>
					{#each cmdb.tree() as { cls: c, depth } (c.name)}
						<option value={c.name}>{' '.repeat(depth)}{c.label}</option>
					{/each}
				</select>
				{#if klass?.description}<small class="help">{klass.description}</small>{/if}
			</div>
			<div class="field">
				<label for="ci-name">Name</label>
				<input id="ci-name" bind:value={name} maxlength="200" required />
			</div>
			<div class="field">
				<label for="ci-status">Status</label>
				<select id="ci-status" bind:value={status}>
					{#each cmdb.model?.statuses ?? [] as s (s.value)}<option value={s.value}>{s.label}</option>{/each}
				</select>
			</div>
			<div class="field">
				<label for="ci-support">Supported by</label>
				<select id="ci-support" bind:value={support}>
					<option value="">No group</option>
					{#each groups.list as g (g.id)}<option value={g.id}>{g.name}</option>{/each}
				</select>
			</div>
			<div class="field">
				<label for="ci-owner">Owned by</label>
				<select id="ci-owner" bind:value={owner}>
					<option value="">No group</option>
					{#each groups.list as g (g.id)}<option value={g.id}>{g.name}</option>{/each}
				</select>
			</div>
			<div class="field wide">
				<label for="ci-description">Description</label>
				<textarea id="ci-description" bind:value={description} maxlength="10000"></textarea>
			</div>
		</div>

		{#if klass && klass.fields.length}
			<section aria-labelledby="ci-fields-h">
				<h2 id="ci-fields-h">{klass.label}</h2>
				<div class="grid">
					{#each klass.fields as f (f.name)}
						<div class="field" class:wide={f.type === 'textarea'}>
							<label for="ci-f-{f.name}">{f.label}{#if f.required}<span class="req" aria-hidden="true"> *</span>{/if}</label>
							<FieldInput field={f} value={values[f.name] ?? null} id="ci-f-{f.name}" live onchange={(v) => (values[f.name] = v)} />
						</div>
					{/each}
				</div>
			</section>
		{/if}

		{#if error}<p class="error" role="alert">{error}</p>{/if}
		<div><button class="btn primary" disabled={!klass || !name.trim() || busy}>Add item</button></div>
	</form>
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
		font: 800 1.6rem/1.2 var(--font-display);
		margin-bottom: 1rem;
	}
	h2 {
		font: 700 1.25rem/1.2 var(--font-display);
		margin-bottom: 0.75rem;
	}
	form {
		display: grid;
		gap: 1.25rem;
	}
	section {
		padding-top: 1.25rem;
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
	.help {
		font-size: 0.8rem;
		color: var(--ink-2);
	}
	.req {
		color: var(--red);
	}
	.error {
		padding: 0.5rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
	}
</style>
