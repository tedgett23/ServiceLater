<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import type { FormEntry } from '$lib/formEditor';

	let items = $state<FormEntry[] | null>(null);
	let error = $state('');

	onMount(async () => {
		try {
			items = (await api<{ items: FormEntry[] }>('/api/v1/forms')).items;
		} catch (e) {
			error = e instanceof Error ? e.message : 'The forms could not be loaded';
		}
	});
</script>

<svelte:head><title>Forms – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Forms</h1>
	<p class="lede">
		Each record type's form: the fields people fill in, when they show, and how the New ticket dialog and the record page
		are laid out. Changes made here sit on top of what the app ships and can be undone.
	</p>
	{#if error}<p class="error" role="alert">{error}</p>{/if}
	{#if items}
		<table>
			<thead><tr><th>Record type</th><th>Form from</th><th>Extra fields</th><th>Changed here</th></tr></thead>
			<tbody>
				{#each items as it (it.record_type)}
					<tr>
						<td>
							<span class="stub">{it.prefix}</span>
							{#if it.catalog}{it.label}{:else}<a href="/admin/forms/{it.record_type}">{it.label}</a>{/if}
						</td>
						<td class="muted">
							{#if it.catalog}Each catalog item's questions{:else if it.builtin && !it.edited}Built-in ({it.app || 'no app'} owns the type){:else}{it.app}{/if}
						</td>
						<td>{it.catalog ? '' : it.extra_fields}</td>
						<td>
							{#if it.catalog}<a href="/admin/catalog">Edit in Catalog</a>{:else if it.edited}<span class="pill">Changed</span>{/if}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 3rem;
		max-width: 60rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	table {
		width: 100%;
		border-collapse: collapse;
		background: var(--ground);
		border: 1px solid var(--rule);
		border-radius: var(--radius);
	}
	th {
		text-align: left;
		font-size: 0.8125rem;
		font-weight: 600;
		color: var(--ink-2);
		padding: 0.55rem 0.75rem;
		border-bottom: 1px solid var(--rule);
	}
	td {
		padding: 0.65rem 0.75rem;
		border-bottom: 1px solid var(--rule);
		vertical-align: top;
	}
	td a {
		font-weight: 600;
	}
	.stub {
		font: 600 0.6875rem var(--font-mono);
		background: var(--marker);
		color: var(--on-marker);
		padding: 0.05rem 0.3rem;
		border-radius: 4px;
		margin-right: 0.35rem;
	}
	.muted {
		color: var(--ink-2);
	}
	.pill {
		font-size: 0.75rem;
		padding: 0.1rem 0.5rem;
		border-radius: 99px;
		background: var(--marker-soft);
		color: var(--marker-ink);
		font-weight: 600;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
	@media (max-width: 40rem) {
		th:nth-child(3),
		td:nth-child(3) {
			display: none;
		}
	}
</style>
